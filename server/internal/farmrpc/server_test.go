package farmrpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

type runtimeStub struct {
	actor *actor.FarmActor
}

func (s runtimeStub) Do(_ uint64, fn func(*actor.FarmActor) error) error {
	return fn(s.actor)
}
