package crossfarm

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
	"farm/server/shared/outbox"
	"farm/server/shared/presence"
	"farm/server/shared/sharding"
	"farm/server/shared/store"

	"google.golang.org/grpc"
)

type outboxAckStub struct {
	mu         sync.Mutex
	published  map[string]bool
	batchCalls int
}

func newOutboxAckStub() *outboxAckStub {
	return &outboxAckStub{published: make(map[string]bool)}
}

func (s *outboxAckStub) InsertOutboxEvents(context.Context, []outbox.Event) error { return nil }
func (s *outboxAckStub) ClaimDueOutbox(context.Context, int, int64) ([]store.OutboxRow, error) {
	return nil, nil
}
func (s *outboxAckStub) MarkOutboxPublished(_ context.Context, eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.published[eventID] {
		return sql.ErrNoRows
	}
	s.published[eventID] = true
	return nil
}
func (s *outboxAckStub) MarkOutboxPublishedBatch(_ context.Context, eventIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchCalls++
	for _, eventID := range eventIDs {
		s.published[eventID] = true
	}
	return nil
}
func (s *outboxAckStub) MarkOutboxRetry(context.Context, string, int, int64) error { return nil }
func (s *outboxAckStub) MarkOutboxDeadLetter(context.Context, string, int) error   { return nil }
func (s *outboxAckStub) DeletePublishedOutboxBefore(context.Context, int64) (int64, error) {
	return 0, nil
}

func TestGRPCApplyAndDeliverDuplicateSettle(t *testing.T) {
	ownerAgg := growingAggregate(9)
	visitorAgg := farm.NewAggregate(7, "visitor")
	visitorAgg.Coin = 1_000
	runtime := ownerRuntime{actors: map[uint64]*room.FarmActor{
		9: {Aggregate: ownerAgg},
		7: {Aggregate: visitorAgg},
	}}
	owner := NewOwner(runtime, ownerFriends{allowed: true}, func() int64 { return 40_000 }, nil, nil)
	visitor := NewVisitorSettler(runtime, func() int64 { return 40_000 })
	server := NewGRPCServer(owner, visitor, func(uint64) bool { return true }, newOutboxAckStub())

	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatalf("parse routes: %v", err)
	}
	pair := grpcx.NewBufconnPair(t, "token", func(s *grpc.Server) {
		RegisterCrossFarmService(s, server)
	})
	client := NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"}, routes)

	action := CrossAction{ReqID: 11, Kind: Water, VisitorUID: 7, OwnerUID: 9, PlotIndex: 0}
	reserveCode, err := client.ReserveCrossVisitor(context.Background(), action, 20260808)
	if err != nil || reserveCode != errcode.OK {
		t.Fatalf("reserve = %d err=%v", reserveCode, err)
	}
	result, err := client.ApplyCrossAction(context.Background(), action)
	if err != nil || result.Code != errcode.OK {
		t.Fatalf("apply = %#v err=%v", result, err)
	}

	_, _, code, err := client.DeliverCrossResult(context.Background(), result)
	if err != nil || code != errcode.OK {
		t.Fatalf("first deliver = %d err=%v", code, err)
	}
	_, _, code, err = client.DeliverCrossResult(context.Background(), result)
	if err != nil || code != errcode.OK {
		t.Fatalf("duplicate deliver = %d err=%v", code, err)
	}
}

func TestGRPCAcknowledgeCrossResultIdempotent(t *testing.T) {
	stub := newOutboxAckStub()
	server := NewGRPCServer(nil, nil, func(uint64) bool { return true }, stub)
	pair := grpcx.NewBufconnPair(t, "token", func(s *grpc.Server) {
		RegisterCrossFarmService(s, server)
	})
	client := NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"}, shardingStub(t))

	eventID := outbox.CrossResultEventID(9, 7, 42)
	if err := stub.MarkOutboxPublished(context.Background(), eventID); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	if err := client.AcknowledgeCrossResult(context.Background(), 9, 7, 42); err != nil {
		t.Fatalf("ack already published: %v", err)
	}
	if err := client.AcknowledgeCrossResult(context.Background(), 9, 7, 99); err != nil {
		t.Fatalf("ack missing row: %v", err)
	}
}

