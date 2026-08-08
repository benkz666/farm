// Command servicebench drives internal gRPC boundaries directly so service
// capacity can be measured independently from the client-facing Gateway path.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/gateway"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientjson"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
)

type result struct {
	Mode       string                `json:"mode"`
	TargetQPS  int                   `json:"target_qps"`
	Sent       uint64                `json:"sent"`
	Succeeded  uint64                `json:"succeeded"`
	Failed     uint64                `json:"failed"`
	ActualQPS  float64               `json:"actual_qps"`
	AverageMS  float64               `json:"average_ms"`
	P50MS      float64               `json:"p50_ms"`
	P95MS      float64               `json:"p95_ms"`
	P99MS      float64               `json:"p99_ms"`
	MaxMS      float64               `json:"max_ms"`
	WallMillis int64                 `json:"wall_millis"`
	FirstError int32                 `json:"first_error_code,omitempty"`
	TimedOut   bool                  `json:"timed_out,omitempty"`
	Steps      map[string]stepResult `json:"steps,omitempty"`
}

type stepResult struct {
	Succeeded uint64  `json:"succeeded"`
	Failed    uint64  `json:"failed"`
	AverageMS float64 `json:"average_ms"`
	P95MS     float64 `json:"p95_ms"`
	P99MS     float64 `json:"p99_ms"`
	MaxMS     float64 `json:"max_ms"`
}

type recorder struct {
	mu        sync.Mutex
	latencies []time.Duration
	succeeded atomic.Uint64
	failed    atomic.Uint64
	firstErr  atomic.Int32
}

func (recorder *recorder) add(latency time.Duration, ok bool, code int32) {
	if ok {
		recorder.succeeded.Add(1)
	} else {
		recorder.failed.Add(1)
		recorder.firstErr.CompareAndSwap(0, code)
	}
	recorder.mu.Lock()
	recorder.latencies = append(recorder.latencies, latency)
	recorder.mu.Unlock()
}

