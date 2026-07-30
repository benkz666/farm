package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSHeartbeatKeepsPongCapableConnectionAlive(t *testing.T) {
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()

		connection := &wsConnection{conn: conn, authed: true}
		connection.installPongHandler(nil, 300*time.Millisecond)
		connection.enableHeartbeatEvery(50 * time.Millisecond)
		defer connection.closeHeartbeat()

		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			result <- err
			return
		}
		_, message, err := conn.ReadMessage()
		if err == nil && string(message) != "still alive" {
			err = &unexpectedHeartbeatMessage{message: string(message)}
		}
		result <- err
	}))
	t.Cleanup(server.Close)

	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	endpoint.Scheme = "ws"
	client, _, err := websocket.DefaultDialer.Dial(endpoint.String(), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer client.Close()

	// Gorilla 在 ReadMessage 处理 Ping 控制帧时，会用默认 handler 自动回复 Pong。
	go func() {
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// 超过初始 300ms 读超时；若未正确收 Pong，服务端会在此之前关连接。
	time.Sleep(450 * time.Millisecond)
	if err := client.WriteMessage(websocket.TextMessage, []byte("still alive")); err != nil {
		t.Fatalf("write after heartbeat window: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("server read after heartbeats: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive post-heartbeat message")
	}
}

type unexpectedHeartbeatMessage struct {
	message string
}

func (err *unexpectedHeartbeatMessage) Error() string {
	return "unexpected websocket message: " + err.message
}
