package farmrpc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	"farm/server/gateway/presence"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"
	"farm/server/shared/store"

	"google.golang.org/grpc"
)

func TestHandlerExecutesEnterFarmForAuthorizedAssignedFarm(t *testing.T) {
	runtime := runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	handler := NewHandler(
		runtime,
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithTimeProfile(gameconfig.TimeProfileAuthentic),
	)

	response := handler.Execute(CommandRequest{Operation: OperationEnterFarm, FarmUID: 42})
	if response.Err != errcode.OK {
		t.Fatalf("response error = %d, want %d", response.Err, errcode.OK)
	}
	var payload EnterFarmResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode enter response: %v", err)
	}
	if payload.Snapshot.OwnerUID != 42 || payload.ServerTime != 123 || payload.TimeProfile != gameconfig.TimeProfileAuthentic {
		t.Fatalf("enter payload = %#v", payload)
	}
}

func TestHandlerEnterFarmBuildsOnlyRequestedPreparedRepresentation(t *testing.T) {
	runtime := runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	handler := NewHandler(runtime, nil, func(uint64) bool { return true }, func() int64 { return 123 })

	response := handler.Execute(CommandRequest{
		Operation: OperationEnterFarm, FarmUID: 42, PreferPrepared: true,
	})
	if response.Err != errcode.OK {
		t.Fatalf("response error = %d", response.Err)
	}
	if len(response.Payload) != 0 {
		t.Fatalf("prepared response duplicated JSON payload: %s", response.Payload)
	}
	if len(response.PreparedPayload) == 0 || response.PreparedField != clientwire.PreparedEnterFarmResponse {
		t.Fatalf("prepared response metadata = field %d payload %d", response.PreparedField, len(response.PreparedPayload))
	}
}

func TestHandlerReadsHotSwitchedTimeProfile(t *testing.T) {
	profiles := gameconfig.NewTimeProfileSwitch(gameconfig.TimeProfileDemo)
	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		StageCount:     3,
		SeasonStartAt:  123,
		SeasonDuration: 10 * gameconfig.HourMs(gameconfig.TimeProfileDemo),
		MatureAt:       123 + 10*gameconfig.HourMs(gameconfig.TimeProfileDemo),
		LastSettleAt:   123,
		LastWaterAt:    123,
	}
	handler := NewHandler(
		runtimeStub{actor: &room.FarmActor{Aggregate: aggregate}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithTimeProfileSwitch(profiles),
	)
	if !profiles.Set(gameconfig.TimeProfileFast) {
		t.Fatal("failed to switch profile")
	}

	response := handler.Execute(CommandRequest{Operation: OperationEnterFarm, FarmUID: 42})
	if response.Err != errcode.OK {
		t.Fatalf("EnterFarm err = %d", response.Err)
	}
	var payload EnterFarmResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode EnterFarm: %v", err)
	}
	if payload.TimeProfile != gameconfig.TimeProfileFast {
		t.Fatalf("time profile = %q, want fast", payload.TimeProfile)
	}
	wantDuration := int64(10 * gameconfig.HourMs(gameconfig.TimeProfileFast))
	if got := payload.Snapshot.Plots[0].SeasonDuration; got != wantDuration {
		t.Fatalf("growing plot duration = %d, want hot-switched %d", got, wantDuration)
	}
	if got := payload.Snapshot.Plots[0].MatureAt; got != 123+wantDuration {
		t.Fatalf("growing plot mature_at = %d, want %d", got, 123+wantDuration)
	}
}

func TestMarshalSnapshotResponsePreservesUint64Strings(t *testing.T) {
	snapshot, err := json.Marshal(farm.FarmSnapshotJSON{
		OwnerUID: 9_007_199_254_740_993,
		Coin:     9_007_199_254_740_993,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := marshalSnapshotResponse(snapshot, 9_007_199_254_740_993, 123, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var response EnterFarmResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("decode response: %v; payload=%s", err, payload)
	}
	if response.Snapshot.OwnerUID != 9_007_199_254_740_993 ||
		response.Snapshot.Coin != 9_007_199_254_740_993 ||
		uint64(response.FarmSeq) != 9_007_199_254_740_993 {
		t.Fatalf("response lost uint64 precision: %#v", response)
	}
}

func TestHandlerRejectsUnownedFarm(t *testing.T) {
	handler := NewHandler(runtimeStub{}, []byte("internal-token"), func(uint64) bool { return false }, nil)
	response := handler.Execute(CommandRequest{Operation: OperationEnterFarm, FarmUID: 42})
	if response.Err != errcode.BadRequest {
		t.Fatalf("response error = %d, want %d", response.Err, errcode.BadRequest)
	}
}

func TestGRPCClientSendsTokenAndDecodesFarmResponse(t *testing.T) {
	stub := &stubCommandServer{response: CommandResponse{
		Err:     errcode.OK,
		Payload: json.RawMessage(`{"farm_seq":7}`),
	}}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterFarmCommandServiceServer(server, stub)
	})
	client := NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"})

	response, err := client.Execute(t.Context(), "farm-0", CommandRequest{
		Operation: OperationEnterFarm,
		FarmUID:   42,
	})

	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if response.Err != errcode.OK || string(response.Payload) != `{"farm_seq":7}` {
		t.Fatalf("response = %#v", response)
	}
}

type stubCommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
	response CommandResponse
}

func (stub *stubCommandServer) Execute(context.Context, *farmv1.ExecuteRequest) (*farmv1.ExecuteResponse, error) {
	return &farmv1.ExecuteResponse{
		Err:         int32(stub.response.Err),
		PayloadJson: stub.response.Payload,
	}, nil
}

func TestHandlerPublishesDeltaAfterRoutedTill(t *testing.T) {
	runtime := runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	publisher := &deltaPublisherStub{published: make(chan struct{}, 1)}
	handler := NewHandler(
		runtime,
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithDeltaPublisher(publisher),
	)
	response := handler.Execute(CommandRequest{
		Operation: OperationPlotAction,
		FarmUID:   42,
		Payload: marshalPayload(PlotActionRequest{
			PlotIndex: 0,
			Kind:      farm.Till,
		}),
	})
	if response.Err != errcode.OK {
		t.Fatalf("response error = %d", response.Err)
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
	runtime := runtimeStub{actor: &room.FarmActor{Aggregate: aggregate}}
	handler := NewHandler(runtime, []byte("internal-token"), func(uid uint64) bool { return uid == 42 }, func() int64 { return 123 })

	plant := handler.Execute(CommandRequest{
		Operation: OperationPlotAction,
		FarmUID:   42,
		Payload: marshalPayload(PlotActionRequest{
			Kind: farm.Plant, PlotIndex: 0, Arg: 1, Command: 210,
		}),
	})
	buy := handler.Execute(CommandRequest{
		Operation: OperationShop,
		FarmUID:   42,
		Payload: marshalPayload(ShopRequest{
			Buy: true, ItemID: uint32(farm.DogFoodShopItemID), Quantity: 4, Command: 302,
		}),
	})
	sync := handler.Execute(CommandRequest{
		Operation: OperationSyncFarm,
		FarmUID:   42,
		Payload:   marshalPayload(SyncFarmRequest{FromSeq: 0}),
	})
	activate := handler.Execute(CommandRequest{
		Operation: OperationPet,
		FarmUID:   42,
		Payload:   marshalPayload(PetRequest{Kind: PetActivate, DogType: farm.DogMutt}),
	})
	feed := handler.Execute(CommandRequest{
		Operation: OperationPet,
		FarmUID:   42,
		Payload:   marshalPayload(PetRequest{Kind: PetFeed, Grams: 4}),
	})

	for name, response := range map[string]CommandResponse{
		"plant": plant, "buy": buy, "sync": sync, "activate": activate, "feed": feed,
	} {
		if response.Err != errcode.OK {
			t.Fatalf("%s response = %#v", name, response)
		}
	}
	if aggregate.Plots[0].State != farm.StateGrowing || aggregate.Pet.ActiveDog != farm.DogMutt ||
		aggregate.Pet.BowlEmptyAt <= 123 {
		t.Fatalf("aggregate after routed commands = %#v", aggregate)
	}
}

func TestHandlerSyncFarmAdvancesMatureStateAndPublishesDelta(t *testing.T) {
	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		SeasonTotal:    1,
		SeasonStartAt:  10_000,
		SeasonDuration: 10_000,
		MatureAt:       20_000,
		LastSettleAt:   10_000,
		LastWaterAt:    19_999,
		WeedNextWin:    gameconfig.RiskWindowsPerSeason,
		PestNextWin:    gameconfig.RiskWindowsPerSeason,
	}
	publisher := &deltaPublisherStub{published: make(chan struct{}, 1)}
	handler := NewHandler(
		runtimeStub{actor: &room.FarmActor{Aggregate: aggregate}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 20_000 },
		WithDeltaPublisher(publisher),
	)

	response := handler.Execute(CommandRequest{
		Operation: OperationSyncFarm,
		FarmUID:   42,
		Payload:   marshalPayload(SyncFarmRequest{FromSeq: 0}),
	})
	if response.Err != errcode.OK {
		t.Fatalf("SyncFarm response = %#v", response)
	}
	var payload SyncFarmResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode SyncFarm response: %v", err)
	}
	if payload.FarmSeq != 1 || payload.ServerTime != 20_000 || len(payload.Deltas) != 1 {
		t.Fatalf("SyncFarm payload = %#v", payload)
	}
	if got := payload.Deltas[0].Plots[0]; got.State != farm.StateMature ||
		got.FinalYield == 0 || got.LastSettleAt != 20_000 {
		t.Fatalf("mature delta = %#v", got)
	}
	select {
	case <-publisher.published:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for mature Delta publish")
	}
	if deltas := publisher.Deltas(); len(deltas) != 1 || deltas[0].FarmSeq != 1 {
		t.Fatalf("published deltas = %#v", deltas)
	}
}

