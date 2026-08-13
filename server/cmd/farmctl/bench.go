package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/domain/farm"
	"farm/server/gateway"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
)

const (
	benchCropID = 1 // 白萝卜：0 级解锁、最便宜，补种成本最低

	benchDefaultVisitors = 1
	benchDefaultRounds   = 30
	benchDefaultWarmup   = 3

	// benchMaxVisitors 卡在好友上限 200 之下：每个访客都要先加主人为好友。
	benchMaxVisitors = 190

	// benchResponseTimeout 必须大于 crossfarm.PendingTimeout(5s)，否则客户端会先于
	// 服务端判超时，把一次「服务端已回 1004」记成本地读超时，错误分布就失真了。
	benchResponseTimeout = 8 * time.Second

	// Gateway 对单条连接限流（容量 20、10 QPS）。主人在轮间要连发几十条整备命令，
	// 撞上限流是常态，退避重试而不是让压测中断。
	benchRateLimitRetries = 24
	benchRateLimitBackoff = 250 * time.Millisecond

	// benchDryMargin 是推进时钟时额外多给的毫秒数。水分窗口的判定用的是服务端
	// 时钟，而调时到浇水命令真正提交之间还会流逝一段真实时间；不留余量会卡在
	// 「刚好差几毫秒」的边界上被判 AlreadyWatered。
	benchDryMargin int64 = 2000

	// benchFailureWarnRatio 超过这个失败率就说明场景编排本身有问题，数据不可信。
	benchFailureWarnRatio = 0.05

	// 上一轮有失败时，先等主人农场不再变动再开下一轮。上限取得比
	// farm.CrossPendingTimeout(10s) 略大，保证最迟的那条裁决也已落地或被回滚。
	benchQuiesceInterval = 600 * time.Millisecond
	benchQuiesceMaxWait  = 12 * time.Second

	benchResponseBuffer = 32
)

var errBenchResponseTimeout = errors.New("等待应答超时")