func TestGRPCExecuteColocatedCrossAction(t *testing.T) {
	ownerAgg := growingAggregate(9)
	visitorAgg := farm.NewAggregate(7, "visitor")
	runtime := &atomicOwnerRuntime{ownerRuntime: ownerRuntime{actors: map[uint64]*room.FarmActor{
		9: {Aggregate: ownerAgg},
		7: {Aggregate: visitorAgg},
	}}}
	// The Gateway verification marker deliberately bypasses this rejecting
	// fallback checker; unmarked callers are still covered by owner tests.
	publishedOrigin := make(chan presence.ConnRef, 1)
	owner := NewOwner(runtime, ownerFriends{allowed: false}, func() int64 { return 40_000 },
		DeltaPublisherFunc(func(_ context.Context, _ farm.FarmDelta, origin presence.ConnRef) error {
			publishedOrigin <- origin
			return nil
		}), nil)
	visitor := NewVisitorSettler(runtime, func() int64 { return 40_000 })
	stub := newOutboxAckStub()
	server := NewGRPCServer(owner, visitor, func(uint64) bool { return true }, stub)
	pair := grpcx.NewBufconnPair(t, "token", func(s *grpc.Server) {
		RegisterCrossFarmService(s, server)
	})
	client := NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"}, shardingStub(t))
	action := CrossAction{
		ReqID: 101, Kind: Water, VisitorUID: 7, OwnerUID: 9,
		PlotIndex: 0, FriendshipVerified: true,
		Originator: presence.ConnRef{ConnID: 71, GatewayID: "gateway-0"},
	}
	if !client.CanExecuteCrossAction(action) {
		t.Fatal("colocated action did not select fast path")
	}
	execution, err := client.ExecuteCrossAction(context.Background(), action, 20260810)
	if err != nil {
		t.Fatalf("ExecuteCrossAction: %v", err)
	}
	if execution.Code != errcode.OK || !execution.OwnerCommitted || execution.Result.Code != errcode.OK {
		t.Fatalf("execution = %#v", execution)
	}
	if execution.AckRequired || runtime.pairCalls != 1 {
		t.Fatalf("atomic execution ack_required=%v pair_calls=%d", execution.AckRequired, runtime.pairCalls)
	}
	if execution.PlayerDelta == nil || execution.Reward.ExpGained == 0 {
		t.Fatalf("execution reward = %#v delta=%#v", execution.Reward, execution.PlayerDelta)
	}
	if execution.FarmDelta == nil || execution.FarmDelta.OwnerUID != 9 ||
		execution.FarmDelta.FarmSeq != 1 || len(execution.FarmDelta.Plots) != 1 {
		t.Fatalf("execution FarmDelta = %#v", execution.FarmDelta)
	}
	if origin := <-publishedOrigin; origin != action.Originator {
		t.Fatalf("published originator = %#v, want %#v", origin, action.Originator)
	}
	if len(visitorAgg.CrossPending) != 0 || ownerAgg.Plots[0].LastWaterAt != 40_000 {
		t.Fatalf("visitor pending=%#v owner plot=%#v", visitorAgg.CrossPending, ownerAgg.Plots[0])
	}
}

type atomicOwnerRuntime struct {
	ownerRuntime
	pairCalls int
}

func (runtime *atomicOwnerRuntime) DoPairDurable(
	firstUID, secondUID uint64,
	fn func(first, second *room.FarmActor) error,
) error {
	runtime.pairCalls++
	return fn(runtime.actors[firstUID], runtime.actors[secondUID])
}

func TestGRPCClientBatchesCrossResultAcks(t *testing.T) {
	stub := newOutboxAckStub()
	server := NewGRPCServer(nil, nil, func(uint64) bool { return true }, stub)
	pair := grpcx.NewBufconnPair(t, "token", func(s *grpc.Server) {
		RegisterCrossFarmService(s, server)
	})
	client := NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"}, shardingStub(t))
	if err := client.EnqueueCrossResultAck(9, 7, 201); err != nil {
		t.Fatalf("enqueue first ack: %v", err)
	}
	if err := client.EnqueueCrossResultAck(9, 8, 202); err != nil {
		t.Fatalf("enqueue second ack: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stub.mu.Lock()
		calls := stub.batchCalls
		published := len(stub.published)
		stub.mu.Unlock()
		if calls == 1 && published == 2 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	t.Fatalf("batch calls=%d published=%#v", stub.batchCalls, stub.published)
}

func TestCrossResultEventDeterministic(t *testing.T) {
	result := &farmv1.CrossResult{
		ReqId:      5,
		VisitorUid: 7,
		OwnerUid:   9,
		Code:       int32(errcode.OK),
	}
	first, err := outbox.NewCrossResultEvent(9, result)
	if err != nil {
		t.Fatalf("NewCrossResultEvent: %v", err)
	}
	second, err := outbox.NewCrossResultEvent(9, result)
	if err != nil {
		t.Fatalf("NewCrossResultEvent second: %v", err)
	}
	if first.EventID != second.EventID || string(first.Payload) != string(second.Payload) {
		t.Fatalf("events differ: %#v vs %#v", first, second)
	}
}

func shardingStub(t *testing.T) *sharding.RouteTable {
	t.Helper()
	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatalf("parse routes: %v", err)
	}
	return routes
}
