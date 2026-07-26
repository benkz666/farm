// Command smoke exercises the phase 1 HTTP and WebSocket happy path.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/gateway"
	"farm/server/internal/pkgerr"
)

const (
	defaultBaseURL = "http://127.0.0.1:8080"
	smokePassword  = "smoke-password"
)

type authResponse struct {
	UID   uint64 `json:"uid"`
	Token string `json:"token"`
	WSURL string `json:"ws_url"`
}

type enterFarmPayload struct {
	Snapshot farm.FarmSnapshotJSON `json:"snapshot"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		os.Exit(1)
	}
	fmt.Println("smoke: register/login/handshake/enter-farm passed")
}

func run() error {
	baseURL := strings.TrimRight(getenv("FARM_SMOKE_BASE_URL", defaultBaseURL), "/")
	username, err := smokeUsername()
	if err != nil {
		return err
	}

	if _, err := authenticate(baseURL+"/api/register", username, smokePassword); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	login, err := authenticate(baseURL+"/api/login", username, smokePassword)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if login.UID == 0 || login.Token == "" || login.WSURL == "" {
		return fmt.Errorf("login returned incomplete credentials")
	}

	conn, response, err := websocket.DefaultDialer.Dial(login.WSURL, http.Header{
		"Sec-WebSocket-Protocol": []string{gateway.JSONSubprotocol},
	})
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer conn.Close()
	if response.Header.Get("Sec-WebSocket-Protocol") != gateway.JSONSubprotocol {
		return fmt.Errorf("websocket subprotocol was not negotiated")
	}

	if err := exchange(conn, gateway.Envelope{
		Cmd:       gateway.CommandHandshake,
		ClientSeq: 1,
		Payload: mustJSON(map[string]any{
			"token":             login.Token,
			"client_config_ver": gameconf.ConfigVer,
		}),
	}); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	responseEnvelope, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0}`),
	})
	if err != nil {
		return fmt.Errorf("enter farm: %w", err)
	}
	var payload enterFarmPayload
	if err := json.Unmarshal(responseEnvelope.Payload, &payload); err != nil {
		return fmt.Errorf("decode EnterFarm payload: %w", err)
	}
	if payload.Snapshot.Coin != gameconf.InitialCoin {
		return fmt.Errorf("coin = %d, want %d", payload.Snapshot.Coin, gameconf.InitialCoin)
	}
	if payload.Snapshot.UnlockedPlots != gameconf.InitialUnlockedPlots {
		return fmt.Errorf("unlocked_plots = %d, want %d", payload.Snapshot.UnlockedPlots, gameconf.InitialUnlockedPlots)
	}
	return nil
}

func authenticate(endpoint, username, password string) (authResponse, error) {
	body, err := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	if err != nil {
		return authResponse{}, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return authResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		if readErr != nil {
			return authResponse{}, fmt.Errorf("status %d (read response: %w)", response.StatusCode, readErr)
		}
		return authResponse{}, fmt.Errorf("status %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var result authResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return authResponse{}, err
	}
	return result, nil
}

func exchange(conn *websocket.Conn, request gateway.Envelope) error {
	_, err := exchangeResponse(conn, request)
	return err
}

func exchangeResponse(conn *websocket.Conn, request gateway.Envelope) (gateway.Envelope, error) {
	data, err := gateway.EncodeEnvelope(request)
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("encode request: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return gateway.Envelope{}, fmt.Errorf("write request: %w", err)
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("read response: %w", err)
	}
	if messageType != websocket.TextMessage {
		return gateway.Envelope{}, fmt.Errorf("response message type = %d, want text", messageType)
	}
	response, err := gateway.DecodeEnvelope(data)
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("decode response: %w", err)
	}
	if response.Cmd != request.Cmd || response.ClientSeq != request.ClientSeq {
		return gateway.Envelope{}, fmt.Errorf("response headers = cmd %d, seq %d; want cmd %d, seq %d", response.Cmd, response.ClientSeq, request.Cmd, request.ClientSeq)
	}
	if response.Err != pkgerr.OK {
		return gateway.Envelope{}, fmt.Errorf("err = %d, want %d", response.Err, pkgerr.OK)
	}
	return response, nil
}

func smokeUsername() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate username: %w", err)
	}
	return "smoke-" + hex.EncodeToString(suffix[:]), nil
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