// runBench 测量 N 个访客并发对同一主人农场浇水时的端到端延迟。
//
// 它用于回归跨农场 gRPC、Actor 裁决、组提交、访客结算与客户端应答的完整链路。
// -unlock-plots 只供本地 Docker 压测：扩到 MaxPlots，让至多 18 个访客各浇一块地。
func runBench(args []string, defaultBaseURL string) error {
	flags := flag.NewFlagSet("bench", flag.ContinueOnError)
	visitorCount := flags.Int("visitors", benchDefaultVisitors, "并发访客数")
	rounds := flags.Int("rounds", benchDefaultRounds, "计入统计的轮数")
	warmup := flags.Int("warmup", benchDefaultWarmup, "预热轮数，不计入统计")
	baseURL := flags.String("base-url", defaultBaseURL, "网关地址")
	unlockPlots := flags.Bool("unlock-plots", false, "本地 Docker 压测时把主人地块扩到 min(访客数, MaxPlots)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *visitorCount < 1 || *visitorCount > benchMaxVisitors {
		return fmt.Errorf("-visitors 必须在 1..%d 之间，当前 %d", benchMaxVisitors, *visitorCount)
	}
	if *rounds < 1 {
		return fmt.Errorf("-rounds 必须 >= 1，当前 %d", *rounds)
	}
	if *warmup < 0 {
		return fmt.Errorf("-warmup 不能为负，当前 %d", *warmup)
	}

	scenario, err := benchSetup(*baseURL, *visitorCount, *unlockPlots)
	if err != nil {
		return err
	}
	defer scenario.close()

	totalRounds := *warmup + *rounds
	stats := newBenchStats(*visitorCount)
	previousRoundFailed := false
	for round := 1; round <= totalRounds; round++ {
		if previousRoundFailed {
			if err := scenario.awaitQuiesce(); err != nil {
				return fmt.Errorf("第 %d 轮等待上一轮收敛: %w", round, err)
			}
		}
		if err := scenario.prepareRound(); err != nil {
			return fmt.Errorf("第 %d 轮整备农场: %w", round, err)
		}
		outcomes := benchFireRound(scenario.visitors, scenario.owner.login.UID)
		previousRoundFailed = benchHasFailure(outcomes)
		warming := round <= *warmup
		if !warming {
			stats.addAll(outcomes)
		}
		fmt.Println(benchRoundLine(round, totalRounds, warming, outcomes))
	}

	fmt.Println()
	fmt.Println("bench 汇总:")
	for _, line := range stats.render() {
		fmt.Println(line)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 连接与收发
// ---------------------------------------------------------------------------

// benchPlayer 是一条带常驻读协程的玩家连接。
//
// 压测必须给每个访客独立连接：Gateway 按连接限流（容量 20、10 QPS），复用一条连接
// 发 N 个请求会先撞限流，测到的就不是主人侧的排队时间了。读写各自固定在一个 goroutine
// 里，符合 gorilla/websocket「同时至多一个读者、一个写者」的约束。
type benchPlayer struct {
	login authResponse
	conn  *websocket.Conn

	// seq 只由发起请求的一方访问：同一玩家同一时刻只有一个在途请求。
	seq uint32

	// resp 只投递带 client_seq 的应答；服务端推送（FarmDelta 等 client_seq=0）
	// 由读协程直接丢弃，从而不会在连接上堆积、也不会挤进被测的那一次读。
	resp chan clientwire.Envelope

	dead    chan struct{}
	readErr error
}

func newBenchPlayer(baseURL string) (*benchPlayer, error) {
	username, err := smokeUsername()
	if err != nil {
		return nil, err
	}
	if _, err := authenticate(baseURL+"/api/register", username, smokePassword); err != nil {
		return nil, fmt.Errorf("注册 %s: %w", username, err)
	}
	login, err := authenticate(baseURL+"/api/login", username, smokePassword)
	if err != nil {
		return nil, fmt.Errorf("登录 %s: %w", username, err)
	}
	conn, err := dialAndHandshake(login)
	if err != nil {
		return nil, fmt.Errorf("连接 %s: %w", username, err)
	}
	player := &benchPlayer{
		login: login,
		conn:  conn,
		seq:   2, // 握手占用了 client_seq=1
		resp:  make(chan clientwire.Envelope, benchResponseBuffer),
		dead:  make(chan struct{}),
	}
	go player.readLoop()
	return player, nil
}

func (p *benchPlayer) readLoop() {
	defer close(p.dead)
	for {
		messageType, frame, err := p.conn.ReadMessage()
		if err != nil {
			p.readErr = err
			return
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		envelopes, err := clientwire.DecodeBinaryBatch(frame)
		if err != nil {
			continue
		}
		for _, envelope := range envelopes {
			if envelope.ClientSeq == 0 {
				continue
			}
			select {
			case p.resp <- envelope:
			default:
				// 缓冲满只可能是迟到应答堆积；丢弃它们，绝不让读协程停下来。
			}
		}
	}
}

// await 等待与 cmd/seq 配对的应答，沿途丢弃上一次请求的迟到应答。
func (p *benchPlayer) await(cmd, seq uint32, timeout time.Duration) (clientwire.Envelope, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case envelope := <-p.resp:
			if envelope.Cmd == cmd && envelope.ClientSeq == seq {
				return envelope, nil
			}
		case <-p.dead:
			return clientwire.Envelope{}, fmt.Errorf("连接已断开: %w", p.readErr)
		case <-timer.C:
			return clientwire.Envelope{}, errBenchResponseTimeout
		}
	}
}

// benchExchange 发一条命令并等应答，遇限流退避重试。不校验 Err，便于调用方分支。
func benchExchange(p *benchPlayer, cmd uint32, payload map[string]any) (clientwire.Envelope, error) {
	for attempt := 0; attempt < benchRateLimitRetries; attempt++ {
		seq := p.seq
		p.seq++
		frame, err := clientwire.EncodeBinaryBatch([]clientwire.Envelope{{
			Cmd:       cmd,
			ClientSeq: seq,
			Payload:   mustJSON(payload),
		}})
		if err != nil {
			return clientwire.Envelope{}, fmt.Errorf("编码 cmd=%d: %w", cmd, err)
		}
		if err := p.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return clientwire.Envelope{}, fmt.Errorf("发送 cmd=%d: %w", cmd, err)
		}
		envelope, err := p.await(cmd, seq, benchResponseTimeout)
		if err != nil {
			return clientwire.Envelope{}, fmt.Errorf("等待 cmd=%d 应答: %w", cmd, err)
		}
		if envelope.Err == errcode.RateLimited {
			time.Sleep(benchRateLimitBackoff)
			continue
		}
		return envelope, nil
	}
	return clientwire.Envelope{}, fmt.Errorf("cmd=%d 连续 %d 次被限流", cmd, benchRateLimitRetries)
}

