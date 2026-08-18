package farmrpc

import (
	"context"
	"sync"
	"testing"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/store"
)

type staticWriteBacklogSource struct {
	backlog store.WriteJournalBacklog
	err     error
}

func (source staticWriteBacklogSource) WriteBacklog(context.Context) (store.WriteJournalBacklog, error) {
	return source.backlog, source.err
}

func TestDynamicWriteAdmissionTracksFarmJournalBacklog(t *testing.T) {
	config := DefaultWriteAdmissionConfig(512)
	config.AdmissionWait = 0
	admission, err := NewDynamicWriteAdmission(staticWriteBacklogSource{}, config)
	if err != nil {
		t.Fatalf("NewDynamicWriteAdmission: %v", err)
	}
	if admission.Limit() != 512 {
		t.Fatalf("initial limit=%d", admission.Limit())
	}
	admission.applyBacklog(config.HardWatermark)
	if admission.Limit() != config.MinLimit {
		t.Fatalf("hard-backlog limit=%d, want %d", admission.Limit(), config.MinLimit)
	}
	admission.applyBacklog(0)
	if admission.Limit() != config.MinLimit+config.RecoveryStep {
		t.Fatalf("recovered limit=%d", admission.Limit())
	}
}

func TestDynamicWriteAdmissionDoesNotCancelAdmittedWrites(t *testing.T) {
	config := DefaultWriteAdmissionConfig(8)
	config.MinLimit, config.RecoveryStep, config.AdmissionWait = 2, 2, 0
	admission, err := NewDynamicWriteAdmission(staticWriteBacklogSource{}, config)
	if err != nil {
		t.Fatalf("NewDynamicWriteAdmission: %v", err)
	}
	for range config.MaxLimit {
		if !admission.Acquire() {
			t.Fatal("maximum limit rejected an admissible request")
		}
	}
	admission.applyBacklog(config.HardWatermark)
	if admission.Acquire() {
		t.Fatal("reduced limit admitted a new request while old writes were in flight")
	}
	for range config.MaxLimit {
		admission.Release()
	}
	for range config.MinLimit {
		if !admission.Acquire() {
			t.Fatal("minimum limit did not reopen")
		}
	}
}

type rejectingAdmission struct{}

func (rejectingAdmission) Acquire() bool { return false }
func (rejectingAdmission) Release()      {}

func TestClientHandlerAppliesAdmissionOnlyToDurableWrites(t *testing.T) {
	handler := NewClientHandler(&Handler{}, nil, nil, nil, rejectingAdmission{})
	write := &farmv1.ClientCommandRequest{Uid: 42, Envelope: &publicv3.WireEnvelope{
		Cmd: 212, ClientSeq: 1,
		Payload: &publicv3.WireEnvelope_CommandRequest{CommandRequest: &publicv3.CommandRequest{}},
	}}
	if response := handler.ExecuteClient(t.Context(), write); response.Envelope.GetErr() != int32(errcode.RateLimited) {
		t.Fatalf("write err=%d, want RateLimited", response.Envelope.GetErr())
	}

	read := &farmv1.ClientCommandRequest{Uid: 42, ActiveFarmUid: 42, Envelope: &publicv3.WireEnvelope{
		Cmd: 204, ClientSeq: 2,
		Payload: &publicv3.WireEnvelope_SyncFarmRequest{SyncFarmRequest: &publicv3.SyncFarmRequest{OwnerUid: 42}},
	}}
	if response := handler.ExecuteClient(t.Context(), read); response.Envelope.GetErr() == int32(errcode.RateLimited) {
		t.Fatal("read command passed through the write admission guard")
	}
}

func TestJournalAdmissionExcludesDirectDatabaseWrites(t *testing.T) {
	for _, command := range []uint32{602, 608, 610} {
		if isJournalProducingWriteCommand(command) {
			t.Fatalf("direct database command %d unexpectedly uses journal admission", command)
		}
	}
	for _, command := range []uint32{206, 212, 614} {
		if !isJournalProducingWriteCommand(command) {
			t.Fatalf("journal-producing command %d bypasses journal admission", command)
		}
	}
}

func TestDynamicWriteAdmissionWakesAllWaitingWriters(t *testing.T) {
	config := DefaultWriteAdmissionConfig(2)
	config.MinLimit = 1
	config.AdmissionWait = 200 * time.Millisecond
	admission, err := NewDynamicWriteAdmission(staticWriteBacklogSource{}, config)
	if err != nil {
		t.Fatalf("NewDynamicWriteAdmission: %v", err)
	}
	if !admission.Acquire() || !admission.Acquire() {
		t.Fatal("failed to fill admission capacity")
	}

	ready := make(chan struct{}, 2)
	results := make(chan bool, 2)
	var acquired sync.WaitGroup
	acquired.Add(2)
	for range 2 {
		go func() {
			ready <- struct{}{}
			ok := admission.Acquire()
			results <- ok
			acquired.Done()
		}()
	}
	<-ready
	<-ready
	// Give both goroutines a chance to subscribe to the current generation.
	time.Sleep(10 * time.Millisecond)
	admission.Release()
	admission.Release()

	done := make(chan struct{})
	go func() {
		acquired.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiting writers were not woken")
	}
	for range 2 {
		if !<-results {
			t.Fatal("writer timed out despite released capacity")
		}
	}
}