func main() {
	mode := flag.String("mode", "farm-stream", "farm-stream, gateway-ws, gateway-startup or social-are-friends")
	target := flag.String("target", "farm:9210", "gRPC target")
	token := flag.String("token", "perf-internal-token", "internal bearer token")
	qps := flag.Int("qps", 20000, "open-loop target QPS")
	duration := flag.Duration("duration", 20*time.Second, "measurement duration")
	concurrency := flag.Int("concurrency", 512, "maximum in-flight requests")
	uidBase := flag.Uint64("uid-base", 1470, "first fixture UID")
	uidCount := flag.Uint64("uid-count", 2600, "number of fixture UIDs")
	operation := flag.String("operation", "sync", "operation: enter, sync, sync-snapshot, ping, friend-list, search-user, task-list or mail-list")
	accounts := flag.String("accounts", "", "gateway-ws account fixture JSON")
	gatewayURLs := flag.String("gateway-urls", "", "optional comma-separated WebSocket URLs distributed round-robin across fixture accounts")
	perConnectionQPS := flag.Int("per-connection-qps", 8, "gateway-ws maximum commands per connection per second")
	output := flag.String("output", "", "optional JSON result path")
	flag.Parse()
	if *qps <= 0 || *duration <= 0 || *concurrency <= 0 || *uidCount == 0 {
		panic("qps, duration, concurrency and uid-count must be positive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration+30*time.Second)
	defer cancel()

	var measured result
	var err error
	switch *mode {
	case "farm-stream":
		pool := grpcx.NewPool(*token)
		defer pool.Close()
		conn, connErr := pool.Conn(ctx, *target)
		if connErr != nil {
			panic(connErr)
		}
		measured, err = runFarmStream(ctx, conn, *qps, *duration, *concurrency, *uidBase, *uidCount, *operation)
	case "gateway-ws":
		measured, err = runGatewayWS(ctx, *accounts, *gatewayURLs, *qps, *duration, *concurrency, *perConnectionQPS, *operation)
	case "gateway-startup":
		measured, err = runGatewayStartup(ctx, *accounts, *gatewayURLs, *qps, *duration, *concurrency)
	case "social-are-friends":
		pool := grpcx.NewPool(*token)
		defer pool.Close()
		conn, connErr := pool.Conn(ctx, *target)
		if connErr != nil {
			panic(connErr)
		}
		measured, err = runSocial(ctx, conn, *qps, *duration, *concurrency, *uidBase, *uidCount)
	default:
		err = fmt.Errorf("unsupported mode %q", *mode)
	}
	if err != nil {
		panic(err)
	}
	encoded, _ := json.MarshalIndent(measured, "", "  ")
	fmt.Println(string(encoded))
	if *output != "" {
		if err := os.WriteFile(*output, append(encoded, '\n'), 0o644); err != nil {
			panic(err)
		}
	}
}

type gatewayAccount struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	Token        string `json:"token"`
	WSURL        string `json:"ws_url"`
	PeerUsername string `json:"peer_username"`
}

type gatewayBenchConnection struct {
	conn           *websocket.Conn
	nextSeq        uint32
	farmSeq        clientjson.Uint64
	searchUsername string
}

type gatewayStartupRecorders struct {
	login     recorder
	handshake recorder
	enter     recorder
	taskList  recorder
	mailList  recorder
}

type startupAuthResponse struct {
	Token string `json:"token"`
}

func runGatewayStartup(ctx context.Context, fixturePath, gatewayURLs string, qps int, duration time.Duration, concurrency int) (result, error) {
	if fixturePath == "" {
		return result{}, fmt.Errorf("gateway-startup requires -accounts")
	}
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		return result{}, err
	}
	var fixture struct {
		Accounts []gatewayAccount `json:"accounts"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return result{}, fmt.Errorf("decode gateway accounts: %w", err)
	}
	if len(fixture.Accounts) == 0 {
		return result{}, fmt.Errorf("gateway account fixture is empty")
	}
	urls, err := parseGatewayURLs(gatewayURLs)
	if err != nil {
		return result{}, err
	}
	if len(urls) > 0 {
		for index := range fixture.Accounts {
			fixture.Accounts[index].WSURL = urls[index%len(urls)]
		}
	}
	for index, account := range fixture.Accounts {
		if account.Username == "" || account.Password == "" || account.WSURL == "" {
			return result{}, fmt.Errorf("gateway startup account %d lacks username/password/ws_url", index)
		}
	}
	if concurrency > len(fixture.Accounts) {
		concurrency = len(fixture.Accounts)
	}

	transport := &http.Transport{
		MaxIdleConns:        concurrency,
		MaxIdleConnsPerHost: concurrency,
		IdleConnTimeout:     30 * time.Second,
	}
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	defer transport.CloseIdleConnections()

	jobs := make(chan gatewayAccount, concurrency)
	chainRecorder := &recorder{}
	steps := &gatewayStartupRecorders{}
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for account := range jobs {
				started := time.Now()
				code, chainErr := runGatewayStartupChain(ctx, httpClient, account, steps)
				chainRecorder.add(time.Since(started), chainErr == nil, code)
			}
		}()
	}

	started := time.Now()
	deadline := started.Add(duration)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var sent uint64
sendLoop:
	for now := range ticker.C {
		if !now.Before(deadline) || int(sent) >= len(fixture.Accounts) {
			break
		}
		expected := uint64(now.Sub(started).Seconds() * float64(qps))
		for sent < expected && int(sent) < len(fixture.Accounts) {
			account := fixture.Accounts[sent]
			sent++
			select {
			case jobs <- account:
			case <-ctx.Done():
				break sendLoop
			}
		}
	}
	close(jobs)
	workers.Wait()
	wall := time.Since(started)
	measured := summarize("gateway-startup", qps, sent, wall, chainRecorder, ctx.Err() != nil)
	measured.Steps = map[string]stepResult{
		"login":      summarizeStep(&steps.login),
		"handshake":  summarizeStep(&steps.handshake),
		"enter_farm": summarizeStep(&steps.enter),
		"task_list":  summarizeStep(&steps.taskList),
		"mail_list":  summarizeStep(&steps.mailList),
	}
	return measured, nil
}

func runGatewayStartupChain(ctx context.Context, httpClient *http.Client, account gatewayAccount, steps *gatewayStartupRecorders) (int32, error) {
	loginStarted := time.Now()
	token, status, err := loginGateway(ctx, httpClient, account)
	steps.login.add(time.Since(loginStarted), err == nil, status)
	if err != nil {
		return status, err
	}

	handshakeStarted := time.Now()
	client, err := openGatewayBenchConnection(ctx, gatewayAccount{Token: token, WSURL: account.WSURL}, "enter")
	steps.handshake.add(time.Since(handshakeStarted), err == nil, -1)
	if err != nil {
		return -1, err
	}
	defer client.conn.Close()

	commands := []struct {
		command  uint32
		payload  json.RawMessage
		recorder *recorder
	}{
		{gateway.CommandEnterFarm, json.RawMessage(`{"owner_uid":"0"}`), &steps.enter},
		{gateway.CommandTaskList, json.RawMessage(`{}`), &steps.taskList},
		{gateway.CommandMailList, json.RawMessage(`{}`), &steps.mailList},
	}
	for _, command := range commands {
		request := gateway.Envelope{Cmd: command.command, ClientSeq: client.nextSeq, Payload: command.payload}
		client.nextSeq++
		stepStarted := time.Now()
		response, exchangeErr := client.exchange(request)
		code := int32(-1)
		if exchangeErr == nil {
			code = int32(response.Err)
		}
		ok := exchangeErr == nil && response.Err == 0
		command.recorder.add(time.Since(stepStarted), ok, code)
		if !ok {
			if exchangeErr != nil {
				return code, exchangeErr
			}
			return code, fmt.Errorf("gateway command %d returned %d", command.command, response.Err)
		}
	}
	return 0, nil
}

func loginGateway(ctx context.Context, client *http.Client, account gatewayAccount) (string, int32, error) {
	wsURL, err := url.Parse(account.WSURL)
	if err != nil {
		return "", -1, err
	}
	scheme := "http"
	if wsURL.Scheme == "wss" {
		scheme = "https"
	} else if wsURL.Scheme != "ws" {
		return "", -1, fmt.Errorf("unsupported WebSocket scheme %q", wsURL.Scheme)
	}
	endpoint := &url.URL{Scheme: scheme, Host: wsURL.Host, Path: "/api/login"}
	body, err := json.Marshal(map[string]string{"username": account.Username, "password": account.Password})
	if err != nil {
		return "", -1, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", -1, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", -1, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return "", -int32(response.StatusCode), fmt.Errorf("login HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var auth startupAuthResponse
	if err := json.NewDecoder(response.Body).Decode(&auth); err != nil {
		return "", -1, err
	}
	if auth.Token == "" {
		return "", -1, fmt.Errorf("login returned an empty token")
	}
	return auth.Token, 0, nil
}

func parseGatewayURLs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	urls := make([]string, 0, 4)
	for _, candidate := range strings.Split(raw, ",") {
		candidate = strings.TrimSpace(candidate)
		if !strings.HasPrefix(candidate, "ws://") && !strings.HasPrefix(candidate, "wss://") {
			return nil, fmt.Errorf("invalid gateway URL %q", candidate)
		}
		urls = append(urls, candidate)
	}
	return urls, nil
}

func runGatewayWS(ctx context.Context, fixturePath, gatewayURLs string, qps int, duration time.Duration, maxConnections, perConnectionQPS int, operation string) (result, error) {
	if !gatewayOperationSupported(operation) {
		return result{}, fmt.Errorf("unsupported gateway operation %q", operation)
	}
	if fixturePath == "" {
		return result{}, fmt.Errorf("gateway-ws requires -accounts")
	}
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		return result{}, err
	}
	var fixture struct {
		Accounts []gatewayAccount `json:"accounts"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return result{}, fmt.Errorf("decode gateway accounts: %w", err)
	}
	if len(fixture.Accounts) == 0 {
		return result{}, fmt.Errorf("gateway account fixture is empty")
	}
	if gatewayURLs != "" {
		urls, err := parseGatewayURLs(gatewayURLs)
		if err != nil {
			return result{}, err
		}
		for index := range fixture.Accounts {
			fixture.Accounts[index].WSURL = urls[index%len(urls)]
		}
	}
	if perConnectionQPS <= 0 {
		return result{}, fmt.Errorf("per-connection-qps must be positive")
	}
	connections := (qps + perConnectionQPS - 1) / perConnectionQPS
	if connections > maxConnections {
		connections = maxConnections
	}
	if connections > len(fixture.Accounts) {
		connections = len(fixture.Accounts)
	}
	if connections <= 0 {
		return result{}, fmt.Errorf("gateway-ws has no usable connections")
	}

	clients := make([]*gatewayBenchConnection, connections)
	connectErrs := make(chan error, connections)
	var connectWG sync.WaitGroup
	for index := range connections {
		connectWG.Add(1)
		go func(index int) {
			defer connectWG.Done()
			client, dialErr := openGatewayBenchConnection(ctx, fixture.Accounts[index], operation)
			if dialErr != nil {
				connectErrs <- dialErr
				return
			}
			clients[index] = client
		}(index)
	}
	connectWG.Wait()
	close(connectErrs)
	if err := <-connectErrs; err != nil {
		for _, client := range clients {
			if client != nil {
				_ = client.conn.Close()
			}
		}
		return result{}, err
	}

	recorded := &recorder{}
	var sent atomic.Uint64
	interval := time.Duration(float64(time.Second) * float64(connections) / float64(qps))
	minimumInterval := time.Second / time.Duration(perConnectionQPS)
	if interval < minimumInterval {
		interval = minimumInterval
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(len(clients))
	for _, client := range clients {
		go func(client *gatewayBenchConnection) {
			defer workers.Done()
			defer client.conn.Close()
			<-start
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			deadline := time.Now().Add(duration)
			for {
				select {
				case now := <-ticker.C:
					if !now.Before(deadline) {
						return
					}
					request := client.request(operation)
					started := time.Now()
					sent.Add(1)
					response, exchangeErr := client.exchange(request)
					code := int32(-1)
					if exchangeErr == nil {
						code = int32(response.Err)
					}
					recorded.add(time.Since(started), exchangeErr == nil && response.Err == 0, code)
				case <-ctx.Done():
					return
				}
			}
		}(client)
	}
	started := time.Now()
	close(start)
	workers.Wait()
	wall := time.Since(started)
	return summarize("gateway-ws-"+operation, qps, sent.Load(), wall, recorded, ctx.Err() != nil), nil
}

func openGatewayBenchConnection(ctx context.Context, account gatewayAccount, operation string) (*gatewayBenchConnection, error) {
	if account.Token == "" || account.WSURL == "" {
		return nil, fmt.Errorf("gateway account token/ws_url is empty")
	}
	dialer := *websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, account.WSURL, http.Header{
		"Sec-WebSocket-Protocol": []string{gateway.BinarySubprotocol},
	})
	if err != nil {
		return nil, err
	}
	searchUsername := account.PeerUsername
	if searchUsername == "" {
		searchUsername = account.Username
	}
	client := &gatewayBenchConnection{conn: conn, nextSeq: 1, searchUsername: searchUsername}
	handshake := gateway.Envelope{
		Cmd:       gateway.CommandHandshake,
		ClientSeq: client.nextSeq,
		Payload: json.RawMessage(fmt.Sprintf(
			`{"token":%q,"client_config_ver":%d}`,
			account.Token,
			gameconfig.ConfigVer,
		)),
	}
	client.nextSeq++
	if response, err := client.exchange(handshake); err != nil || response.Err != 0 {
		_ = conn.Close()
		return nil, fmt.Errorf("gateway handshake: err=%v code=%d", err, response.Err)
	}
	if operation == "sync" || operation == "sync-snapshot" {
		response, err := client.exchange(gateway.Envelope{
			Cmd:       gateway.CommandEnterFarm,
			ClientSeq: client.nextSeq,
			Payload:   json.RawMessage(`{"owner_uid":"0"}`),
		})
		client.nextSeq++
		if err != nil || response.Err != 0 {
			_ = conn.Close()
			return nil, fmt.Errorf("gateway enter precondition: err=%v code=%d", err, response.Err)
		}
		var entered struct {
			FarmSeq clientjson.Uint64 `json:"farm_seq"`
		}
		if err := json.Unmarshal(response.Payload, &entered); err != nil {
			_ = conn.Close()
			return nil, err
		}
		client.farmSeq = entered.FarmSeq
	}
	return client, nil
}

