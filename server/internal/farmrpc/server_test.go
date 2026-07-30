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
	"farm/server/internal/connreg"
	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
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

func TestHandlerExecutesShardedPlotShopSyncAndPetCommands(t *testing.T) {
	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0].State = farm.StateTilled
	aggregate.Items[farm.SeedItem(1)] = 1
	aggregate.Pet.Owned = 1
	runtime := runtimeStub{actor: &actor.FarmActor{Aggregate: aggregate}}
	handler := NewHandler(runtime, []byte("internal-token"), func(uid uint64) bool { return uid == 42 }, func() int64 { return 123 })

	plant := handler.execute(CommandRequest{
		Operation: OperationPlotAction,
		FarmUID:   42,
		Payload: marshalPayload(PlotActionRequest{
			Kind: farm.Plant, PlotIndex: 0, Arg: 1, Command: 210,
		}),
	})
	buy := handler.execute(CommandRequest{
		Operation: OperationShop,
		FarmUID:   42,
		Payload: marshalPayload(ShopRequest{
			Buy: true, ItemID: uint32(farm.DogFoodShopItemID), Quantity: 4, Command: 302,
		}),
	})
	sync := handler.execute(CommandRequest{
		Operation: OperationSyncFarm,
		FarmUID:   42,
		Payload:   marshalPayload(SyncFarmRequest{FromSeq: 0}),
	})
	activate := handler.execute(CommandRequest{
		Operation: OperationPet,
		FarmUID:   42,
		Payload:   marshalPayload(PetRequest{Kind: PetActivate, DogType: farm.DogMutt}),
	})
	feed := handler.execute(CommandRequest{
		Operation: OperationPet,
		FarmUID:   42,
		Payload:   marshalPayload(PetRequest{Kind: PetFeed, Grams: 4}),
	})

	for name, response := range map[string]CommandResponse{
		"plant": plant, "buy": buy, "sync": sync, "activate": activate, "feed": feed,
	} {
		if response.Err != pkgerr.OK {
			t.Fatalf("%s response = %#v", name, response)
		}
	}
	if aggregate.Plots[0].State != farm.StateGrowing || aggregate.Pet.ActiveDog != farm.DogMutt ||
		aggregate.Pet.BowlEmptyAt <= 123 {
		t.Fatalf("aggregate after routed commands = %#v", aggregate)
	}
}

func TestHandlerTaskClaimCreditsDirectReward(t *testing.T) {
	aggregate := farm.NewAggregate(42, "alice")
	handler := NewHandler(
		runtimeStub{actor: &actor.FarmActor{Aggregate: aggregate}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithTaskClaimer(taskClaimerStub{reward: store.TaskReward{Coin: 20}}),
	)

	response := handler.execute(CommandRequest{
		Operation: OperationTaskClaim,
		FarmUID:   42,
		Payload:   marshalPayload(TaskClaimRequest{TaskID: 1}),
	})

	if response.Err != pkgerr.OK {
		t.Fatalf("TaskClaim response = %#v", response)
	}
	var reward store.TaskReward
	if err := json.Unmarshal(response.Payload, &reward); err != nil {
		t.Fatalf("decode TaskClaim reward: %v", err)
	}
	if reward.Coin != 20 || aggregate.Coin != 1020 {
		t.Fatalf("reward = %#v, aggregate coin = %d", reward, aggregate.Coin)
	}
}

func TestHandlerDailyLoginCreditsDirectReward(t *testing.T) {
	aggregate := farm.NewAggregate(42, "alice")
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.Local).UnixMilli()
	claimer := &recordingTaskClaimer{reward: store.TaskReward{Coin: 100}}
	handler := NewHandler(
		runtimeStub{actor: &actor.FarmActor{Aggregate: aggregate}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return now },
		WithTaskClaimer(claimer),
	)

	response := handler.execute(CommandRequest{
		Operation: OperationDailyLogin,
		FarmUID:   42,
		Payload:   marshalPayload(struct{}{}),
	})

	if response.Err != pkgerr.OK {
		t.Fatalf("DailyLogin response = %#v", response)
	}
	var reward store.TaskReward
	if err := json.Unmarshal(response.Payload, &reward); err != nil {
		t.Fatalf("decode DailyLogin reward: %v", err)
	}
	if reward.Coin != 100 || aggregate.Coin != 1100 {
		t.Fatalf("reward = %#v, aggregate coin = %d", reward, aggregate.Coin)
	}
	if claimer.taskID != store.TaskDailyLoginID || claimer.dayKey != gameconf.LocalDayKey(now) {
		t.Fatalf("legacy daily login claimed task=%d day=%d, want task=%d day=%d",
			claimer.taskID, claimer.dayKey, store.TaskDailyLoginID, gameconf.LocalDayKey(now))
	}
}