func TestHandlerTaskClaimCreditsDirectReward(t *testing.T) {
	aggregate := farm.NewAggregate(42, "alice")
	handler := NewHandler(
		runtimeStub{actor: &room.FarmActor{Aggregate: aggregate}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithTaskClaimer(taskClaimerStub{reward: store.TaskReward{Coin: 20}}),
	)

	response := handler.Execute(CommandRequest{
		Operation: OperationTaskClaim,
		FarmUID:   42,
		Payload:   marshalPayload(TaskClaimRequest{TaskID: 1}),
	})

	if response.Err != errcode.OK {
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

func TestHandlerTaskListReturnsResetAt(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.Local).UnixMilli()
	taskMail := &taskMailStub{
		tasks: []store.Task{{ID: 1, Title: "plant", Progress: 1, Target: 1, RewardCoin: 20}},
	}
	handler := NewHandler(
		runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return now },
		WithTaskMailStore(taskMail),
	)

	response := handler.Execute(CommandRequest{
		Operation: OperationTaskList,
		FarmUID:   42,
		Payload:   marshalPayload(struct{}{}),
	})
	if response.Err != errcode.OK {
		t.Fatalf("TaskList response = %#v", response)
	}
	var payload TaskListResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode TaskList: %v", err)
	}
	if len(payload.Tasks) != 1 || payload.Tasks[0].ID != 1 {
		t.Fatalf("TaskList tasks = %#v", payload.Tasks)
	}
	if payload.ResetAt != gameconfig.NextLocalDayResetMs(now) {
		t.Fatalf("TaskList reset_at = %d, want %d", payload.ResetAt, gameconfig.NextLocalDayResetMs(now))
	}
	if taskMail.lastListUID != 42 {
		t.Fatalf("ListTasks uid = %d, want 42", taskMail.lastListUID)
	}
}

func TestHandlerMailReadAndDeleteMutateScopedMail(t *testing.T) {
	taskMail := &taskMailStub{
		mails: map[uint64]store.Mail{
			1: {ID: 1, Title: "notice"},
			2: {ID: 2, Title: "reward", AttachmentCoin: 50},
		},
	}
	handler := NewHandler(
		runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
		WithTaskMailStore(taskMail),
	)

	read := handler.Execute(CommandRequest{
		Operation: OperationMailRead,
		FarmUID:   42,
		Payload:   marshalPayload(MailMutationRequest{All: true}),
	})
	if read.Err != errcode.OK {
		t.Fatalf("MailRead response = %#v", read)
	}
	var readPayload MailMutationResponse
	if err := json.Unmarshal(read.Payload, &readPayload); err != nil {
		t.Fatalf("decode MailRead: %v", err)
	}
	if readPayload.Affected != 2 || !taskMail.mails[1].Read || !taskMail.mails[2].Read {
		t.Fatalf("mails after read = %#v, affected = %d", taskMail.mails, readPayload.Affected)
	}

	delete := handler.Execute(CommandRequest{
		Operation: OperationMailDelete,
		FarmUID:   42,
		Payload:   marshalPayload(MailMutationRequest{All: true}),
	})
	if delete.Err != errcode.OK {
		t.Fatalf("MailDelete response = %#v", delete)
	}
	var deletePayload MailMutationResponse
	if err := json.Unmarshal(delete.Payload, &deletePayload); err != nil {
		t.Fatalf("decode MailDelete: %v", err)
	}
	if deletePayload.Affected != 1 || len(taskMail.mails) != 1 || taskMail.mails[2].ID != 2 {
		t.Fatalf("mails after delete = %#v, affected = %d", taskMail.mails, deletePayload.Affected)
	}
	if taskMail.lastMailUID != 42 {
		t.Fatalf("mail mutation uid = %d, want 42", taskMail.lastMailUID)
	}
}

