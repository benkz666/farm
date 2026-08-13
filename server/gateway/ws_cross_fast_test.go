package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/crossfarm"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
)

type revisionFriendStore struct {
	*friendStoreStub
	revision atomic.Uint64
	calls    atomic.Int64
}

func (store *revisionFriendStore) AreFriends(ctx context.Context, a, b uint64) (bool, error) {
	store.calls.Add(1)
	return store.friendStoreStub.AreFriends(ctx, a, b)
}

func (store *revisionFriendStore) Revision() uint64 { return store.revision.Load() }

func TestCrossTargetReusesEnterFarmFriendLeaseUntilInvalidated(t *testing.T) {
	inner := newFriendStoreStub()
	inner.add(7, 42)
	friends := &revisionFriendStore{friendStoreStub: inner}
	gateway := &Gateway{friends: friends}
	connection := &wsConnection{uid: 7, roomUID: 42}

	allowed, err := gateway.refreshFriendLease(connection, 42)
	if err != nil || !allowed {
		t.Fatalf("refresh lease allowed=%v err=%v", allowed, err)
	}
	if _, code := gateway.resolveCrossTarget(connection, 42); code != errcode.OK {
		t.Fatalf("leased resolve code=%d", code)
	}
	if got := friends.calls.Load(); got != 1 {
		t.Fatalf("AreFriends calls=%d, want 1 while lease is current", got)
	}

	friends.revision.Add(1)
	if _, code := gateway.resolveCrossTarget(connection, 42); code != errcode.OK {
		t.Fatalf("resolve after invalidation code=%d", code)
	}
	if got := friends.calls.Load(); got != 2 {
		t.Fatalf("AreFriends calls=%d, want revalidation after revision change", got)
	}
}

type colocatedCrossClientStub struct {
	reserveCalls atomic.Int32
	started      chan struct{}
	release      chan struct{}
	actions      chan crossfarm.CrossAction
}

func (client *colocatedCrossClientStub) ReserveCrossVisitor(context.Context, crossfarm.CrossAction, uint32) (errcode.Code, error) {
	client.reserveCalls.Add(1)
	return errcode.OK, nil
}

func (*colocatedCrossClientStub) ApplyCrossAction(context.Context, crossfarm.CrossAction) (crossfarm.CrossResult, error) {
	return crossfarm.CrossResult{}, nil
}

func (*colocatedCrossClientStub) DeliverCrossResult(context.Context, crossfarm.CrossResult) (crossfarm.VisitorReward, *farm.PlayerDelta, errcode.Code, error) {
	return crossfarm.VisitorReward{}, nil, errcode.OK, nil
}

func (*colocatedCrossClientStub) AcknowledgeCrossResult(context.Context, uint64, uint64, uint64) error {
	return nil
}

func (*colocatedCrossClientStub) CanExecuteCrossAction(crossfarm.CrossAction) bool { return true }

func (client *colocatedCrossClientStub) ExecuteCrossAction(_ context.Context, action crossfarm.CrossAction, _ uint32) (crossfarm.CrossExecution, error) {
	client.actions <- action
	close(client.started)
	<-client.release
	return crossfarm.CrossExecution{
		Result: crossfarm.CrossResult{
			ReqID: action.ReqID, VisitorUID: action.VisitorUID,
			OwnerUID: action.OwnerUID, Code: errcode.OK,
		},
		Reward:    crossfarm.VisitorReward{ReqID: action.ReqID},
		FarmDelta: &farm.FarmDelta{OwnerUID: action.OwnerUID, FarmSeq: 17},
		Code:      errcode.OK,
	}, nil
}

func TestGatewayUsesColocatedCrossFastPath(t *testing.T) {
	client := &colocatedCrossClientStub{
		started: make(chan struct{}), release: make(chan struct{}), actions: make(chan crossfarm.CrossAction, 1),
	}
	gateway := New(authStub{}, sessionStub{uid: 7}, nil, WithCrossFarmClient(client))
	gateway.gatewayID = "gateway-0"
	connection, writer := newTestPushConn(t, time.Millisecond)
	connection.id = 71
	action := crossfarm.CrossAction{
		ReqID: 1, Kind: crossfarm.Water, VisitorUID: 7, OwnerUID: 9,
		PlotIndex: 0, FriendshipVerified: true,
	}
	connection.roomUID = action.OwnerUID
	response := gateway.dispatchCrossAction(connection, Envelope{Cmd: CommandWater, ClientSeq: 3}, action, 20260810)
	if response.Cmd != 0 {
		t.Fatalf("response = %#v, want deferred", response)
	}
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("colocated execution did not start")
	}
	if calls := client.reserveCalls.Load(); calls != 0 {
		t.Fatalf("legacy reserve calls = %d, want 0", calls)
	}
	if received := <-client.actions; received.Originator.ConnID != connection.id ||
		received.Originator.GatewayID != gateway.gatewayID {
		t.Fatalf("originator = %#v", received.Originator)
	}
	close(client.release)
	frames := waitWrites(t, writer, 2, time.Second)
	commands := make(map[uint32]int)
	for _, frame := range frames {
		envelopes, err := clientwire.DecodeBinaryBatch(frame)
		if err != nil {
			t.Fatalf("decode response frame: %v", err)
		}
		for _, envelope := range envelopes {
			commands[envelope.Cmd]++
		}
	}
	if commands[CommandWater] != 1 || commands[CommandFarmDelta] != 1 {
		t.Fatalf("commands = %#v, want one response and one direct FarmDelta", commands)
	}
	if _, pending := gateway.crossPending.Load(action.ReqID); pending {
		t.Fatal("colocated pending entry was not released")
	}
}
