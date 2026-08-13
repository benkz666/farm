package farmrpc

import (
	"context"
	"sync"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"
	"farm/server/shared/presence"
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
	payload := response.EnterFarmResponse
	if payload == nil || payload.Snapshot.GetOwnerUid() != 42 || payload.ServerTime != 123 || payload.TimeProfile != gameconfig.TimeProfileAuthentic {
		t.Fatalf("enter payload = %#v", payload)
	}
}

func TestHandlerEnterFarmBuildsTypedRepresentation(t *testing.T) {
	runtime := runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
	handler := NewHandler(runtime, nil, func(uint64) bool { return true }, func() int64 { return 123 })

	response := handler.Execute(CommandRequest{
		Operation: OperationEnterFarm, FarmUID: 42,
	})
	if response.Err != errcode.OK {
		t.Fatalf("response error = %d", response.Err)
	}
	if response.EnterFarmResponse == nil || response.EnterFarmResponse.Snapshot == nil || response.EnterFarmResponse.Snapshot.OwnerUid != 42 {
		t.Fatalf("typed enter response = %#v", response.EnterFarmResponse)
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
	payload := response.EnterFarmResponse
	if payload == nil {
		t.Fatal("missing typed EnterFarm response")
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

func TestHandlerRejectsUnownedFarm(t *testing.T) {
	handler := NewHandler(runtimeStub{}, []byte("internal-token"), func(uint64) bool { return false }, nil)
	response := handler.Execute(CommandRequest{Operation: OperationEnterFarm, FarmUID: 42})
	if response.Err != errcode.BadRequest {
		t.Fatalf("response error = %d, want %d", response.Err, errcode.BadRequest)
	}
}

func TestCommandServiceSendsTokenAndDecodesFarmResponse(t *testing.T) {
	stub := &stubCommandServer{}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterFarmCommandServiceServer(server, stub)
	})
	request := typedRequest(42, 204, 7)
	conn, err := pair.Pool.Conn(t.Context(), "bufconn")
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	response, err := farmv1.NewFarmCommandServiceClient(conn).Execute(t.Context(), request)

	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if response.Envelope.GetErr() != int32(errcode.OK) || response.Envelope.GetClientSeq() != 7 {
		t.Fatalf("response = %#v", response)
	}
}

type stubCommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
}

func (stub *stubCommandServer) Execute(_ context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	return typedResponse(request, errcode.OK), nil
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
		Operation:     OperationPlotAction,
		FarmUID:       42,
		ClientCommand: 206,
		ClientRequest: &publicv3.CommandRequest{PlotIndex: 0},
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
		Operation:     OperationPlotAction,
		FarmUID:       42,
		ClientCommand: 210,
		ClientRequest: &publicv3.CommandRequest{PlotIndex: 0, Arg: 1},
	})
	buy := handler.Execute(CommandRequest{
		Operation:     OperationShop,
		FarmUID:       42,
		ClientCommand: 302,
		ClientRequest: &publicv3.CommandRequest{ItemId: uint32(farm.DogFoodShopItemID), Quantity: 4},
	})
	sync := handler.Execute(CommandRequest{
		Operation:   OperationSyncFarm,
		FarmUID:     42,
		SyncRequest: &publicv3.SyncFarmRequest{FromSeq: 0},
	})
	activate := handler.Execute(CommandRequest{
		Operation:     OperationPet,
		FarmUID:       42,
		ClientCommand: 502,
		ClientRequest: &publicv3.CommandRequest{DogType: uint32(farm.DogMutt)},
	})
	feed := handler.Execute(CommandRequest{
		Operation:     OperationPet,
		FarmUID:       42,
		ClientCommand: 504,
		ClientRequest: &publicv3.CommandRequest{Grams: 4},
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
		Operation:   OperationSyncFarm,
		FarmUID:     42,
		SyncRequest: &publicv3.SyncFarmRequest{FromSeq: 0},
	})
	if response.Err != errcode.OK {
		t.Fatalf("SyncFarm response = %#v", response)
	}
	payload := response.SyncFarmResponse
	if payload == nil || payload.FarmSeq != 1 || payload.ServerTime != 20_000 || len(payload.Deltas) != 1 {
		t.Fatalf("SyncFarm payload = %#v", payload)
	}
	if got := payload.Deltas[0].Plots[0]; got.State != uint32(farm.StateMature) ||
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
		Operation:     OperationTaskClaim,
		FarmUID:       42,
		ClientCommand: 602,
		ClientRequest: &publicv3.CommandRequest{TaskId: 1},
	})

	if response.Err != errcode.OK {
		t.Fatalf("TaskClaim response = %#v", response)
	}
	reward := response.ClientResponse.GetTaskReward()
	if reward.GetCoin() != 20 || aggregate.Coin != 1020 {
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
		Operation:     OperationTaskList,
		FarmUID:       42,
		ClientCommand: 600,
		ClientRequest: &publicv3.CommandRequest{},
	})
	if response.Err != errcode.OK {
		t.Fatalf("TaskList response = %#v", response)
	}
	payload := response.ClientResponse
	if payload == nil || len(payload.Tasks) != 1 || payload.Tasks[0].Id != 1 {
		t.Fatalf("TaskList response = %#v", payload)
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
		Operation:     OperationMailRead,
		FarmUID:       42,
		ClientCommand: 606,
		ClientRequest: &publicv3.CommandRequest{All: true},
	})
	if read.Err != errcode.OK {
		t.Fatalf("MailRead response = %#v", read)
	}
	if read.ClientResponse.GetAffected() != 2 || !taskMail.mails[1].Read || !taskMail.mails[2].Read {
		t.Fatalf("mails after read = %#v, affected = %d", taskMail.mails, read.ClientResponse.GetAffected())
	}

	delete := handler.Execute(CommandRequest{
		Operation:     OperationMailDelete,
		FarmUID:       42,
		ClientCommand: 610,
		ClientRequest: &publicv3.CommandRequest{All: true},
	})
	if delete.Err != errcode.OK {
		t.Fatalf("MailDelete response = %#v", delete)
	}
	if delete.ClientResponse.GetAffected() != 1 || len(taskMail.mails) != 1 || taskMail.mails[2].ID != 2 {
		t.Fatalf("mails after delete = %#v, affected = %d", taskMail.mails, delete.ClientResponse.GetAffected())
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
		Operation:     OperationDailyLogin,
		FarmUID:       42,
		ClientCommand: 614,
		ClientRequest: &publicv3.CommandRequest{},
	})

	if response.Err != errcode.OK {
		t.Fatalf("DailyLogin response = %#v", response)
	}
	reward := response.ClientResponse.GetTaskReward()
	if reward.GetCoin() != 100 || aggregate.Coin != 1100 {
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
		Operation:     OperationPlotAction,
		FarmUID:       42,
		Originator:    presence.ConnRef{ConnID: 99, GatewayID: "gateway-0"},
		ClientCommand: 206,
		ClientRequest: &publicv3.CommandRequest{PlotIndex: 0},
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
		Operation:     OperationPlotAction,
		FarmUID:       42,
		ClientCommand: 206,
		ClientRequest: &publicv3.CommandRequest{PlotIndex: 0},
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
