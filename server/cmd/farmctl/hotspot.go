package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"

	_ "github.com/go-sql-driver/mysql"
	"google.golang.org/grpc/status"
)

const (
	hotspotMaxVisitors = 10_000
	hotspotSQLChunk    = 500
)

type hotspotOutcome struct {
	latency time.Duration
	code    int32
	rpcErr  string
	ackErr  string
}

// runHotspot 是隔离的本地极限压测：它绕过 200 好友玩法上限，直接预置临时
// friendship 行，并通过 farmsvr gRPC 让最多一万名虚拟访客同时裁决同一农场。
//
// 虚拟访客没有账号和 WebSocket，因此该工具只测 owner Actor、好友鉴权、组提交、
// outbox 与 ack，不代表万人广播，也不测 visitor settle。
func runHotspot(args []string, defaultBaseURL string) error {
	flags := flag.NewFlagSet("hotspot", flag.ContinueOnError)
	visitors := flags.Int("visitors", 1_000, "同时冲击一个农场的虚拟访客数")
	baseURL := flags.String("base-url", defaultBaseURL, "本地 Gateway HTTP 地址")
	farmTarget := flags.String("farm-grpc", "127.0.0.1:9210", "本地 farmsvr gRPC 地址")
	mysqlDSN := flags.String("mysql-dsn",
		"farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local",
		"本地 MySQL DSN，仅用于预置和清理临时好友",
	)
	internalToken := flags.String("internal-token",
		getenv("FARM_INTERNAL_TOKEN", "dev-only-internal-token-change-me"),
		"服务间 gRPC token",
	)
	timeout := flags.Duration("timeout", 30*time.Second, "每条 ApplyCrossAction 的超时")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *visitors < 1 || *visitors > hotspotMaxVisitors {
		return fmt.Errorf("-visitors 必须在 1..%d 之间，当前 %d", hotspotMaxVisitors, *visitors)
	}
	if *timeout <= 0 {
		return errors.New("-timeout 必须为正")
	}
	if !hotspotLocalOnly(*baseURL, *farmTarget, *mysqlDSN) {
		return errors.New("hotspot 只允许连接 127.0.0.1/localhost，拒绝对远端环境预置测试数据")
	}

	owner, err := newBenchPlayer(*baseURL)
	if err != nil {
		return fmt.Errorf("创建主人: %w", err)
	}
	scenario := &benchScenario{
		baseURL: *baseURL,
		owner:   owner,
		targets: benchTargetPlots(gameconfig.MaxPlots, gameconfig.MaxPlots),
	}
	defer scenario.close()
	if err := unlockOwnerPlotsForBench(owner.login.UID, gameconfig.MaxPlots); err != nil {
		return fmt.Errorf("解锁主人地块: %w", err)
	}
	if err := scenario.replant(); err != nil {
		return fmt.Errorf("种植主人农场: %w", err)
	}
	if err := scenario.prepareRound(); err != nil {
		return fmt.Errorf("推进主人农场到缺水状态: %w", err)
	}

	db, err := sql.Open("mysql", *mysqlDSN)
	if err != nil {
		return fmt.Errorf("打开本地 MySQL: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := db.PingContext(ctx); err != nil {
		cancel()
		return fmt.Errorf("连接本地 MySQL: %w", err)
	}
	cancel()

	visitorBase := hotspotVisitorBase()
	visitorLast := visitorBase + uint64(*visitors) - 1
	if owner.login.UID >= visitorBase {
		return fmt.Errorf("主人 uid=%d 与虚拟访客区间冲突", owner.login.UID)
	}
	if err := seedHotspotFriendships(db, owner.login.UID, visitorBase, *visitors); err != nil {
		return err
	}
	defer cleanupHotspotRows(db, owner.login.UID, visitorBase, visitorLast)

	pool := grpcx.NewPool(*internalToken)
	defer pool.Close()
	connCtx, connCancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := pool.Conn(connCtx, *farmTarget)
	connCancel()
	if err != nil {
		return fmt.Errorf("连接 farmsvr gRPC: %w", err)
	}
	client := farmv1.NewCrossFarmServiceClient(conn)

	fmt.Printf("hotspot: owner_uid=%d virtual_visitors=%d range=%d..%d\n",
		owner.login.UID, *visitors, visitorBase, visitorLast)
	fmt.Println("hotspot: 虚拟好友已预置；同时发射 ApplyCrossAction")

	outcomes, elapsed := fireHotspot(
		client,
		owner.login.UID,
		visitorBase,
		*visitors,
		*timeout,
	)
	for _, line := range renderHotspot(outcomes, elapsed) {
		fmt.Println(line)
	}
	return nil
}

func fireHotspot(
	client farmv1.CrossFarmServiceClient,
	ownerUID uint64,
	visitorBase uint64,
	visitors int,
	timeout time.Duration,
) ([]hotspotOutcome, time.Duration) {
	outcomes := make([]hotspotOutcome, visitors)
	reqBase := uint64(time.Now().UnixNano())
	gun := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(visitors)
	done.Add(visitors)

	for i := 0; i < visitors; i++ {
		go func(i int) {
			defer done.Done()
			reqID := reqBase + uint64(i)
			visitorUID := visitorBase + uint64(i)
			request := &farmv1.ApplyCrossActionRequest{Action: &farmv1.CrossAction{
				ReqId:      reqID,
				Kind:       farmv1.CrossActionKind_CROSS_ACTION_KIND_WATER,
				VisitorUid: visitorUID,
				OwnerUid:   ownerUID,
				PlotIndex:  uint32(i % gameconfig.MaxPlots),
			}}
			ready.Done()
			<-gun

			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			response, err := client.ApplyCrossAction(ctx, request)
			cancel()
			outcomes[i].latency = time.Since(started)
			if err != nil {
				if grpcStatus, ok := status.FromError(err); ok {
					outcomes[i].rpcErr = grpcStatus.Code().String()
				} else {
					outcomes[i].rpcErr = err.Error()
				}
				return
			}
			if response == nil || response.Result == nil {
				outcomes[i].rpcErr = "empty_response"
				return
			}
			outcomes[i].code = response.Result.Code

			ackCtx, ackCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, ackErr := client.AcknowledgeCrossResult(ackCtx, &farmv1.AcknowledgeCrossResultRequest{
				OwnerUid:   ownerUID,
				VisitorUid: visitorUID,
				ReqId:      reqID,
			})
			ackCancel()
			if ackErr != nil {
				if grpcStatus, ok := status.FromError(ackErr); ok {
					outcomes[i].ackErr = grpcStatus.Code().String()
				} else {
					outcomes[i].ackErr = ackErr.Error()
				}
			}
		}(i)
	}

	ready.Wait()
	started := time.Now()
	close(gun)
	done.Wait()
	return outcomes, time.Since(started)
}

func renderHotspot(outcomes []hotspotOutcome, elapsed time.Duration) []string {
	latencies := make([]time.Duration, 0, len(outcomes))
	codes := make(map[string]int)
	ackFailures := make(map[string]int)
	rpcErrors := 0
	ackErrors := 0
	for _, outcome := range outcomes {
		latencies = append(latencies, outcome.latency)
		if outcome.rpcErr != "" {
			rpcErrors++
			codes["grpc:"+outcome.rpcErr]++
		} else {
			codes[strconv.Itoa(int(outcome.code))]++
		}
		if outcome.ackErr != "" {
			ackErrors++
			ackFailures[outcome.ackErr]++
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	labels := make([]string, 0, len(codes))
	for label := range codes {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	distribution := make([]string, 0, len(labels))
	for _, label := range labels {
		distribution = append(distribution, fmt.Sprintf("%s=%d", label, codes[label]))
	}
	qps := 0.0
	if elapsed > 0 {
		qps = float64(len(outcomes)) / elapsed.Seconds()
	}
	lines := []string{
		fmt.Sprintf("hotspot 汇总: 样本=%d RPC错误=%d ACK错误=%d 突发窗口=%.1fms 吞吐=%.1f req/s",
			len(outcomes), rpcErrors, ackErrors, benchMillis(elapsed), qps),
		benchLatencyLine(latencies),
		"结果分布: " + strings.Join(distribution, "  "),
	}
	if len(ackFailures) > 0 {
		labels = labels[:0]
		for label := range ackFailures {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		distribution = distribution[:0]
		for _, label := range labels {
			distribution = append(distribution, fmt.Sprintf("%s=%d", label, ackFailures[label]))
		}
		lines = append(lines, "ACK错误分布: "+strings.Join(distribution, "  "))
	}
	return lines
}

func seedHotspotFriendships(db *sql.DB, ownerUID, visitorBase uint64, visitors int) error {
	now := time.Now().UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for start := 0; start < visitors; start += hotspotSQLChunk {
		end := start + hotspotSQLChunk
		if end > visitors {
			end = visitors
		}
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*3)
		for i := start; i < end; i++ {
			values = append(values, "(?, ?, ?)")
			args = append(args, ownerUID, visitorBase+uint64(i), now)
		}
		query := "INSERT IGNORE INTO friendship (uid_lo, uid_hi, created_at) VALUES " +
			strings.Join(values, ",")
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("预置虚拟好友 %d..%d: %w", start, end-1, err)
		}
	}
	return nil
}

func cleanupHotspotRows(db *sql.DB, ownerUID, visitorBase, visitorLast uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx,
		"DELETE FROM friendship WHERE uid_lo = ? AND uid_hi BETWEEN ? AND ?",
		ownerUID, visitorBase, visitorLast,
	)
	_, _ = db.ExecContext(ctx,
		"DELETE FROM farm_outbox WHERE producer_uid = ? AND target_uid BETWEEN ? AND ?",
		ownerUID, visitorBase, visitorLast,
	)
}

func hotspotVisitorBase() uint64 {
	return 8_000_000_000_000_000_000 + uint64(time.Now().UnixNano()%1_000_000_000)*20_000
}

func hotspotLocalOnly(baseURL, farmTarget, mysqlDSN string) bool {
	local := func(value string) bool {
		return strings.Contains(value, "127.0.0.1") || strings.Contains(value, "localhost")
	}
	return local(baseURL) && local(farmTarget) && local(mysqlDSN)
}
