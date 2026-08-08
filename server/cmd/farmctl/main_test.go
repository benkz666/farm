package main

import (
	"testing"

	"farm/server/gateway"

	"github.com/gorilla/websocket"
)

func TestReadServerPushDrainsPendingBatchInOrder(t *testing.T) {
	conn := &websocket.Conn{}
	want := []gateway.Envelope{
		{Cmd: gateway.CommandFarmDelta},
		{Cmd: gateway.CommandPlayerDelta},
	}
	pendingServerPushes.Lock()
	pendingServerPushes.byConnection[conn] = append([]gateway.Envelope(nil), want...)
	pendingServerPushes.Unlock()
	defer func() {
		pendingServerPushes.Lock()
		delete(pendingServerPushes.byConnection, conn)
		pendingServerPushes.Unlock()
	}()

	for i := range want {
		got, err := readServerPush(conn)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got.Cmd != want[i].Cmd {
			t.Fatalf("read %d cmd=%d, want %d", i, got.Cmd, want[i].Cmd)
		}
	}
}
