package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/gateway"
	"farm/server/shared/clientwire"
	"farm/server/shared/gameconfig"

	"github.com/gorilla/websocket"
)

// mixedBehaviorModel is intentionally data-driven: the reference QPS values
// are the capacity-model inputs, while -qps scales the complete mix without
// changing its shape.
type mixedBehaviorModel struct {
	Name       string                   `json:"name"`
	Operations []mixedBehaviorOperation `json:"operations"`
}

type mixedBehaviorOperation struct {
	Name         string  `json:"name"`
	ReferenceQPS float64 `json:"reference_qps"`
	Pool         string  `json:"pool"`
	Enabled      bool    `json:"enabled"`
	Reason       string  `json:"reason,omitempty"`

	recorder recorder
	sent     uint64
	start    int
	end      int
}

type mixedJob struct {
	operation     *mixedBehaviorOperation
	round         int
	enqueued      time.Time
	stateOwnerUID string
}

func runGatewayMixed(
	ctx context.Context,
	fixturePath, modelPath, gatewayURLs, excludedRaw string,
	qps int,
	duration time.Duration,
	maxConnections, warmupConcurrency int,
	warmupSettle time.Duration,
	fixedConnections int,
	fixtureAccountOffset int,
	residentActors int,
	residentActorRefresh time.Duration,
	measurementStartUnixMS int64,
	measurementReadyFile string,
	measurementStartFile string,
) (result, error) {
	if fixturePath == "" || modelPath == "" {
		return result{}, fmt.Errorf("gateway-mixed requires -accounts and -behavior-model")
	}
	fixtureData, err := os.ReadFile(fixturePath)
	if err != nil {
		return result{}, err
	}
	var fixture struct {
		TimeProfile string           `json:"time_profile"`
		Accounts    []gatewayAccount `json:"accounts"`
	}
	if err := json.Unmarshal(fixtureData, &fixture); err != nil {
		return result{}, fmt.Errorf("decode gateway accounts: %w", err)
	}
	if len(fixture.Accounts) < 10 {
		return result{}, fmt.Errorf("gateway-mixed needs at least 10 fixture accounts")
	}
	if fixtureAccountOffset < 0 || fixtureAccountOffset >= len(fixture.Accounts) {
		return result{}, fmt.Errorf(
			"fixture-account-offset %d is outside fixture account range [0,%d)",
			fixtureAccountOffset,
			len(fixture.Accounts),
		)
	}
	fixture.Accounts = fixture.Accounts[fixtureAccountOffset:]
	if len(fixture.Accounts) < 10 {
		return result{}, fmt.Errorf("gateway-mixed has fewer than 10 accounts after fixture offset")
	}
	if fixture.TimeProfile != "" && !gameconfig.ValidTimeProfile(fixture.TimeProfile) {
		return result{}, fmt.Errorf("gateway account fixture has invalid time_profile %q", fixture.TimeProfile)
	}
	if gatewayURLs != "" {
		urls, parseErr := parseGatewayURLs(gatewayURLs)
		if parseErr != nil {
			return result{}, parseErr
		}
		for index := range fixture.Accounts {
			fixture.Accounts[index].WSURL = urls[index%len(urls)]
		}
	}

	modelData, err := os.ReadFile(modelPath)
	if err != nil {
		return result{}, err
	}
	var model mixedBehaviorModel
	if err := json.Unmarshal(modelData, &model); err != nil {
		return result{}, fmt.Errorf("decode behavior model: %w", err)
	}
	excluded := make(map[string]struct{})
	for _, name := range strings.Split(excludedRaw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			excluded[name] = struct{}{}
		}
	}
	operations := make([]*mixedBehaviorOperation, 0, len(model.Operations))
	var referenceTotal float64
	seen := make(map[string]struct{}, len(model.Operations))
	matchedExcluded := make(map[string]struct{}, len(excluded))
	for index := range model.Operations {
		operation := &model.Operations[index]
		operation.Name = strings.TrimSpace(operation.Name)
		if !operation.Enabled {
			continue
		}
		if operation.Name == "" || operation.ReferenceQPS <= 0 {
			return result{}, fmt.Errorf("behavior operation %d has invalid name/reference_qps", index)
		}
		if _, ok := seen[operation.Name]; ok {
			return result{}, fmt.Errorf("behavior operation %q is duplicated", operation.Name)
		}
		seen[operation.Name] = struct{}{}
		if !mixedOperationSupported(operation.Name) {
			return result{}, fmt.Errorf("behavior operation %q is not supported by gateway-mixed", operation.Name)
		}
		if _, skip := excluded[operation.Name]; skip {
			matchedExcluded[operation.Name] = struct{}{}
			continue
		}
		operations = append(operations, operation)
		referenceTotal += operation.ReferenceQPS
	}
	for name := range excluded {
		if _, ok := matchedExcluded[name]; !ok {
			return result{}, fmt.Errorf("excluded operation %q is not enabled in the behavior model", name)
		}
	}
	if len(operations) == 0 || referenceTotal <= 0 {
		return result{}, fmt.Errorf("behavior model has no enabled load operations")
	}

	connections := fixedConnections
	if connections == 0 {
		connections = min(maxConnections, len(fixture.Accounts))
	}
	if connections > maxConnections || connections > len(fixture.Accounts) {
		return result{}, fmt.Errorf("gateway-mixed connections %d exceed concurrency/fixture limits", connections)
	}
	if connections < 10 {
		return result{}, fmt.Errorf("gateway-mixed has only %d usable connections", connections)
	}
	if residentActors == 0 {
		residentActors = connections
	}
	if residentActors < connections || residentActors > len(fixture.Accounts) {
		return result{}, fmt.Errorf(
			"gateway-mixed resident actors %d must be between connections %d and fixture accounts %d",
			residentActors, connections, len(fixture.Accounts),
		)
	}
	// Sixty percent of connections own local Farm mutations and forty percent
	// drive cross-farm mutations. Social reads can use the complete account set.
	// Keeping the two Farm mutation pools disjoint prevents local and visitor
	// writes from consuming the same finite plot fixture during one measurement.
	localEnd := connections * 3 / 5
	visitorEnd := connections
	for _, operation := range operations {
		switch operation.Pool {
		case "local":
			operation.start, operation.end = 0, localEnd
		case "visitor":
			operation.start, operation.end = localEnd, visitorEnd
		case "social":
			operation.start, operation.end = 0, connections
		case "all":
			operation.start, operation.end = 0, connections
		default:
			return result{}, fmt.Errorf("behavior operation %q has invalid pool %q", operation.Name, operation.Pool)
		}
		if operation.end <= operation.start {
			return result{}, fmt.Errorf("behavior operation %q has an empty account pool", operation.Name)
		}
	}

	clients := make([]*gatewayBenchConnection, connections)
	var connectionKeepalives atomic.Uint64
	var connectionKeepaliveFailures atomic.Uint64
	keepaliveRegistrations := make(chan *gatewayBenchConnection, connections)
	connectionKeepaliveCtx, cancelConnectionKeepalive := context.WithCancel(ctx)
	var connectionKeepaliveWG sync.WaitGroup
	connectionKeepaliveWG.Add(1)
	go func() {
		defer connectionKeepaliveWG.Done()
		runGatewayMixedPongKeepalive(
			connectionKeepaliveCtx,
			keepaliveRegistrations,
			connections,
			30*time.Second,
			&connectionKeepalives,
			&connectionKeepaliveFailures,
		)
	}()
	defer func() {
		cancelConnectionKeepalive()
		connectionKeepaliveWG.Wait()
	}()
	connectErrs := make(chan error, connections)
	warmupSlots := make(chan struct{}, min(warmupConcurrency, connections))
	var connectWG sync.WaitGroup
	for index := 0; index < connections; index++ {
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
			operation, warmupMode := "sync", gatewayWarmupFull
			if index >= localEnd {
				operation, warmupMode = "ping", gatewayWarmupSessionOnly
			}
			client, openErr := openGatewayBenchConnection(ctx, fixture.Accounts[index], operation, fixture.TimeProfile, warmupMode)
			if openErr == nil && index >= localEnd && index < visitorEnd {
				if client.account.PeerUID == "" || client.account.PeerUID == "0" {
					openErr = fmt.Errorf("mixed visitor account %d has no peer_uid", index)
				} else {
					openErr = client.prewarmFarm(client.account.PeerUID)
				}
			}
			if openErr != nil {
				if client != nil {
					_ = client.conn.Close()
				}
				connectErrs <- openErr
				return
			}
			clients[index] = client
			keepaliveRegistrations <- client
		}(index)
	}
	connectWG.Wait()
	close(keepaliveRegistrations)
	close(connectErrs)
	if connectErr := <-connectErrs; connectErr != nil {
		closeGatewayMixedClients(clients)
		return result{}, connectErr
	}

	extraActorCount := residentActors - connections
	extraActors := selectExtraResidentActors(
		fixture.Accounts, localEnd, visitorEnd, extraActorCount,
	)
	if len(extraActors) != extraActorCount {
		closeGatewayMixedClients(clients)
		return result{}, fmt.Errorf(
			"gateway-mixed found %d unique extra resident actors, want %d",
			len(extraActors), extraActorCount,
		)
	}
	if err := prewarmExtraResidentActors(
		ctx, extraActors, fixture.TimeProfile, warmupConcurrency,
	); err != nil {
		closeGatewayMixedClients(clients)
		return result{}, err
	}

	recorded := &recorder{}
	var residentRefreshes atomic.Uint64
	var residentRefreshFailures atomic.Uint64
	jobs := make([]chan mixedJob, connections)
	var workers sync.WaitGroup
	for index, client := range clients {
		jobs[index] = make(chan mixedJob, 4)
		workers.Add(1)
		go func(client *gatewayBenchConnection, input <-chan mixedJob) {
			defer workers.Done()
			defer client.conn.Close()
			for job := range input {
				if job.stateOwnerUID != "" {
					if err := client.prewarmFarm(job.stateOwnerUID); err != nil {
						residentRefreshFailures.Add(1)
					} else {
						residentRefreshes.Add(1)
					}
					continue
				}
				request := client.mixedRequest(job.operation.Name, job.round)
				response, exchangeErr := client.exchange(request)
				code := int32(-1)
				if exchangeErr == nil {
					code = int32(response.Err)
				}
				latency := time.Since(job.enqueued)
				ok := exchangeErr == nil && response.Err == 0
				job.operation.recorder.add(latency, ok, code)
				recorded.add(latency, ok, code)
			}
		}(client, jobs[index])
	}

	keeperCtx, cancelKeeper := context.WithCancel(ctx)
	var keeperWG sync.WaitGroup
	visitorCount := visitorEnd - localEnd
	if len(extraActors) > 0 && residentActorRefresh > 0 {
		keeperWG.Add(1)
		go func() {
			defer keeperWG.Done()
			period := residentActorRefresh / time.Duration(len(extraActors))
			if period < time.Millisecond {
				period = time.Millisecond
			}
			ticker := time.NewTicker(period)
			defer ticker.Stop()
			var sequence int
			for {
				select {
				case <-keeperCtx.Done():
					return
				case <-ticker.C:
					index := sequence % len(extraActors)
					clientIndex := localEnd + index%visitorCount
					job := mixedJob{stateOwnerUID: extraActors[index].UID, enqueued: time.Now()}
					select {
					case jobs[clientIndex] <- job:
						sequence++
					case <-keeperCtx.Done():
						return
					}
				}
			}
		}()
	}

	stateReadyAt := time.Now()
	if warmupSettle > 0 {
		if err := waitUntil(ctx, time.Now().Add(warmupSettle)); err != nil {
			cancelKeeper()
			keeperWG.Wait()
			cancelConnectionKeepalive()
			connectionKeepaliveWG.Wait()
			for _, input := range jobs {
				close(input)
			}
			workers.Wait()
			return result{}, err
		}
	}
	if measurementReadyFile != "" {
		if err := os.WriteFile(measurementReadyFile, []byte("ready\n"), 0o644); err != nil {
			cancelKeeper()
			keeperWG.Wait()
			cancelConnectionKeepalive()
			connectionKeepaliveWG.Wait()
			for _, input := range jobs {
				close(input)
			}
			workers.Wait()
			return result{}, fmt.Errorf("write measurement ready file: %w", err)
		}
	}
	if measurementStartFile != "" {
		startAt, err := waitForMeasurementStartFile(ctx, measurementStartFile)
		if err != nil {
			cancelKeeper()
			keeperWG.Wait()
			cancelConnectionKeepalive()
			connectionKeepaliveWG.Wait()
			for _, input := range jobs {
				close(input)
			}
			workers.Wait()
			return result{}, err
		}
		measurementStartUnixMS = startAt.UnixMilli()
	}
	if measurementStartUnixMS > 0 {
		startAt := time.UnixMilli(measurementStartUnixMS)
		if measurementStartFile == "" && time.Now().After(startAt) {
			cancelKeeper()
			keeperWG.Wait()
			cancelConnectionKeepalive()
			connectionKeepaliveWG.Wait()
			for _, input := range jobs {
				close(input)
			}
			workers.Wait()
			return result{}, fmt.Errorf("measurement start barrier %d was missed", measurementStartUnixMS)
		}
		if err := waitUntil(ctx, startAt); err != nil {
			cancelKeeper()
			keeperWG.Wait()
			cancelConnectionKeepalive()
			connectionKeepaliveWG.Wait()
			for _, input := range jobs {
				close(input)
			}
			workers.Wait()
			return result{}, err
		}
	}

	planned := oneShotOperationCount(qps, duration)
	measurementStarted := time.Now()
	if measurementStartUnixMS > 0 {
		// All shards report the same logical boundary. Using time.Now() here
		// would make their Prometheus windows differ by a few milliseconds.
		measurementStarted = time.UnixMilli(measurementStartUnixMS)
	}
	var scheduled uint64
	for scheduled < uint64(planned) && ctx.Err() == nil {
		due := measurementStarted.Add(
			time.Duration(scheduled) * time.Second / time.Duration(qps),
		)
		if err := waitUntil(ctx, due); err != nil {
			break
		}
		operation := selectMixedOperation(operations, referenceTotal, scheduled)
		poolSize := operation.end - operation.start
		operationIndex := operation.sent
		operation.sent++
		accountIndex := operation.start + mixedAccountIndex(operation.Name, operationIndex, poolSize)
		round := int(operationIndex / uint64(poolSize))
		job := mixedJob{operation: operation, round: round, enqueued: time.Now()}
		select {
		case jobs[accountIndex] <- job:
		default:
			// A bounded open-loop generator must expose server saturation as a
			// dropped/failed arrival instead of silently reducing target QPS.
			operation.recorder.add(0, false, -2)
			recorded.add(0, false, -2)
		}
		scheduled++
	}
	cancelKeeper()
	keeperWG.Wait()
	cancelConnectionKeepalive()
	connectionKeepaliveWG.Wait()
	for _, input := range jobs {
		close(input)
	}
	workers.Wait()
	wall := time.Since(measurementStarted)
	measured := summarize("gateway-mixed-"+model.Name, qps, scheduled, wall, recorded, ctx.Err() != nil)
	measured.StartedMS = measurementStarted.UnixMilli()
	measured.EndedMS = measurementStarted.Add(wall).UnixMilli()
	// Work is offered for exactly duration, then drained so queued outcomes are
	// not discarded. Keep both windows explicit in the result.
	measured.CompletionQPS = measured.ActualQPS
	measured.ActualQPS = float64(measured.Succeeded) / duration.Seconds()
	measured.MeasurementMillis = duration.Milliseconds()
	measured.DrainMillis = max((wall - duration).Milliseconds(), 0)
	measured.StateReadyMS = stateReadyAt.UnixMilli()
	measured.StateWindowMillis = max(measurementStarted.Sub(stateReadyAt).Milliseconds(), 0)
	measured.ResidentActorsTarget = residentActors
	measured.ResidentActorRefreshes = residentRefreshes.Load()
	measured.ResidentActorRefreshFailed = residentRefreshFailures.Load()
	measured.ConnectionKeepalives = connectionKeepalives.Load()
	measured.ConnectionKeepaliveFailed = connectionKeepaliveFailures.Load()
	if len(excluded) > 0 {
		measured.ExcludedOperations = make([]string, 0, len(excluded))
		for name := range excluded {
			measured.ExcludedOperations = append(measured.ExcludedOperations, name)
		}
		sort.Strings(measured.ExcludedOperations)
	}
	measured.WarmupMode = gatewayWarmupFull
	measured.Steps = make(map[string]stepResult, len(operations))
	for _, operation := range operations {
		target := float64(qps) * operation.ReferenceQPS / referenceTotal
		measured.Steps[operation.Name] = summarizeWeightedStep(&operation.recorder, target, duration)
	}
	return measured, nil
}