func TestHandlerPublishesTaskNotifyOnlyWhenProgressChanges(t *testing.T) {
	task := store.Task{
		ID: 1, Title: "完成一次播种", Progress: 1, Target: 1, RewardCoin: 20,
	}
	progress := &taskProgressStub{results: []store.TaskAdvanceResult{
		{Task: task, Changed: true, JustCompleted: true},
		{Task: task, Changed: false},
	}}
	publisher := &taskNotifyPublisherStub{published: make(chan store.Task, 2)}
	handler := NewHandler(
		runtimeStub{actor: &actor.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithTaskProgressWriter(progress),
		WithTaskNotifyPublisher(publisher),
	)

	if err := handler.advanceGameplayTask(42, farm.Plant); err != nil {
		t.Fatalf("first advanceGameplayTask: %v", err)
	}
	select {
	case got := <-publisher.published:
		if got != task {
			t.Fatalf("TaskNotify payload = %#v, want %#v", got, task)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for TaskNotify")
	}

	if err := handler.advanceGameplayTask(42, farm.Plant); err != nil {
		t.Fatalf("second advanceGameplayTask: %v", err)
	}
	select {
	case got := <-publisher.published:
		t.Fatalf("unexpected TaskNotify for unchanged task: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHandlerAcceptsCompositeOriginatorConnection(t *testing.T) {
	runtime := runtimeStub{actor: &actor.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	handler := NewHandler(runtime, []byte("internal-token"), func(uid uint64) bool { return uid == 42 }, func() int64 { return 123 })
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/cmd", bytes.NewBufferString(
		`{"operation":"till","farm_uid":42,"originator":{"conn_id":99,"gateway_id":"gateway-0"},"payload":{"plot_index":0,"arg":0}}`,
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

type taskClaimerStub struct {
	reward store.TaskReward
}

func (s taskClaimerStub) ClaimTask(context.Context, uint64, int64, uint32) (store.TaskReward, error) {
	return s.reward, nil
}

func (s taskClaimerStub) ClaimDailyLogin(context.Context, uint64, int64) (store.TaskReward, error) {
	return s.reward, nil
}

type recordingTaskClaimer struct {
	reward store.TaskReward
	taskID uint32
	dayKey int64
}

func (s *recordingTaskClaimer) ClaimTask(_ context.Context, _ uint64, dayKey int64, taskID uint32) (store.TaskReward, error) {
	s.taskID = taskID
	s.dayKey = dayKey
	return s.reward, nil
}

type taskProgressStub struct {
	results []store.TaskAdvanceResult
}

func (s *taskProgressStub) AdvanceTask(context.Context, uint64, int64, uint32, uint32) (store.TaskAdvanceResult, error) {
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

type taskNotifyPublisherStub struct {
	published chan store.Task
}

func (p *taskNotifyPublisherStub) PublishTaskNotify(_ context.Context, _ uint64, task store.Task) error {
	p.published <- task
	return nil
}

type deltaPublisherStub struct {
	mu        sync.Mutex
	deltas    []farm.FarmDelta
	published chan struct{}
}

func (p *deltaPublisherStub) Publish(_ context.Context, delta farm.FarmDelta, _ connreg.ConnRef) error {
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

func (p *blockingDeltaPublisher) Publish(_ context.Context, _ farm.FarmDelta, _ connreg.ConnRef) error {
	close(p.started)
	<-p.release
	return nil
}

func (s runtimeStub) Do(_ uint64, fn func(*actor.FarmActor) error) error {
	return fn(s.actor)
}