func (client *gatewayBenchConnection) request(operation string) gateway.Envelope {
	request := gateway.Envelope{ClientSeq: client.nextSeq}
	client.nextSeq++
	switch operation {
	case "enter":
		request.Cmd = gateway.CommandEnterFarm
		request.Payload = json.RawMessage(`{"owner_uid":"0"}`)
	case "sync":
		request.Cmd = gateway.CommandSyncFarm
		request.Payload = json.RawMessage(fmt.Sprintf(`{"owner_uid":"0","from_seq":"%d"}`, client.farmSeq))
	case "sync-snapshot":
		request.Cmd = gateway.CommandSyncFarm
		request.Payload = json.RawMessage(`{"owner_uid":"0","from_seq":"18446744073709551615"}`)
	case "ping":
		request.Cmd = gateway.CommandPing
		request.Payload = json.RawMessage(fmt.Sprintf(`{"client_time":%d}`, time.Now().UnixMilli()))
	case "friend-list":
		request.Cmd = gateway.CommandFriendList
		request.Payload = json.RawMessage(`{}`)
	case "search-user":
		request.Cmd = gateway.CommandSearchUser
		request.Payload = json.RawMessage(fmt.Sprintf(`{"username":%q}`, client.searchUsername))
	case "task-list":
		request.Cmd = gateway.CommandTaskList
		request.Payload = json.RawMessage(`{}`)
	case "mail-list":
		request.Cmd = gateway.CommandMailList
		request.Payload = json.RawMessage(`{}`)
	}
	return request
}