func runGatewayMixedPongKeepalive(
	ctx context.Context,
	registrations <-chan *gatewayBenchConnection,
	expectedConnections int,
	cycle time.Duration,
	succeeded, failed *atomic.Uint64,
) {
	if expectedConnections <= 0 || cycle <= 0 {
		return
	}
	// 每个 tick 推一批连接，而不是一个。逐连接推送要求的定时器频率随连接数线性
	// 上升——60000 条连接就是 2000 次/秒，而这个 select 还要和 registrations 抢
	// 就绪分支，实际轮完一圈远超 cycle。一旦超过 Gateway 的 90s 读超时，服务端
	// 就把连接判死，压测会误报成被测系统的容量上限。按批推送把定时器频率固定下来，
	// 与连接数无关。
	const tick = 500 * time.Millisecond
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	clients := make([]*gatewayBenchConnection, 0, expectedConnections)
	keepaliveJobs := make(chan *gatewayBenchConnection, 8192)
	var keepaliveWorkers sync.WaitGroup
	for range 32 {
		keepaliveWorkers.Add(1)
		go func() {
			defer keepaliveWorkers.Done()
			for client := range keepaliveJobs {
				if client == nil || client.conn == nil {
					failed.Add(1)
					continue
				}
				if err := client.conn.WriteControl(
					websocket.PongMessage,
					nil,
					time.Now().Add(time.Second),
				); err != nil {
					failed.Add(1)
				} else {
					succeeded.Add(1)
				}
			}
		}()
	}
	defer func() {
		close(keepaliveJobs)
		keepaliveWorkers.Wait()
	}()
	index := 0
	dispatched := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case client, open := <-registrations:
			if !open {
				registrations = nil
				continue
			}
			if client != nil {
				clients = append(clients, client)
			}
		case now := <-ticker.C:
			elapsed := now.Sub(dispatched)
			dispatched = now
			if len(clients) == 0 {
				continue
			}
			// 批量按实际流逝的时间算。建连阶段 registrations 一直就绪，select 会
			// 随机挤掉部分 tick，按固定批量推会让一圈拖长；按流逝时间推则自动补齐。
			batch := int(int64(len(clients)) * int64(elapsed) / int64(cycle))
			if batch < 1 {
				batch = 1
			}
			for range batch {
				client := clients[index%len(clients)]
				index++
				select {
				case keepaliveJobs <- client:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func waitForMeasurementStartFile(ctx context.Context, path string) (time.Time, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			milliseconds, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
			if parseErr != nil || milliseconds <= 0 {
				return time.Time{}, fmt.Errorf("parse measurement start file %s: %q", path, data)
			}
			return time.UnixMilli(milliseconds), nil
		}
		if !os.IsNotExist(err) {
			return time.Time{}, fmt.Errorf("read measurement start file: %w", err)
		}
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func prewarmExtraResidentActors(
	ctx context.Context,
	targets []gatewayAccount,
	timeProfile string,
	concurrency int,
) error {
	if len(targets) == 0 {
		return nil
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	concurrency = min(concurrency, len(targets))
	sem := make(chan struct{}, concurrency)
	errs := make(chan error, len(targets))
	var group sync.WaitGroup
	for index := range targets {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
			client, err := openGatewayBenchConnection(
				ctx, targets[index], "buy", timeProfile, gatewayWarmupFull,
			)
			if client != nil {
				_ = client.conn.Close()
			}
			if err != nil {
				errs <- err
			}
		}(index)
	}
	group.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return fmt.Errorf("prewarm extra resident Actors: %w", err)
	}
	return nil
}

func selectExtraResidentActors(
	accounts []gatewayAccount,
	localEnd, visitorEnd, wanted int,
) []gatewayAccount {
	if wanted <= 0 {
		return nil
	}
	known := make(map[string]struct{}, visitorEnd+wanted)
	for index := 0; index < localEnd; index++ {
		known[accounts[index].UID] = struct{}{}
	}
	for index := localEnd; index < visitorEnd; index++ {
		known[accounts[index].PeerUID] = struct{}{}
	}
	targets := make([]gatewayAccount, 0, wanted)
	for index := visitorEnd; index < len(accounts) && len(targets) < wanted; index++ {
		uid := accounts[index].UID
		if uid == "" || uid == "0" {
			continue
		}
		if _, duplicate := known[uid]; duplicate {
			continue
		}
		known[uid] = struct{}{}
		targets = append(targets, accounts[index])
	}
	return targets
}

func mixedAccountIndex(operation string, sequence uint64, poolSize int) int {
	if poolSize <= 1 {
		return 0
	}
	// Per-operation offsets prevent every low-frequency API from starting at
	// account zero. A coprime stride then walks the complete pool exactly once
	// before reuse, preserving one-shot fixture capacity without hot prefixes.
	var hash uint64 = 1469598103934665603
	for index := 0; index < len(operation); index++ {
		hash ^= uint64(operation[index])
		hash *= 1099511628211
	}
	stride := 7919 % poolSize
	if stride == 0 {
		stride = 1
	}
	for greatestCommonDivisor(stride, poolSize) != 1 {
		stride++
	}
	return (int(hash%uint64(poolSize)) + int(sequence%uint64(poolSize))*stride) % poolSize
}

func greatestCommonDivisor(left, right int) int {
	for right != 0 {
		left, right = right, left%right
	}
	return left
}

func selectMixedOperation(operations []*mixedBehaviorOperation, total float64, sequence uint64) *mixedBehaviorOperation {
	// SplitMix64 makes adjacent arrivals choose independent operations while the
	// global ticker still preserves a smooth aggregate arrival rate.
	x := sequence + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	x ^= x >> 31
	point := (float64(x>>11) / float64(uint64(1)<<53)) * total
	for _, operation := range operations {
		if point < operation.ReferenceQPS {
			return operation
		}
		point -= operation.ReferenceQPS
	}
	return operations[len(operations)-1]
}

func closeGatewayMixedClients(clients []*gatewayBenchConnection) {
	for _, client := range clients {
		if client != nil {
			_ = client.conn.Close()
		}
	}
}

func mixedOperationSupported(operation string) bool {
	switch operation {
	case "enter-self", "enter-friend", "sync",
		"till", "clear", "plant", "water-local", "weed-local", "pest-local", "fertilize", "harvest",
		"water-cross", "weed-cross", "pest-cross", "steal",
		"buy", "sell", "friend-list", "gen-share", "search-user", "list-friend-requests",
		"pet-status", "pet-activate", "pet-feed", "task-list", "task-claim",
		"mail-list", "mail-read", "mail-claim", "mail-delete", "codex-list":
		return true
	default:
		return false
	}
}

func (client *gatewayBenchConnection) mixedRequest(operation string, round int) clientwire.Envelope {
	request := clientwire.Envelope{ClientSeq: client.nextSeq}
	client.nextSeq++
	ownerUID := "0"
	if strings.HasSuffix(operation, "-cross") || operation == "steal" || operation == "enter-friend" {
		ownerUID = client.account.PeerUID
	}
	plotIndex := 0
	switch operation {
	case "water-local", "weed-local", "pest-local", "fertilize", "water-cross", "weed-cross", "pest-cross":
		plotIndex = round % 6
	case "harvest", "steal":
		plotIndex = 6 + round%5
	case "till":
		plotIndex = 11 + round%2
	case "clear":
		plotIndex = 13 + round%2
	case "plant":
		plotIndex = 15 + round%3
	}
	plotPayload := func(arg int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"owner_uid":%q,"plot_index":%d,"arg":%d}`, ownerUID, plotIndex, arg))
	}
	switch operation {
	case "enter-self":
		request.Cmd, request.Payload = gateway.CommandEnterFarm, json.RawMessage(`{"owner_uid":"0"}`)
	case "enter-friend":
		request.Cmd = gateway.CommandEnterFarm
		request.Payload = json.RawMessage(fmt.Sprintf(`{"owner_uid":%q}`, client.account.PeerUID))
	case "sync":
		request.Cmd = gateway.CommandSyncFarm
		request.Payload = json.RawMessage(fmt.Sprintf(`{"owner_uid":"0","from_seq":"%d"}`, client.farmSeq))
	case "till":
		request.Cmd, request.Payload = gateway.CommandTill, plotPayload(0)
	case "clear":
		request.Cmd, request.Payload = gateway.CommandClear, plotPayload(0)
	case "plant":
		request.Cmd, request.Payload = gateway.CommandPlant, plotPayload(max(client.account.CropID, 1))
	case "water-local", "water-cross":
		request.Cmd, request.Payload = gateway.CommandWater, plotPayload(0)
	case "weed-local", "weed-cross":
		request.Cmd, request.Payload = gateway.CommandRemoveWeed, plotPayload(0)
	case "pest-local", "pest-cross":
		request.Cmd, request.Payload = gateway.CommandRemovePest, plotPayload(0)
	case "fertilize":
		request.Cmd, request.Payload = gateway.CommandFertilize, plotPayload(1)
	case "harvest":
		request.Cmd, request.Payload = gateway.CommandHarvest, plotPayload(0)
	case "steal":
		request.Cmd = gateway.CommandSteal
		request.Payload = json.RawMessage(fmt.Sprintf(`{"owner_uid":%q,"plot_index":%d,"crop_id":%d}`, ownerUID, plotIndex, max(client.account.CropID, 1)))
	case "buy":
		request.Cmd, request.Payload = gateway.CommandBuy, json.RawMessage(`{"item_id":1,"quantity":1}`)
	case "sell":
		request.Cmd, request.Payload = gateway.CommandSell, json.RawMessage(`{"item_id":1,"quantity":1}`)
	case "friend-list":
		request.Cmd, request.Payload = gateway.CommandFriendList, json.RawMessage(`{}`)
	case "gen-share":
		request.Cmd, request.Payload = gateway.CommandGenShareLink, json.RawMessage(`{}`)
	case "search-user":
		request.Cmd = gateway.CommandSearchUser
		request.Payload = json.RawMessage(fmt.Sprintf(`{"username":%q}`, client.searchUsername))
	case "list-friend-requests":
		request.Cmd, request.Payload = gateway.CommandListFriendRequests, json.RawMessage(`{}`)
	case "pet-status":
		request.Cmd, request.Payload = gateway.CommandPetStatus, json.RawMessage(`{}`)
	case "pet-activate":
		request.Cmd, request.Payload = gateway.CommandPetActivate, json.RawMessage(`{"dog_type":2}`)
	case "pet-feed":
		request.Cmd, request.Payload = gateway.CommandPetFeed, json.RawMessage(`{"grams":1}`)
	case "task-list":
		request.Cmd, request.Payload = gateway.CommandTaskList, json.RawMessage(`{}`)
	case "task-claim":
		taskIDs := client.account.TaskIDs
		if len(taskIDs) == 0 {
			taskIDs = []int{4}
		}
		request.Cmd = gateway.CommandTaskClaim
		request.Payload = json.RawMessage(fmt.Sprintf(`{"task_id":%d}`, taskIDs[round%len(taskIDs)]))
	case "mail-list":
		request.Cmd, request.Payload = gateway.CommandMailList, json.RawMessage(`{}`)
	case "mail-read":
		request.Cmd, request.Payload = gateway.CommandMailRead, mixedMailPayload(client.account, client.account.MailReadID, 1)
	case "mail-claim":
		request.Cmd, request.Payload = gateway.CommandMailClaim, mixedMailPayload(client.account, client.account.MailClaimID, 2)
	case "mail-delete":
		request.Cmd, request.Payload = gateway.CommandMailDelete, mixedMailPayload(client.account, client.account.MailDeleteID, 3)
	case "codex-list":
		request.Cmd, request.Payload = gateway.CommandCodexList, json.RawMessage(`{}`)
	}
	return request
}

func mixedMailPayload(account gatewayAccount, primary string, offset uint64) json.RawMessage {
	if primary == "" {
		if uid, err := strconv.ParseUint(account.UID, 10, 64); err == nil && uid <= (^uint64(0)-offset)/10 {
			primary = strconv.FormatUint(uid*10+offset, 10)
		}
	}
	if primary == "" {
		primary = account.MailID
		if primary == "" {
			primary = "1"
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"mail_id":%q,"all":false}`, primary))
}