func TestHandlerDailyLoginCreditsDirectReward(t *testing.T) {
	aggregate := farm.NewAggregate(42, "alice")
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.Local).UnixMilli()
	claimer := &recordingTaskClaimer{reward: store.TaskReward{Coin: 100}}
	handler := NewHandler(
		runtimeStub{actor: &room.FarmActor{Aggregate: aggregate}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return now },
		WithTaskClaimer(claimer),
	)

	response := handler.Execute(CommandRequest{
		Operation: OperationDailyLogin,
		FarmUID:   42,
		Payload:   marshalPayload(struct{}{}),
	})

	if response.Err != errcode.OK {
		t.Fatalf("DailyLogin response = %#v", response)
	}
	var reward store.TaskReward
	if err := json.Unmarshal(response.Payload, &reward); err != nil {
		t.Fatalf("decode DailyLogin reward: %v", err)
	}
	if reward.Coin != 100 || aggregate.Coin != 1100 {
		t.Fatalf("reward = %#v, aggregate coin = %d", reward, aggregate.Coin)
	}
	if claimer.taskID != store.TaskDailyLoginID || claimer.dayKey != gameconfig.LocalDayKey(now) {
		t.Fatalf("legacy daily login claimed task=%d day=%d, want task=%d day=%d",
			claimer.taskID, claimer.dayKey, store.TaskDailyLoginID, gameconfig.LocalDayKey(now))
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
		runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}},
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
	runtime := runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	handler := NewHandler(runtime, []byte("internal-token"), func(uid uint64) bool { return uid == 42 }, func() int64 { return 123 })
	response := handler.Execute(CommandRequest{
		Operation:  OperationPlotAction,
		FarmUID:    42,
		Originator: presence.ConnRef{ConnID: 99, GatewayID: "gateway-0"},
		Payload: marshalPayload(PlotActionRequest{
			PlotIndex: 0,
			Arg:       0,
			Kind:      farm.Till,
		}),
	})
	if response.Err != errcode.OK {
		t.Fatalf("response = %#v", response)
	}
}

func TestHandlerReturnsBeforeSlowDeltaFanout(t *testing.T) {
	runtime := runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
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
	command := CommandRequest{
		Operation: OperationPlotAction,
		FarmUID:   42,
		Payload: marshalPayload(PlotActionRequest{
			PlotIndex: 0,
			Kind:      farm.Till,
		}),
	}
	responses := make(chan CommandResponse, 1)
	go func() {
		responses <- handler.Execute(command)
	}()

	select {
	case response := <-responses:
		if response.Err != errcode.OK {
			t.Fatalf("response error = %d", response.Err)
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
	actor *room.FarmActor
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

func (p *deltaPublisherStub) Publish(_ context.Context, delta farm.FarmDelta, _ presence.ConnRef) error {
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

func (p *blockingDeltaPublisher) Publish(_ context.Context, _ farm.FarmDelta, _ presence.ConnRef) error {
	close(p.started)
	<-p.release
	return nil
}

func (s runtimeStub) Do(_ uint64, fn func(*room.FarmActor) error) error {
	return fn(s.actor)
}

type taskMailStub struct {
	tasks       []store.Task
	mails       map[uint64]store.Mail
	lastListUID uint64
	lastMailUID uint64
}

func (s *taskMailStub) ListTasks(_ context.Context, uid uint64, _ int64) ([]store.Task, error) {
	s.lastListUID = uid
	return append([]store.Task(nil), s.tasks...), nil
}

func (s *taskMailStub) AdvanceTask(context.Context, uint64, int64, uint32, uint32) (store.TaskAdvanceResult, error) {
	return store.TaskAdvanceResult{}, nil
}

func (s *taskMailStub) ClaimTask(context.Context, uint64, int64, uint32) (store.TaskReward, error) {
	return store.TaskReward{}, nil
}

func (s *taskMailStub) ListMails(_ context.Context, uid uint64) ([]store.Mail, error) {
	s.lastListUID = uid
	out := make([]store.Mail, 0, len(s.mails))
	for _, mail := range s.mails {
		out = append(out, mail)
	}
	return out, nil
}

func (s *taskMailStub) MarkMailsRead(_ context.Context, uid uint64, mailID uint64) (int64, error) {
	s.lastMailUID = uid
	var affected int64
	for id, mail := range s.mails {
		if mailID != 0 && id != mailID {
			continue
		}
		if !mail.Read {
			mail.Read = true
			s.mails[id] = mail
			affected++
		}
	}
	return affected, nil
}

func (s *taskMailStub) DeleteMails(_ context.Context, uid uint64, mailID uint64) (int64, error) {
	s.lastMailUID = uid
	var affected int64
	for id, mail := range s.mails {
		if mailID != 0 && id != mailID {
			continue
		}
		if mail.AttachmentCoin > 0 && !mail.Claimed {
			continue
		}
		delete(s.mails, id)
		affected++
	}
	return affected, nil
}

func (s *taskMailStub) ClaimMail(context.Context, uint64, uint64) (store.Mail, error) {
	return store.Mail{}, nil
}

func (s *taskMailStub) ClaimDailyLogin(context.Context, uint64, int64) (store.TaskReward, error) {
	return store.TaskReward{}, nil
}