func gatewayOperationSupported(operation string) bool {
	switch operation {
	case "enter", "sync", "sync-snapshot", "ping", "friend-list", "search-user", "task-list", "mail-list":
		return true
	default:
		return false
	}
}

func (client *gatewayBenchConnection) exchange(request gateway.Envelope) (gateway.Envelope, error) {
	frame, err := gateway.EncodeBinaryBatch([]gateway.Envelope{request})
	if err != nil {
		return gateway.Envelope{}, err
	}
	_ = client.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := client.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		return gateway.Envelope{}, err
	}
	for {
		_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		messageType, data, err := client.conn.ReadMessage()
		if err != nil {
			return gateway.Envelope{}, err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		envelopes, err := gateway.DecodeBinaryBatch(data)
		if err != nil {
			return gateway.Envelope{}, err
		}
		for _, response := range envelopes {
			if response.Cmd == request.Cmd && response.ClientSeq == request.ClientSeq {
				return response, nil
			}
		}
	}
}

func runFarmStream(ctx context.Context, conn grpc.ClientConnInterface, qps int, duration time.Duration, concurrency int, uidBase, uidCount uint64, operation string) (result, error) {
	client := farmv1.NewFarmCommandServiceClient(conn)
	stream, err := client.ExecuteStream(ctx)
	if err != nil {
		return result{}, err
	}
	op := farmv1.Operation_OPERATION_SYNC_FARM
	payload := []byte(`{"from_seq":0}`)
	switch operation {
	case "enter":
		op = farmv1.Operation_OPERATION_ENTER_FARM
		payload = nil
	case "sync":
	case "sync-snapshot":
		// Ahead-of-server sequence deterministically selects the full-snapshot
		// recovery path without depending on fixture FarmSeq or delta-ring state.
		payload = []byte(`{"from_seq":18446744073709551615}`)
	default:
		return result{}, fmt.Errorf("unsupported farm operation %q", operation)
	}

	type pendingRequest struct{ started time.Time }
	pending := make(map[uint64]pendingRequest, concurrency)
	var pendingMu sync.Mutex
	semaphore := make(chan struct{}, concurrency)
	var wait sync.WaitGroup
	recorded := &recorder{}
	receiveErr := make(chan error, 1)
	go func() {
		for {
			response, recvErr := stream.Recv()
			if recvErr != nil {
				receiveErr <- recvErr
				return
			}
			pendingMu.Lock()
			request, ok := pending[response.GetRequestId()]
			delete(pending, response.GetRequestId())
			pendingMu.Unlock()
			if ok {
				code := int32(-1)
				if response.GetResponse() != nil {
					code = response.GetResponse().GetErr()
				}
				recorded.add(time.Since(request.started), code == 0, code)
				<-semaphore
				wait.Done()
			}
		}
	}()

	started := time.Now()
	deadline := started.Add(duration)
	var sent uint64
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
sendLoop:
	for now := range ticker.C {
		if !now.Before(deadline) {
			break
		}
		expected := uint64(now.Sub(started).Seconds() * float64(qps))
		for sent < expected {
			select {
			case semaphore <- struct{}{}:
			case err := <-receiveErr:
				return result{}, err
			case <-ctx.Done():
				break sendLoop
			}
			sent++
			requestID := sent
			requestStarted := time.Now()
			pendingMu.Lock()
			pending[requestID] = pendingRequest{started: requestStarted}
			pendingMu.Unlock()
			wait.Add(1)
			if err := stream.Send(&farmv1.StreamExecuteRequest{
				RequestId: requestID,
				Request: &farmv1.ExecuteRequest{
					Operation:   op,
					FarmUid:     uidBase + requestID%uidCount,
					PayloadJson: payload,
				},
			}); err != nil {
				return result{}, err
			}
		}
	}
	drained := make(chan struct{})
	go func() {
		wait.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case err := <-receiveErr:
		if ctx.Err() != nil {
			markOutstandingFailed(recorded, sent)
			return summarize("farm-stream-"+operation, qps, sent, time.Since(started), recorded, true), nil
		}
		return result{}, err
	case <-ctx.Done():
		markOutstandingFailed(recorded, sent)
		return summarize("farm-stream-"+operation, qps, sent, time.Since(started), recorded, true), nil
	}
	wall := time.Since(started)
	return summarize("farm-stream-"+operation, qps, sent, wall, recorded, false), nil
}

