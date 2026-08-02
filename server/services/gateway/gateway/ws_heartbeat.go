package gateway

import (
	"context"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/platform/pkgerr"
)

// installPongHandler lets valid control-frame Pongs keep an authenticated
// connection and its distributed routing leases alive. It intentionally starts
// only after Handshake succeeds, so unauthenticated sockets still time out.
func (connection *wsConnection) installPongHandler(gateway *Gateway, readTimeout time.Duration) {
	if connection == nil || connection.conn == nil {
		return
	}
	connection.conn.SetPongHandler(func(string) error {
		if !connection.authed {
			return nil
		}
		if err := connection.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return err
		}
		if gateway != nil && !gateway.renewConnectionLease(context.Background(), connection) {
			connection.kick(pkgerr.Kicked)
		}
		return nil
	})
}

// enableHeartbeat starts a server-driven WebSocket Ping loop after Handshake.
// Browsers answer these control frames even when page JavaScript is throttled.
func (connection *wsConnection) enableHeartbeat() {
	connection.enableHeartbeatEvery(wsHeartbeatInterval)
}

func (connection *wsConnection) enableHeartbeatEvery(interval time.Duration) {
	if connection == nil || connection.conn == nil || interval <= 0 {
		return
	}
	connection.heartbeatMu.Lock()
	if connection.heartbeatClosed || connection.heartbeatStarted {
		connection.heartbeatMu.Unlock()
		return
	}
	connection.heartbeatStarted = true
	connection.heartbeatStop = make(chan struct{})
	connection.heartbeatDone = make(chan struct{})
	connection.heartbeatMu.Unlock()

	go connection.runHeartbeat(interval)
}

func (connection *wsConnection) closeHeartbeat() {
	if connection == nil {
		return
	}
	connection.heartbeatMu.Lock()
	if connection.heartbeatClosed {
		connection.heartbeatMu.Unlock()
		return
	}
	connection.heartbeatClosed = true
	if connection.heartbeatStarted {
		close(connection.heartbeatStop)
	}
	connection.heartbeatMu.Unlock()
}

func (connection *wsConnection) runHeartbeat(interval time.Duration) {
	defer close(connection.heartbeatDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-connection.heartbeatStop:
			return
		case <-ticker.C:
			connection.writeMu.Lock()
			err := connection.conn.WriteControl(
				websocket.PingMessage,
				nil,
				time.Now().Add(wsWriteTimeout),
			)
			connection.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
