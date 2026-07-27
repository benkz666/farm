package farmrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
)

func TestHandlerExecutesEnterFarmForAuthorizedAssignedFarm(t *testing.T) {
	runtime := runtimeStub{actor: &actor.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	handler := NewHandler(runtime, []byte("internal-token"), func(uid uint64) bool { return uid == 42 }, func() int64 { return 123 })

	body, err := json.Marshal(CommandRequest{Operation: OperationEnterFarm, FarmUID: 42})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/cmd", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer internal-token")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response CommandResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Err != pkgerr.OK {
		t.Fatalf("response error = %d, want %d", response.Err, pkgerr.OK)
	}
	var payload EnterFarmResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode enter response: %v", err)
	}
	if payload.Snapshot.OwnerUID != 42 || payload.ServerTime != 123 {
		t.Fatalf("enter payload = %#v", payload)
	}
}

func TestHandlerRejectsRequestWithoutServiceToken(t *testing.T) {
	handler := NewHandler(runtimeStub{}, []byte("internal-token"), func(uint64) bool { return true }, nil)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/cmd", bytes.NewBufferString(`{"operation":"enter_farm","farm_uid":42}`))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestHTTPClientSendsTokenAndDecodesFarmResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer internal-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.URL.Path != "/internal/v1/cmd" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(CommandResponse{
			Err:     pkgerr.OK,
			Payload: json.RawMessage(`{"farm_seq":7}`),
		})
	}))
	t.Cleanup(server.Close)
	client := NewHTTPClient(map[string]string{"farm-0": server.URL}, "internal-token")

	response, err := client.Execute(t.Context(), "farm-0", CommandRequest{
		Operation: OperationEnterFarm,
		FarmUID:   42,
	})

	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if response.Err != pkgerr.OK || string(response.Payload) != `{"farm_seq":7}` {
		t.Fatalf("response = %#v", response)
	}
}

func TestHandlerPublishesDeltaAfterRoutedTill(t *testing.T) {
	runtime := runtimeStub{actor: &actor.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	publisher := &deltaPublisherStub{published: make(chan struct{}, 1)}
	handler := NewHandler(
		runtime,
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithDeltaPublisher(publisher),
	)
	body, err := json.Marshal(CommandRequest{
		Operation: OperationTill,
		FarmUID:   42,
		Payload:   json.RawMessage(`{"plot_index":0,"arg":0}`),
	})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/cmd", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer internal-token")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	select {
	case <-publisher.published:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published Delta")
	}
	deltas := publisher.Deltas()
	if len(deltas) != 1 {
		t.Fatalf("published Delta count = %d, want 1", len(deltas))
	}
	delta := deltas[0]
	if delta.OwnerUID != 42 || delta.FarmSeq != 1 || len(delta.Plots) != 1 {
		t.Fatalf("published Delta = %#v", delta)
	}
}

func TestHandlerAcceptsOriginatorConnectionID(t *testing.T) {
	runtime := runtimeStub{actor: &actor.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	handler := NewHandler(runtime, []byte("internal-token"), func(uid uint64) bool { return uid == 42 }, func() int64 { return 123 })
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/cmd", bytes.NewBufferString(
		`{"operation":"till","farm_uid":42,"originator_conn_id":99,"payload":{"plot_index":0,"arg":0}}`,
	))
	request.Header.Set("Authorization", "Bearer internal-token")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	var response CommandResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if recorder.Code != http.StatusOK || response.Err != pkgerr.OK {
		t.Fatalf("response = status:%d body:%#v", recorder.Code, response)
	}
}

func TestHandlerReturnsBeforeSlowDeltaFanout(t *testing.T) {
	runtime := runtimeStub{actor: &actor.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	publisher := &blockingDeltaPublisher{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	handler := NewHandler(
		runtime,
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithDeltaPublisher(publisher),
	)
	body, err := json.Marshal(CommandRequest{
		Operation: OperationTill,
		FarmUID:   42,
		Payload:   json.RawMessage(`{"plot_index":0,"arg":0}`),
	})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/cmd", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer internal-token")
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		responses <- recorder
	}()

	select {
	case recorder := <-responses:
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
	case <-time.After(100 * time.Millisecond):
		close(publisher.release)
		t.Fatal("Till response blocked on Delta fan-out")
	}
	select {
	case <-publisher.started:
	case <-time.After(time.Second):
		close(publisher.release)
		t.Fatal("Delta fan-out did not start")
	}
	close(publisher.release)
}

type runtimeStub struct {
	actor *actor.FarmActor
}

type deltaPublisherStub struct {
	mu        sync.Mutex
	deltas    []farm.FarmDelta
	published chan struct{}
}

func (p *deltaPublisherStub) Publish(_ context.Context, delta farm.FarmDelta, _ uint64) error {
	p.mu.Lock()
	p.deltas = append(p.deltas, delta)
	p.mu.Unlock()
	if p.published != nil {
		p.published <- struct{}{}
	}
	return nil
}

func (p *deltaPublisherStub) Deltas() []farm.FarmDelta {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]farm.FarmDelta(nil), p.deltas...)
}

type blockingDeltaPublisher struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingDeltaPublisher) Publish(_ context.Context, _ farm.FarmDelta, _ uint64) error {
	close(p.started)
	<-p.release
	return nil
}

func (s runtimeStub) Do(_ uint64, fn func(*actor.FarmActor) error) error {
	return fn(s.actor)
}