func benchMustExchange(p *benchPlayer, cmd uint32, payload map[string]any) (clientwire.Envelope, error) {
	envelope, err := benchExchange(p, cmd, payload)
	if err != nil {
		return clientwire.Envelope{}, err
	}
	if envelope.Err != errcode.OK {
		return clientwire.Envelope{}, fmt.Errorf("cmd=%d 返回错误码 %d", cmd, envelope.Err)
	}
	return envelope, nil
}

func benchAction(p *benchPlayer, cmd uint32, payload map[string]any) (actionPayload, error) {
	envelope, err := benchMustExchange(p, cmd, payload)
	if err != nil {
		return actionPayload{}, err
	}
	var out actionPayload
	if err := json.Unmarshal(envelope.Payload, &out); err != nil {
		return actionPayload{}, fmt.Errorf("解析 cmd=%d 应答: %w", cmd, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 场景编排
// ---------------------------------------------------------------------------

type benchVisitor struct {
	player *benchPlayer
	index  int
	plot   uint32
}

type benchScenario struct {
	baseURL  string
	owner    *benchPlayer
	visitors []*benchVisitor
	// targets 是本次压测会被浇的地块，按索引升序且互不重复。
	targets []uint32
}

func (s *benchScenario) close() {
	for _, v := range s.visitors {
		_ = v.player.conn.Close()
	}
	if s.owner != nil {
		_ = s.owner.conn.Close()
	}
}

func benchSetup(baseURL string, visitorCount int, unlockPlots bool) (*benchScenario, error) {
	owner, err := newBenchPlayer(baseURL)
	if err != nil {
		return nil, fmt.Errorf("创建主人: %w", err)
	}
	scenario := &benchScenario{baseURL: baseURL, owner: owner}
	if unlockPlots && visitorCount > gameconfig.InitialUnlockedPlots {
		want := visitorCount
		if want > gameconfig.MaxPlots {
			want = gameconfig.MaxPlots
		}
		if err := unlockOwnerPlotsForBench(owner.login.UID, want); err != nil {
			scenario.close()
			return nil, fmt.Errorf("将主人扩地到 %d: %w", want, err)
		}
	}

	enter, err := benchOwnerSnapshot(owner)
	if err != nil {
		scenario.close()
		return nil, fmt.Errorf("主人进入自己农场: %w", err)
	}
	plotCount := int(enter.Snapshot.UnlockedPlots)
	if plotCount <= 0 {
		scenario.close()
		return nil, fmt.Errorf("主人已解锁地块数为 %d", plotCount)
	}
	scenario.targets = benchTargetPlots(visitorCount, plotCount)

	fmt.Printf("bench: 主人 uid=%d 访客数=%d 已解锁地块=%d 本轮使用地块=%d\n",
		owner.login.UID, visitorCount, plotCount, len(scenario.targets))
	if visitorCount > plotCount {
		// 一块地在一个水分窗口内只接受一次浇水，所以「一人一块地」最多支撑
		// plotCount 个并发成功请求。多出来的访客会命中同一块地并拿到 1211：
		// 这类请求同样完整走完主人 Actor 的串行区与同步落盘，但按要求不计入
		// 成功延迟统计。
		fmt.Printf("bench: 注意，访客数 %d 超过已解锁地块数 %d，超出的访客会与人共用地块并返回 1211（AlreadyWatered）\n",
			visitorCount, plotCount)
	}

	for i := 0; i < visitorCount; i++ {
		player, err := newBenchPlayer(baseURL)
		if err != nil {
			scenario.close()
			return nil, fmt.Errorf("创建访客[%d]: %w", i, err)
		}
		scenario.visitors = append(scenario.visitors, &benchVisitor{
			player: player,
			index:  i,
			plot:   benchPlotForVisitor(i, plotCount),
		})
		if _, err := benchMustExchange(player, gateway.CommandAddFriendByUID, map[string]any{
			"peer_uid": strconv.FormatUint(owner.login.UID, 10),
		}); err != nil {
			scenario.close()
			return nil, fmt.Errorf("访客[%d] 加主人为好友: %w", i, err)
		}
	}
	fmt.Printf("bench: %d 个访客已连接并与主人互为好友\n", visitorCount)

	// 先把地种上，访客进农场时看到的就是最终形态。
	if err := scenario.replant(); err != nil {
		scenario.close()
		return nil, fmt.Errorf("初始化主人农场: %w", err)
	}
	for _, v := range scenario.visitors {
		if err := benchVisitorEnter(v, owner.login.UID); err != nil {
			scenario.close()
			return nil, fmt.Errorf("访客[%d] 进入主人农场: %w", v.index, err)
		}
	}
	fmt.Printf("bench: %d 个访客已进入主人农场，开始压测\n", visitorCount)
	return scenario, nil
}

// unlockOwnerPlotsForBench 是本地 Docker 场景的压测旁路。扩地玩法尚未实现，
// 因此在主人 Actor 首次加载前修改 MySQL 权威值并清掉 Redis 缓存。
func unlockOwnerPlotsForBench(ownerUID uint64, want int) error {
	if ownerUID == 0 || want < 1 || want > gameconfig.MaxPlots {
		return fmt.Errorf("uid=%d want=%d", ownerUID, want)
	}
	composeFile := "deploy/compose.yml"
	if _, err := os.Stat(composeFile); err != nil {
		composeFile = "../deploy/compose.yml"
	}
	query := fmt.Sprintf(
		"UPDATE player SET unlocked_plots = GREATEST(unlocked_plots, %d), "+
			"coin = GREATEST(coin, 100000) WHERE uid = %d",
		want, ownerUID,
	)
	mysql := exec.Command(
		"docker", "compose", "-f", composeFile, "exec", "-T",
		"-e", "MYSQL_PWD=farm",
		"mysql", "mysql", "--protocol=TCP", "-h127.0.0.1", "-ufarm", "farm",
		"-e", query,
	)
	if output, err := mysql.CombinedOutput(); err != nil {
		return fmt.Errorf("mysql: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	redis := exec.Command(
		"docker", "compose", "-f", composeFile, "exec", "-T",
		"redis", "redis-cli", "DEL", "farm:"+strconv.FormatUint(ownerUID, 10),
	)
	if output, err := redis.CombinedOutput(); err != nil {
		return fmt.Errorf("redis: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func benchVisitorEnter(v *benchVisitor, ownerUID uint64) error {
	envelope, err := benchMustExchange(v.player, gateway.CommandEnterFarm, map[string]any{
		"owner_uid": strconv.FormatUint(ownerUID, 10),
	})
	if err != nil {
		return err
	}
	var enter enterFarmResponse
	if err := json.Unmarshal(envelope.Payload, &enter); err != nil {
		return fmt.Errorf("解析 EnterFarm 应答: %w", err)
	}
	if enter.Relation != "FRIEND" {
		return fmt.Errorf("relation = %q，期望 FRIEND", enter.Relation)
	}
	return nil
}

func benchOwnerSnapshot(owner *benchPlayer) (enterFarmResponse, error) {
	envelope, err := benchMustExchange(owner, gateway.CommandEnterFarm, map[string]any{
		"owner_uid": "0",
	})
	if err != nil {
		return enterFarmResponse{}, err
	}
	var enter enterFarmResponse
	if err := json.Unmarshal(envelope.Payload, &enter); err != nil {
		return enterFarmResponse{}, fmt.Errorf("解析 EnterFarm 应答: %w", err)
	}
	if enter.ServerTime == 0 {
		return enterFarmResponse{}, errors.New("EnterFarm 未返回 server_time，无法据此规划调时")
	}
	return enter, nil
}

// awaitQuiesce 等上一轮的迟到裁决全部落到主人农场，再开下一轮。
//
// Gateway 只等 5 秒就回客户端 1004，但那条动作还在主人侧的队列里没被处理完。不等
// 它落完就开下一轮，上一轮的浇水会抢在下一轮请求之前把地浇了，下一轮就被判 1211。
// 那是压测工具自己制造的失败，会把被测系统的错误分布搅浑。
//
// 收敛信号取目标地块的 last_water_at：它只被真正的浇水改写，不像 farm_seq 那样会被
// 时间推进带来的健康度漂移干扰。
func (s *benchScenario) awaitQuiesce() error {
	previous, err := s.waterMarks()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(benchQuiesceMaxWait)
	for {
		time.Sleep(benchQuiesceInterval)
		current, err := s.waterMarks()
		if err != nil {
			return err
		}
		if benchMarksEqual(previous, current) {
			return nil
		}
		if time.Now().After(deadline) {
			// 超过上限仍在变动就照常继续：与其在这里卡死，不如让失败率如实进入
			// 汇总，由那条显著警告告诉使用者这批数据不可信。
			return nil
		}
		previous = current
	}
}

func (s *benchScenario) waterMarks() ([]int64, error) {
	enter, err := benchOwnerSnapshot(s.owner)
	if err != nil {
		return nil, err
	}
	return benchWaterMarks(enter.Snapshot.Plots, s.targets)
}

// prepareRound 把目标地块整备成「生长中且缺水」，让本轮每个请求都能真正走完
// 主人的裁决与落盘，而不是被「已浇水」之类的前置校验挡掉。
func (s *benchScenario) prepareRound() error {
	enter, err := benchOwnerSnapshot(s.owner)
	if err != nil {
		return err
	}
	plan, err := benchPlanRound(enter.Snapshot.Plots, s.targets, enter.ServerTime, benchDryMargin)
	if err != nil {
		return err
	}
	if plan.Replant {
		// 一季白萝卜只够浇两次水（水分持续 35% 季长），所以每两轮就要重新种一遍。
		// 补种前先把在生长的作物推到成熟再收获，果实卖掉就是下一轮的种子钱，
		// 主人的金币因此能自给自足，不会跑几十轮后余额见底。
		if plan.AdvanceToMature > 0 {
			if err := debugAdvance(s.baseURL, plan.AdvanceToMature); err != nil {
				return fmt.Errorf("推进到成熟: %w", err)
			}
		}
		if err := s.replant(); err != nil {
			return err
		}
		enter, err = benchOwnerSnapshot(s.owner)
		if err != nil {
			return err
		}
		plan, err = benchPlanRound(enter.Snapshot.Plots, s.targets, enter.ServerTime, benchDryMargin)
		if err != nil {
			return err
		}
		if plan.Replant {
			return errors.New("补种后目标地块仍不是生长中状态")
		}
	}
	if plan.Advance > 0 {
		if err := debugAdvance(s.baseURL, plan.Advance); err != nil {
			return fmt.Errorf("推进到缺水: %w", err)
		}
	}
	return nil
}

// replant 把全部目标地块恢复成刚种下的生长态。
func (s *benchScenario) replant() error {
	enter, err := benchOwnerSnapshot(s.owner)
	if err != nil {
		return err
	}
	plots := enter.Snapshot.Plots
	for _, idx := range s.targets {
		if int(idx) >= len(plots) {
			return fmt.Errorf("快照缺少地块 %d", idx)
		}
	}

	fruitKey := string(farm.FruitItem(benchCropID))
	var fruitOnHand uint32
	harvested := false
	for _, idx := range s.targets {
		if plots[idx].State != farm.StateMature {
			continue
		}
		patch, err := benchAction(s.owner, gateway.CommandHarvest, benchPlotPayload(idx, 0))
		if err != nil {
			return fmt.Errorf("收获地块 %d: %w", idx, err)
		}
		// warehouse_changes carries the authoritative final count for the
		// harvested key, so the last harvest still yields the current total.
		fruitOnHand = patch.Patch.WarehouseChanges[fruitKey]
		harvested = true
	}
	if harvested && fruitOnHand > 0 {
		if _, err := benchAction(s.owner, gateway.CommandSell, map[string]any{
			"item_id":  benchCropID,
			"quantity": fruitOnHand,
		}); err != nil {
			return fmt.Errorf("出售 %d 个果实: %w", fruitOnHand, err)
		}
	}
	if _, err := benchAction(s.owner, gateway.CommandBuy, map[string]any{
		"item_id":  benchCropID,
		"quantity": len(s.targets),
	}); err != nil {
		return fmt.Errorf("购买 %d 份种子: %w", len(s.targets), err)
	}
	for _, idx := range s.targets {
		cmd, arg, needed := benchTillAction(plots[idx].State)
		if needed {
			if _, err := benchAction(s.owner, cmd, benchPlotPayload(idx, arg)); err != nil {
				return fmt.Errorf("整理地块 %d（状态 %d）: %w", idx, plots[idx].State, err)
			}
		}
		if _, err := benchAction(s.owner, gateway.CommandPlant, benchPlotPayload(idx, benchCropID)); err != nil {
			return fmt.Errorf("种植地块 %d: %w", idx, err)
		}
	}
	return nil
}

func benchPlotPayload(plotIndex uint32, arg uint32) map[string]any {
	return map[string]any{
		"owner_uid":  "0",
		"plot_index": plotIndex,
		"arg":        arg,
	}
}

// ---------------------------------------------------------------------------
// 并发发射与计时
// ---------------------------------------------------------------------------

type benchOutcome struct {
	latency time.Duration
	code    errcode.Code
	// transport 记录没走到协议层的失败（写失败、连接断开、本地读超时）。
	transport string
}

func (o benchOutcome) ok() bool {
	return o.transport == "" && o.code == errcode.OK
}

func benchHasFailure(outcomes []benchOutcome) bool {
	for _, outcome := range outcomes {
		if !outcome.ok() {
			return true
		}
	}
	return false
}

// benchFireRound 让所有访客同时发出浇水请求。
//
// ready 保证每个 goroutine 都已就绪（连帧都编好了）才鸣枪，close(gun) 才是真正的
// 同时放行；少了这道栅栏，先被调度到的 goroutine 会比后面的早出发几毫秒，测出来的
// 就不是并发而是接近串行的发射。
func benchFireRound(visitors []*benchVisitor, ownerUID uint64) []benchOutcome {
	outcomes := make([]benchOutcome, len(visitors))
	gun := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(len(visitors))
	done.Add(len(visitors))
	for i := range visitors {
		go func(i int) {
			defer done.Done()
			outcomes[i] = visitors[i].waterOnce(ownerUID, &ready, gun)
		}(i)
	}
	ready.Wait()
	close(gun)
	done.Wait()
	return outcomes
}

// waterOnce 计一次端到端耗时：从写出 WS 帧到读到同 client_seq 的应答。
// 建连、加好友、EnterFarm 这些一次性开销都在计时窗口之外。
func (v *benchVisitor) waterOnce(ownerUID uint64, ready *sync.WaitGroup, gun <-chan struct{}) benchOutcome {
	seq := v.player.seq
	v.player.seq++
	frame, encodeErr := clientwire.EncodeBinaryBatch([]clientwire.Envelope{{
		Cmd:       gateway.CommandWater,
		ClientSeq: seq,
		Payload: mustJSON(map[string]any{
			"owner_uid":  strconv.FormatUint(ownerUID, 10),
			"plot_index": v.plot,
			"arg":        0,
		}),
	}})
	ready.Done()
	<-gun
	if encodeErr != nil {
		return benchOutcome{transport: "编码请求失败: " + encodeErr.Error()}
	}

	sentAt := time.Now()
	if err := v.player.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		return benchOutcome{latency: time.Since(sentAt), transport: "写出请求失败: " + err.Error()}
	}
	envelope, err := v.player.await(gateway.CommandWater, seq, benchResponseTimeout)
	elapsed := time.Since(sentAt)
	if err != nil {
		return benchOutcome{latency: elapsed, transport: err.Error()}
	}
	return benchOutcome{latency: elapsed, code: envelope.Err}
}

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

type benchStats struct {
	visitors int
	total    int
	// success 只收成功请求的耗时：失败的请求没走完落盘路径，混进来会污染数据。
	success []time.Duration
	// all 收全部请求的耗时，仅在存在失败样本时作为补充信息打印。
	all      []time.Duration
	failures map[string]int
	// burstElapsed 累加每轮从同时发出到最后一条应答的窗口；它排除轮间整备，
	// 用于观察同一主人 Actor 的并发处理吞吐，而不是整套场景吞吐。
	burstElapsed  time.Duration
	burstRequests int
}

func newBenchStats(visitors int) *benchStats {
	return &benchStats{visitors: visitors, failures: make(map[string]int)}
}

func (s *benchStats) addAll(outcomes []benchOutcome) {
	var roundElapsed time.Duration
	for _, outcome := range outcomes {
		s.total++
		if outcome.latency > roundElapsed {
			roundElapsed = outcome.latency
		}
		s.all = append(s.all, outcome.latency)
		if outcome.ok() {
			s.success = append(s.success, outcome.latency)
			continue
		}
		s.failures[benchFailureLabel(outcome)]++
	}
	if roundElapsed > 0 {
		s.burstElapsed += roundElapsed
		s.burstRequests += len(outcomes)
	}
}

func (s *benchStats) render() []string {
	failed := s.total - len(s.success)
	lines := []string{
		fmt.Sprintf("访客数=%d  样本=%d  成功=%d  失败=%d", s.visitors, s.total, len(s.success), failed),
		benchLatencyLine(s.success),
	}
	if failed > 0 {
		lines = append(lines, "失败分布: "+benchFailureBreakdown(s.failures))
		lines = append(lines, "（参考）全部请求含失败: "+benchLatencyLine(s.all))
	}
	if ratio := benchFailureRatio(s.total, failed); ratio > benchFailureWarnRatio {
		lines = append(lines, fmt.Sprintf(
			"!!! 警告：失败率 %.1f%% 超过 %.0f%%，说明场景编排有问题，本次延迟数据不可信 !!!",
			ratio*100, benchFailureWarnRatio*100))
	}
	if s.burstElapsed > 0 {
		qps := float64(s.burstRequests) / s.burstElapsed.Seconds()
		lines = append(lines, fmt.Sprintf(
			"并发窗口吞吐=%.1f req/s（不含轮间整备）", qps))
	}
	return lines
}

func benchFailureRatio(total, failed int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(failed) / float64(total)
}

func benchLatencyLine(samples []time.Duration) string {
	if len(samples) == 0 {
		return "p50=   n/a   p95=   n/a   p99=   n/a   max=   n/a"
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return fmt.Sprintf("p50=%6.1fms   p95=%6.1fms   p99=%6.1fms   max=%6.1fms",
		benchMillis(percentileDuration(sorted, 50)),
		benchMillis(percentileDuration(sorted, 95)),
		benchMillis(percentileDuration(sorted, 99)),
		benchMillis(sorted[len(sorted)-1]))
}

func benchMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// percentileDuration 用最近秩法取分位数：第 ceil(p/100 × n) 个有序样本。
// 相比线性插值，它返回的一定是真实发生过的某一次延迟，便于和日志对上号。
// samples 必须已按升序排好。
func percentileDuration(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	if p <= 0 {
		return samples[0]
	}
	rank := int(math.Ceil(p / 100 * float64(len(samples))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(samples) {
		rank = len(samples)
	}
	return samples[rank-1]
}

func benchFailureBreakdown(failures map[string]int) string {
	labels := make([]string, 0, len(failures))
	for label := range failures {
		labels = append(labels, label)
	}
	// 先按次数降序，次数相同再按标签升序，保证同一份数据每次输出一致。
	sort.Slice(labels, func(i, j int) bool {
		if failures[labels[i]] != failures[labels[j]] {
			return failures[labels[i]] > failures[labels[j]]
		}
		return labels[i] < labels[j]
	})
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		parts = append(parts, fmt.Sprintf("%s=%d", label, failures[label]))
	}
	return strings.Join(parts, "  ")
}

func benchFailureLabel(outcome benchOutcome) string {
	if outcome.transport != "" {
		return "本地失败(" + outcome.transport + ")"
	}
	return fmt.Sprintf("%d(%s)", outcome.code, benchErrorName(outcome.code))
}

func benchErrorName(code errcode.Code) string {
	switch code {
	case errcode.Internal:
		return "服务内部错误"
	case errcode.BadRequest:
		return "请求参数有误"
	case errcode.RateLimited:
		return "限流"
	case errcode.Timeout:
		return "主人侧裁决超时"
	case errcode.PlotNotFound:
		return "地块不存在"
	case errcode.NotOwner:
		return "不在该农场房间"
	case errcode.PlotNotGrowing:
		return "作物不在生长中"
	case errcode.PlotEmpty:
		return "地块没有作物"
	case errcode.AlreadyWatered:
		return "水分充足"
	case errcode.NotFriend:
		return "不是好友"
	default:
		return "未登记错误码"
	}
}

func benchRoundLine(round, totalRounds int, warming bool, outcomes []benchOutcome) string {
	stage := "计入"
	if warming {
		stage = "预热"
	}
	latencies := make([]time.Duration, 0, len(outcomes))
	success := 0
	for _, outcome := range outcomes {
		latencies = append(latencies, outcome.latency)
		if outcome.ok() {
			success++
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var maxLatency time.Duration
	if len(latencies) > 0 {
		maxLatency = latencies[len(latencies)-1]
	}
	return fmt.Sprintf("bench 轮 %3d/%d %s  成功=%d/%d  本轮 p50=%6.1fms  max=%6.1fms",
		round, totalRounds, stage, success, len(outcomes),
		benchMillis(percentileDuration(latencies, 50)), benchMillis(maxLatency))
}

// ---------------------------------------------------------------------------
// 纯计算：地块分配与调时规划
// ---------------------------------------------------------------------------

// benchPlotForVisitor 给第 i 个访客分配地块。访客数不超过地块数时人各一块，
// 每个请求都会真正落盘；超出的部分只能与人共用，会被判 AlreadyWatered。
func benchPlotForVisitor(visitorIndex, plotCount int) uint32 {
	if plotCount <= 0 {
		return 0
	}
	return uint32(visitorIndex % plotCount)
}

// benchTargetPlots 返回本次压测会被浇的地块，按升序且互不重复。
func benchTargetPlots(visitorCount, plotCount int) []uint32 {
	used := visitorCount
	if used > plotCount {
		used = plotCount
	}
	if used < 1 {
		used = 1
	}
	targets := make([]uint32, 0, used)
	for i := 0; i < used; i++ {
		targets = append(targets, uint32(i))
	}
	return targets
}

// benchWaterSpanMs 复刻服务端 farm.waterFull 的整数算法：水分持续时长 =
// 本季时长 × 35%。这里必须与服务端逐位一致，否则会卡在窗口边界上反复失败。
func benchWaterSpanMs(seasonDuration int64) int64 {
	const denominator int64 = 100
	numerator := int64(gameconfig.WaterSpanRatio * float64(denominator))
	if seasonDuration <= 0 || numerator <= 0 {
		return 0
	}
	return seasonDuration * numerator / denominator
}

// benchRoundPlan 描述开始下一轮之前要对服务端时钟做什么。
type benchRoundPlan struct {
	// Advance 是让全部目标地块重新缺水所需的时钟推进量（毫秒）。
	Advance int64
	// Replant 表示目标地块需要重新种植：要么已不在生长中，要么再推进就会成熟。
	Replant bool
	// AdvanceToMature 是补种前先把在生长的作物推到成熟所需的推进量，
	// 收获的果实卖掉正好覆盖补种成本。
	AdvanceToMature int64
}

// benchPlanRound 根据主人农场快照推算本轮的调时方案。
//
// 判定全部基于快照里的 last_water_at / mature_at / season_duration 与应答里的
// server_time，因此不依赖服务端跑在哪个时间档。
func benchPlanRound(plots []farm.PlotSnapshot, targets []uint32, serverTime, margin int64) (benchRoundPlan, error) {
	var plan benchRoundPlan
	var latestMature int64
	for _, idx := range targets {
		if int(idx) >= len(plots) {
			return benchRoundPlan{}, fmt.Errorf("快照缺少地块 %d", idx)
		}
		plot := plots[idx]
		if plot.State != farm.StateGrowing {
			plan.Replant = true
			continue
		}
		if plot.MatureAt > latestMature {
			latestMature = plot.MatureAt
		}
		if need := plot.LastWaterAt + benchWaterSpanMs(plot.SeasonDuration) + margin - serverTime; need > plan.Advance {
			plan.Advance = need
		}
	}
	if !plan.Replant {
		for _, idx := range targets {
			// 推进到缺水点会不会顺手把作物推熟？熟了就浇不动了，得提前补种。
			if serverTime+plan.Advance >= plots[idx].MatureAt {
				plan.Replant = true
				break
			}
		}
	}
	if plan.Replant {
		plan.Advance = 0
		if latestMature > serverTime {
			plan.AdvanceToMature = latestMature - serverTime + 1
		}
	}
	return plan, nil
}

// benchWaterMarks 抽取目标地块的 last_water_at，作为「上一轮是否已收敛」的比较基准。
func benchWaterMarks(plots []farm.PlotSnapshot, targets []uint32) ([]int64, error) {
	marks := make([]int64, 0, len(targets))
	for _, idx := range targets {
		if int(idx) >= len(plots) {
			return nil, fmt.Errorf("快照缺少地块 %d", idx)
		}
		marks = append(marks, plots[idx].LastWaterAt)
	}
	return marks, nil
}

func benchMarksEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// benchTillAction 返回把某状态的地块整理成「空闲已翻」所需的动作。
// 成熟地块由调用方先收获，收获后落到「待清理」，因此与残留同路。
func benchTillAction(state uint8) (cmd uint32, arg uint32, needed bool) {
	switch state {
	case farm.StateWasteland:
		return gateway.CommandTill, 0, true
	case farm.StateTilled:
		return 0, 0, false
	case farm.StateGrowing:
		return gateway.CommandClear, uint32(farm.ClearArgUproot), true
	default:
		// 成熟（已收获）、待清理、枯萎都靠 Clear 回到已翻状态。
		return gateway.CommandClear, 0, true
	}
}