func runSocial(ctx context.Context, conn grpc.ClientConnInterface, qps int, duration time.Duration, concurrency int, uidBase, uidCount uint64) (result, error) {
	client := farmv1.NewSocialServiceClient(conn)
	type job struct {
		id      uint64
		started time.Time
	}
	jobs := make(chan job, concurrency)
	recorded := &recorder{}
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for request := range jobs {
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := client.AreFriends(callCtx, &farmv1.AreFriendsRequest{
					Uid:     uidBase + request.id%uidCount,
					PeerUid: uidBase + (request.id+1)%uidCount,
				})
				cancel()
				recorded.add(time.Since(request.started), err == nil, -1)
			}
		}()
	}
	started := time.Now()
	deadline := started.Add(duration)
	var sent uint64
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
sendLoop:
	for now := range ticker.C {
		if !now.Before(deadline) {
			break
		}
		expected := uint64(now.Sub(started).Seconds() * float64(qps))
		for sent < expected {
			sent++
			select {
			case jobs <- job{id: sent, started: time.Now()}:
			case <-ctx.Done():
				break sendLoop
			}
		}
	}
	close(jobs)
	workers.Wait()
	wall := time.Since(started)
	return summarize("social-are-friends", qps, sent, wall, recorded, ctx.Err() != nil), nil
}

