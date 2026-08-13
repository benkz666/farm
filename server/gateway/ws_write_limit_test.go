package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type staticWriteBacklogSource struct {
	backlog WriteBacklog
	err     error
}

func (source staticWriteBacklogSource) Snapshot(context.Context) (WriteBacklog, error) {
	return source.backlog, source.err
}

func TestWriteInFlightLimitBoundsAndReleasesSlots(t *testing.T) {
	gateway := New(nil, nil, nil, WithWriteInFlightLimit(1))
	if !gateway.acquireWriteSlot() {
		t.Fatal("first write slot was rejected")
	}
	if gateway.acquireWriteSlot() {
		t.Fatal("write limit admitted excess request")
	}
	gateway.releaseWriteSlot()
	if !gateway.acquireWriteSlot() {
		t.Fatal("released write slot was not reusable")
	}
}

func TestBoundedSlotAbsorbsMomentarySaturation(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	time.AfterFunc(time.Millisecond, func() { releaseBoundedSlot(slots) })
	if !acquireBoundedSlotWithin(slots, 100*time.Millisecond) {
		t.Fatal("briefly saturated slot rejected instead of using the admission wait")
	}
	releaseBoundedSlot(slots)
}

func TestWriteInFlightLimitCanBeDisabled(t *testing.T) {
	gateway := New(nil, nil, nil, WithWriteInFlightLimit(0))
	for range 10_000 {
		if !gateway.acquireWriteSlot() {
			t.Fatal("disabled write guard rejected request")
		}
	}
}

func TestDynamicWriteAdmissionTracksRealBacklog(t *testing.T) {
	config := DefaultDynamicWriteAdmissionConfig(512)
	config.AdmissionWait = 0
	admission, err := NewDynamicWriteAdmission(staticWriteBacklogSource{}, config)
	if err != nil {
		t.Fatalf("NewDynamicWriteAdmission: %v", err)
	}
	if got := admission.Limit(); got != 512 {
		t.Fatalf("initial limit = %d, want 512", got)
	}

	admission.applyBacklog(config.HighWatermark)
	if got := admission.Limit(); got >= 512 || got <= config.MinLimit {
		t.Fatalf("limit under rising backlog = %d, want gradual reduction", got)
	}

	admission.applyBacklog(config.HardWatermark)
	if got := admission.Limit(); got != config.MinLimit {
		t.Fatalf("limit at hard watermark = %d, want %d", got, config.MinLimit)
	}

	admission.applyBacklog(0)
	if got := admission.Limit(); got != config.MinLimit+config.RecoveryStep {
		t.Fatalf("first recovered limit = %d, want additive recovery %d", got, config.MinLimit+config.RecoveryStep)
	}
}

func TestDynamicWriteAdmissionSamplesConfiguredSource(t *testing.T) {
	config := DefaultDynamicWriteAdmissionConfig(512)
	config.AdmissionWait = 0
	admission, err := NewDynamicWriteAdmission(staticWriteBacklogSource{
		backlog: WriteBacklog{Lag: config.HardWatermark, Streams: 32},
	}, config)
	if err != nil {
		t.Fatalf("NewDynamicWriteAdmission: %v", err)
	}
	admission.sample(context.Background())
	if got := admission.Limit(); got != config.MinLimit {
		t.Fatalf("sampled hard-backlog limit = %d, want %d", got, config.MinLimit)
	}
	if got := admission.lag.Load(); got != config.HardWatermark {
		t.Fatalf("sampled lag = %d, want %d", got, config.HardWatermark)
	}
}

func TestDynamicWriteAdmissionNeverCancelsAdmittedRequests(t *testing.T) {
	config := DefaultDynamicWriteAdmissionConfig(16)
	config.MinLimit = 2
	config.RecoveryStep = 2
	config.AdmissionWait = 0
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
		t.Fatal("lowered limit admitted a new request while old requests were still in flight")
	}
	for range config.MaxLimit {
		admission.Release()
	}
	for range config.MinLimit {
		if !admission.Acquire() {
			t.Fatal("minimum limit did not reopen after old requests completed")
		}
	}
}

func TestRedisWriteBacklogSourceUsesCanonicalFarmStreams(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	defer client.Close()
	source, err := NewRedisWriteBacklogSource(client, "farm:write:", []string{"farm-1", "farm-0", "farm-1"}, 2)
	if err != nil {
		t.Fatalf("NewRedisWriteBacklogSource: %v", err)
	}
	want := []string{
		"farm:write:{farm-0-0}:events",
		"farm:write:{farm-0-1}:events",
		"farm:write:{farm-1-0}:events",
		"farm:write:{farm-1-1}:events",
	}
	if len(source.keys) != len(want) {
		t.Fatalf("stream key count = %d, want %d", len(source.keys), len(want))
	}
	for index := range want {
		if source.keys[index] != want[index] {
			t.Fatalf("stream key[%d] = %q, want %q", index, source.keys[index], want[index])
		}
	}
}
