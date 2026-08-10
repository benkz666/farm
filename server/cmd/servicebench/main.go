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
	mode := flag.String("mode", "farm-stream", "farm-stream, gateway-ws, gateway-handshake, gateway-startup or social-are-friends")
	target := flag.String("target", "farm:9210", "gRPC target")
	token := flag.String("token", "perf-internal-token", "internal bearer token")
	qps := flag.Int("qps", 20000, "open-loop target QPS")
	duration := flag.Duration("duration", 20*time.Second, "measurement duration")
	concurrency := flag.Int("concurrency", 512, "maximum in-flight requests")
	warmupConcurrency := flag.Int("warmup-concurrency", 512, "maximum concurrent WebSocket/Actor warmups before measurement")
	warmupSettle := flag.Duration("warmup-settle", 2*time.Second, "idle time after WebSocket/Actor warmup and before measurement")
	fixedConnections := flag.Int("fixed-connections", 0, "exact prewarmed WebSocket count for comparable gateway scenarios; zero derives it from target QPS")
	uidBase := flag.Uint64("uid-base", 1470, "first fixture UID")
	uidCount := flag.Uint64("uid-count", 2600, "number of fixture UIDs")
	operation := flag.String("operation", "sync", "operation: enter, sync, sync-snapshot, ping, water, harvest, steal, water-visitor, buy, sell, friend-list, search-user, task-list or mail-list")
	accounts := flag.String("accounts", "", "gateway-ws account fixture JSON")
	gatewayURLs := flag.String("gateway-urls", "", "optional comma-separated WebSocket URLs distributed round-robin across fixture accounts")
	perConnectionQPS := flag.Int("per-connection-qps", 8, "gateway-ws maximum commands per connection per second")
	output := flag.String("output", "", "optional JSON result path")
	flag.Parse()
	if *qps <= 0 || *duration <= 0 || *concurrency <= 0 || *warmupConcurrency <= 0 || *warmupSettle < 0 || *fixedConnections < 0 || *uidCount == 0 {
		panic("qps, duration, concurrency, warmup-concurrency and uid-count must be positive; warmup-settle and fixed-connections must not be negative")
	}

	// Large formal fixtures can prewarm tens of thousands of Actor/WebSocket
	// sessions. That work remains outside the measured window but needs its own
	// generous deadline so a valid run is not mistaken for a measurement timeout.
	ctx, cancel := context.WithTimeout(context.Background(), *duration+2*time.Minute)
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
		measured, err = runGatewayWS(ctx, *accounts, *gatewayURLs, *qps, *duration, *concurrency, *warmupConcurrency, *warmupSettle, *perConnectionQPS, *fixedConnections, *operation)
	case "gateway-handshake":
		measured, err = runGatewayHandshake(ctx, *accounts, *gatewayURLs, *qps, *duration, *concurrency)
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

// runGatewayHandshake measures only WebSocket dial + protocol Handshake using
// pre-issued fixture tokens. It deliberately never calls /api/login, so bcrypt
// capacity cannot contaminate the connection benchmark.
func runGatewayHandshake(ctx context.Context, fixturePath, gatewayURLs string, qps int, duration time.Duration, concurrency int) (result, error) {
	if fixturePath == "" {
		return result{}, fmt.Errorf("gateway-handshake requires -accounts")
	}
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		return result{}, err
	}
	var fixture struct {
		TimeProfile string           `json:"time_profile"`
		Accounts    []gatewayAccount `json:"accounts"`
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
	for index, account := range fixture.Accounts {
		if account.Token == "" || account.WSURL == "" {
			return result{}, fmt.Errorf("gateway handshake account %d lacks token/ws_url", index)
		}
	}
	concurrency = min(concurrency, len(fixture.Accounts))
	total := min(oneShotOperationCount(qps, duration), len(fixture.Accounts))
	jobs := make(chan gatewayAccount, concurrency)
	recorded := &recorder{}
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for account := range jobs {
				started := time.Now()
				client, openErr := openGatewayBenchConnection(ctx, account, "handshake", fixture.TimeProfile)
				if client != nil {
					_ = client.conn.Close()
				}
				recorded.add(time.Since(started), openErr == nil, -1)
			}
		}()
	}

	started := time.Now()
	for index := 0; index < total; index++ {
		if err := waitUntil(ctx, started.Add(oneShotStartOffset(index, qps))); err != nil {
			break
		}
		select {
		case jobs <- fixture.Accounts[index]:
		case <-ctx.Done():
			index = total
		}
	}
	close(jobs)
	workers.Wait()
	return summarize("gateway-handshake", qps, uint64(total), time.Since(started), recorded, ctx.Err() != nil), nil
}

