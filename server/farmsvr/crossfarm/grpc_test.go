package crossfarm

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
	"farm/server/shared/outbox"
	"farm/server/shared/sharding"
	"farm/server/shared/store"

	"google.golang.org/grpc"
)

type outboxAckStub struct {
	mu        sync.Mutex
	published map[string]bool
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
	server := NewGRPCServer(owner, visitor, func(uint64) bool { return true }, nil, newOutboxAckStub())

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
	server := NewGRPCServer(nil, nil, func(uint64) bool { return true }, nil, stub)
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
