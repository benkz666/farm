package gateway

import (
	"errors"
	"testing"

	"github.com/gorilla/websocket"
)

func TestClassifyWSReadError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "normal", err: &websocket.CloseError{Code: websocket.CloseNormalClosure}, want: "client_normal"},
		{name: "going away", err: &websocket.CloseError{Code: websocket.CloseGoingAway}, want: "client_going_away"},
		{name: "no status", err: &websocket.CloseError{Code: websocket.CloseNoStatusReceived}, want: "client_no_status"},
		{name: "protocol close", err: &websocket.CloseError{Code: websocket.CloseProtocolError}, want: "client_close_error"},
		{name: "read error", err: errors.New("read failed"), want: "read_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyWSReadError(test.err); got != test.want {
				t.Fatalf("classifyWSReadError() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWSDisconnectReasonKeepsFirstCause(t *testing.T) {
	connection := &wsConnection{}
	connection.setDisconnectReason("session_replaced")
	connection.setDisconnectReason("read_error")
	if got := connection.finalDisconnectReason(); got != "session_replaced" {
		t.Fatalf("disconnect reason = %q, want first cause", got)
	}
}