func markOutstandingFailed(recorded *recorder, sent uint64) {
	completed := recorded.succeeded.Load() + recorded.failed.Load()
	if sent > completed {
		recorded.failed.Add(sent - completed)
	}
}

func summarize(mode string, targetQPS int, sent uint64, wall time.Duration, recorder *recorder, timedOut bool) result {
	recorder.mu.Lock()
	latencies := append([]time.Duration(nil), recorder.latencies...)
	recorder.mu.Unlock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	measured := result{
		Mode:       mode,
		TargetQPS:  targetQPS,
		Sent:       sent,
		Succeeded:  recorder.succeeded.Load(),
		Failed:     recorder.failed.Load(),
		WallMillis: wall.Milliseconds(),
		FirstError: recorder.firstErr.Load(),
		TimedOut:   timedOut,
	}
	if wall > 0 {
		measured.ActualQPS = float64(measured.Succeeded) / wall.Seconds()
	}
	if len(latencies) == 0 {
		return measured
	}
	var total time.Duration
	for _, latency := range latencies {
		total += latency
	}
	measured.AverageMS = float64(total) / float64(len(latencies)) / float64(time.Millisecond)
	measured.P50MS = millis(percentile(latencies, 0.50))
	measured.P95MS = millis(percentile(latencies, 0.95))
	measured.P99MS = millis(percentile(latencies, 0.99))
	measured.MaxMS = millis(latencies[len(latencies)-1])
	return measured
}

func summarizeStep(recorder *recorder) stepResult {
	recorder.mu.Lock()
	latencies := append([]time.Duration(nil), recorder.latencies...)
	recorder.mu.Unlock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	measured := stepResult{
		Succeeded: recorder.succeeded.Load(),
		Failed:    recorder.failed.Load(),
	}
	if len(latencies) == 0 {
		return measured
	}
	var total time.Duration
	for _, latency := range latencies {
		total += latency
	}
	measured.AverageMS = float64(total) / float64(len(latencies)) / float64(time.Millisecond)
	measured.P95MS = millis(percentile(latencies, 0.95))
	measured.P99MS = millis(percentile(latencies, 0.99))
	measured.MaxMS = millis(latencies[len(latencies)-1])
	return measured
}

func percentile(sorted []time.Duration, fraction float64) time.Duration {
	index := int(float64(len(sorted)-1) * fraction)
	return sorted[index]
}

func millis(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}