type gatewayAccount struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	UID          string `json:"uid"`
	Token        string `json:"token"`
	WSURL        string `json:"ws_url"`
	OwnerUID     string `json:"owner_uid"`
	PeerUID      string `json:"peer_uid"`
	PeerUsername string `json:"peer_username"`
	PlotIndex    int    `json:"plot_index"`
	PlotIndexes  []int  `json:"plot_indexes,omitempty"`
	CropID       int    `json:"crop_id"`
	ItemID       int    `json:"item_id"`
	Quantity     int    `json:"quantity"`
}

type gatewayBenchConnection struct {
	conn           *websocket.Conn
	nextSeq        uint32
	farmSeq        clientjson.Uint64
	account        gatewayAccount
	timeProfile    string
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
		TimeProfile string           `json:"time_profile"`
		Accounts    []gatewayAccount `json:"accounts"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return result{}, fmt.Errorf("decode gateway accounts: %w", err)
	}
	if len(fixture.Accounts) == 0 {
		return result{}, fmt.Errorf("gateway account fixture is empty")
	}
	if fixture.TimeProfile != "" && !gameconfig.ValidTimeProfile(fixture.TimeProfile) {
		return result{}, fmt.Errorf("gateway account fixture has invalid time_profile %q", fixture.TimeProfile)
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
	client, err := openGatewayBenchConnection(ctx, gatewayAccount{Token: token, WSURL: account.WSURL}, "enter", "")
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

func runGatewayWS(ctx context.Context, fixturePath, gatewayURLs string, qps int, duration time.Duration, maxConnections, warmupConcurrency int, warmupSettle time.Duration, perConnectionQPS, fixedConnections int, operation string) (result, error) {
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
		TimeProfile string           `json:"time_profile"`
		Accounts    []gatewayAccount `json:"accounts"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return result{}, fmt.Errorf("decode gateway accounts: %w", err)
	}
	if len(fixture.Accounts) == 0 {
		return result{}, fmt.Errorf("gateway account fixture is empty")
	}
	if fixture.TimeProfile != "" && !gameconfig.ValidTimeProfile(fixture.TimeProfile) {
		return result{}, fmt.Errorf("gateway account fixture has invalid time_profile %q", fixture.TimeProfile)
	}
	if err := validateGatewayFixturePlots(fixture.Accounts, operation); err != nil {
		return result{}, err
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
	minimumConnections := (qps + perConnectionQPS - 1) / perConnectionQPS
	connections := minimumConnections
	plannedOperations := oneShotOperationCount(qps, duration)
	if gatewayOperationOneShot(operation) {
		// Formal fixtures expose multiple independent legal plots per account.
		// Size the default connection pool by both the requested arrival rate and
		// the total legal state-transition capacity. A fixed pool can be requested
		// below to make the Actor/WebSocket working set identical across APIs.
		actionsPerAccount := minimumGatewayActionCount(fixture.Accounts, operation)
		connectionsForCapacity := (plannedOperations + actionsPerAccount - 1) / actionsPerAccount
		if connectionsForCapacity > connections {
			connections = connectionsForCapacity
		}
	}
	if fixedConnections > 0 {
		if fixedConnections > maxConnections {
			return result{}, fmt.Errorf("fixed-connections %d exceeds concurrency limit %d", fixedConnections, maxConnections)
		}
		if fixedConnections > len(fixture.Accounts) {
			return result{}, fmt.Errorf("fixed-connections %d exceeds fixture accounts %d", fixedConnections, len(fixture.Accounts))
		}
		connections = fixedConnections
	} else {
		connections = min(connections, maxConnections, len(fixture.Accounts))
	}
	if connections <= 0 {
		return result{}, fmt.Errorf("gateway-ws has no usable connections")
	}
	if connections < minimumConnections {
		return result{}, fmt.Errorf(
			"gateway-ws needs at least %d connections for %d QPS at %d commands/connection/s; got %d",
			minimumConnections, qps, perConnectionQPS, connections,
		)
	}
	oneShotActions := 1
	if gatewayOperationOneShot(operation) {
		oneShotActions = minimumGatewayActionCount(fixture.Accounts[:connections], operation)
		if capacity := connections * oneShotActions; plannedOperations > capacity {
			return result{}, fmt.Errorf(
				"gateway %s fixture has %d legal actions, but %d QPS for %s requires %d; lower QPS/duration or add accounts/plots",
				operation, capacity, qps, duration, plannedOperations,
			)
		}
	}

	clients := make([]*gatewayBenchConnection, connections)
	connectErrs := make(chan error, connections)
	warmupSlots := make(chan struct{}, min(warmupConcurrency, connections))
	var connectWG sync.WaitGroup
	for index := range connections {
		connectWG.Add(1)
		go func(index int) {
			defer connectWG.Done()
			select {
			case warmupSlots <- struct{}{}:
				defer func() { <-warmupSlots }()
			case <-ctx.Done():
				connectErrs <- ctx.Err()
				return
			}
			client, dialErr := openGatewayBenchConnection(ctx, fixture.Accounts[index], operation, fixture.TimeProfile)
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
	// Opening 15,000 formal-fixture connections can take longer than the small
	// process-local Task/Mail cache TTL. Refresh those read paths only after all
	// connections are ready, so the measured window really starts with the
	// complete working set hot instead of mixing cache expiry into the result.
	if gatewayOperationNeedsFinalReadWarmup(operation) {
		if err := refreshGatewayReadCache(ctx, clients, operation, warmupConcurrency); err != nil {
			for _, client := range clients {
				_ = client.conn.Close()
			}
			return result{}, err
		}
	}
	if warmupSettle > 0 {
		timer := time.NewTimer(warmupSettle)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			for _, client := range clients {
				_ = client.conn.Close()
			}
			return result{}, ctx.Err()
		}
	}

	recorded := &recorder{}
	var sent atomic.Uint64
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(len(clients))
	var measurementStarted time.Time
	oneShot := gatewayOperationOneShot(operation)
	for index, client := range clients {
		go func(index int, client *gatewayBenchConnection) {
			defer workers.Done()
			defer client.conn.Close()
			<-start
			for round := 0; ; round++ {
				globalIndex := round*connections + index
				if globalIndex >= plannedOperations {
					return
				}
				// Interleave accounts in each round. This keeps the global stream
				// evenly paced and gives one account connections/qps seconds between
				// commands instead of releasing a synchronized connection burst.
				due := measurementStarted.Add(oneShotStartOffset(globalIndex, qps))
				if err := waitUntil(ctx, due); err != nil {
					return
				}
				var request gateway.Envelope
				if oneShot {
					request = client.requestAt(operation, round)
				} else {
					request = client.request(operation)
				}
				requestStarted := time.Now()
				sent.Add(1)
				response, exchangeErr := client.exchange(request)
				code := int32(-1)
				if exchangeErr == nil {
					code = int32(response.Err)
				}
				recorded.add(time.Since(requestStarted), exchangeErr == nil && response.Err == 0, code)
			}
		}(index, client)
	}
	measurementStarted = time.Now()
	close(start)
	workers.Wait()
	wall := time.Since(measurementStarted)
	return summarize("gateway-ws-"+operation, qps, sent.Load(), wall, recorded, ctx.Err() != nil), nil
}

func refreshGatewayReadCache(ctx context.Context, clients []*gatewayBenchConnection, operation string, concurrency int) error {
	if len(clients) == 0 {
		return nil
	}
	concurrency = max(1, min(concurrency, len(clients)))
	slots := make(chan struct{}, concurrency)
	errs := make(chan error, 1)
	var wait sync.WaitGroup
	for _, client := range clients {
		wait.Add(1)
		go func(client *gatewayBenchConnection) {
			defer wait.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				select {
				case errs <- ctx.Err():
				default:
				}
				return
			}
			response, err := client.exchange(client.request(operation))
			if err == nil && response.Err == 0 {
				return
			}
			select {
			case errs <- fmt.Errorf("gateway %s final warm request: err=%v code=%d", operation, err, response.Err):
			default:
			}
		}(client)
	}
	wait.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

func oneShotOperationCount(qps int, duration time.Duration) int {
	if qps <= 0 || duration <= 0 {
		return 0
	}
	// Use seconds as a floating-point value so extreme CLI inputs cannot
	// overflow before the platform-int cap is applied.
	count := float64(qps) * duration.Seconds()
	if count < 1 {
		return 1
	}
	maxInt := int(^uint(0) >> 1)
	if count >= float64(maxInt) {
		return maxInt
	}
	return int(count)
}

func oneShotStartOffset(index, qps int) time.Duration {
	if index <= 0 || qps <= 0 {
		return 0
	}
	return time.Duration(index) * time.Second / time.Duration(qps)
}

func waitUntil(ctx context.Context, due time.Time) error {
	delay := time.Until(due)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func openGatewayBenchConnection(ctx context.Context, account gatewayAccount, operation, timeProfile string) (*gatewayBenchConnection, error) {
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
	client := &gatewayBenchConnection{conn: conn, nextSeq: 1, account: account, timeProfile: timeProfile, searchUsername: searchUsername}
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
	if gatewayOperationNeedsOwnActor(operation) {
		if err := client.prewarmFarm("0"); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if operation == "steal" || operation == "water-visitor" {
		ownerUID := account.PeerUID
		if operation == "water-visitor" && account.OwnerUID != "" && account.OwnerUID != "0" {
			ownerUID = account.OwnerUID
		}
		if ownerUID == "" || ownerUID == "0" {
			_ = conn.Close()
			return nil, fmt.Errorf("gateway %s fixture has no owner uid", operation)
		}
		if err := client.prewarmFarm(ownerUID); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if gatewayOperationWarmRequest(operation) {
		response, err := client.exchange(client.request(operation))
		if err != nil || response.Err != 0 {
			_ = conn.Close()
			return nil, fmt.Errorf("gateway %s warm request: err=%v code=%d", operation, err, response.Err)
		}
	}
	return client, nil
}

func (client *gatewayBenchConnection) prewarmFarm(ownerUID string) error {
	response, err := client.exchange(gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: client.nextSeq,
		Payload:   json.RawMessage(fmt.Sprintf(`{"owner_uid":%q}`, ownerUID)),
	})
	client.nextSeq++
	if err != nil || response.Err != 0 {
		return fmt.Errorf("gateway enter precondition owner=%s: err=%v code=%d", ownerUID, err, response.Err)
	}
	var entered struct {
		FarmSeq     clientjson.Uint64 `json:"farm_seq"`
		TimeProfile string            `json:"time_profile"`
	}
	if err := json.Unmarshal(response.Payload, &entered); err != nil {
		return err
	}
	if client.timeProfile != "" && entered.TimeProfile != client.timeProfile {
		return fmt.Errorf(
			"gateway enter precondition owner=%s: fixture time_profile=%s but Farm returned %s",
			ownerUID, client.timeProfile, entered.TimeProfile,
		)
	}
	client.farmSeq = entered.FarmSeq
	return nil
}

func (client *gatewayBenchConnection) request(operation string) gateway.Envelope {
	return client.requestAt(operation, 0)
}

func (client *gatewayBenchConnection) requestAt(operation string, actionIndex int) gateway.Envelope {
	request := gateway.Envelope{ClientSeq: client.nextSeq}
	client.nextSeq++
	plotIndex := client.account.PlotIndex
	if len(client.account.PlotIndexes) > 0 {
		plotIndex = client.account.PlotIndexes[actionIndex%len(client.account.PlotIndexes)]
	}
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
	case "water", "water-visitor":
		ownerUID := "0"
		if operation == "water-visitor" {
			ownerUID = client.account.OwnerUID
			if ownerUID == "" || ownerUID == "0" {
				ownerUID = client.account.PeerUID
			}
		}
		request.Cmd = gateway.CommandWater
		request.Payload = json.RawMessage(fmt.Sprintf(
			`{"owner_uid":%q,"plot_index":%d,"arg":0}`,
			ownerUID, plotIndex,
		))
	case "harvest":
		request.Cmd = gateway.CommandHarvest
		request.Payload = json.RawMessage(fmt.Sprintf(
			`{"owner_uid":"0","plot_index":%d,"arg":0}`,
			plotIndex,
		))
	case "steal":
		request.Cmd = gateway.CommandSteal
		request.Payload = json.RawMessage(fmt.Sprintf(
			`{"owner_uid":%q,"plot_index":%d,"crop_id":%d}`,
			client.account.PeerUID, plotIndex, max(client.account.CropID, 1),
		))
	case "buy":
		request.Cmd = gateway.CommandBuy
		request.Payload = json.RawMessage(fmt.Sprintf(
			`{"item_id":%d,"quantity":%d}`,
			max(client.account.ItemID, 1), max(client.account.Quantity, 1),
		))
	case "sell":
		request.Cmd = gateway.CommandSell
		request.Payload = json.RawMessage(fmt.Sprintf(
			`{"item_id":%d,"quantity":%d}`,
			max(client.account.ItemID, 1), max(client.account.Quantity, 1),
		))
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
	case "enter", "sync", "sync-snapshot", "ping",
		"water", "harvest", "steal", "water-visitor", "buy", "sell",
		"friend-list", "search-user", "task-list", "mail-list":
		return true
	default:
		return false
	}
}

func gatewayOperationOneShot(operation string) bool {
	switch operation {
	case "water", "harvest", "steal", "water-visitor":
		return true
	default:
		return false
	}
}

func minimumGatewayActionCount(accounts []gatewayAccount, operation string) int {
	if !gatewayOperationOneShot(operation) || len(accounts) == 0 {
		return 1
	}
	minimum := int(^uint(0) >> 1)
	for _, account := range accounts {
		count := len(account.PlotIndexes)
		if count == 0 {
			count = 1
		}
		if count < minimum {
			minimum = count
		}
	}
	return max(minimum, 1)
}

func validateGatewayFixturePlots(accounts []gatewayAccount, operation string) error {
	if !gatewayOperationOneShot(operation) {
		return nil
	}
	for accountIndex, account := range accounts {
		indexes := account.PlotIndexes
		if len(indexes) == 0 {
			indexes = []int{account.PlotIndex}
		}
		if len(indexes) > gameconfig.MaxPlots {
			return fmt.Errorf("gateway fixture account %d exposes %d plots, maximum is %d", accountIndex, len(indexes), gameconfig.MaxPlots)
		}
		seen := [gameconfig.MaxPlots]bool{}
		for _, plotIndex := range indexes {
			if plotIndex < 0 || plotIndex >= gameconfig.MaxPlots {
				return fmt.Errorf("gateway fixture account %d has invalid plot_index %d", accountIndex, plotIndex)
			}
			if seen[plotIndex] {
				return fmt.Errorf("gateway fixture account %d repeats plot_index %d", accountIndex, plotIndex)
			}
			seen[plotIndex] = true
		}
	}
	return nil
}

func gatewayOperationNeedsOwnActor(operation string) bool {
	switch operation {
	case "enter", "sync", "sync-snapshot", "water", "harvest", "steal", "water-visitor", "buy", "sell":
		return true
	default:
		return false
	}
}

func gatewayOperationWarmRequest(operation string) bool {
	switch operation {
	case "ping", "enter", "sync", "sync-snapshot", "friend-list", "search-user", "task-list", "mail-list":
		return true
	default:
		return false
	}
}

func gatewayOperationNeedsFinalReadWarmup(operation string) bool {
	return operation == "task-list" || operation == "mail-list"
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
