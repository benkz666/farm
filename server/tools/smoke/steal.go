package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/platform/farm"
	"farm/server/platform/gameconf"
	"farm/server/platform/pkgerr"
	"farm/server/platform/pkgjson"
	"farm/server/platform/routing"
	"farm/server/services/gateway/gateway"
)

const (
	stealCropID       = 1 // 白萝卜
	stealDogFoodItem  = 2000
	stealDogMuttItem  = 2001
	stealDogTypeMutt  = 1
	stealMaxIntercept = 48 // 土狗 25% 拦截率下连续未中概率极低
)

type stealRewardResponse struct {
	ReqID        pkgjson.Uint64 `json:"req_id"`
	CropID       uint16         `json:"crop_id,omitempty"`
	Amount       uint16         `json:"amount,omitempty"`
	Compensation pkgjson.Int64  `json:"compensation,omitempty"`
	DogType      uint8          `json:"dog_type,omitempty"`
}

type smokePlayer struct {
	login authResponse
	farm  string
	conn  *websocket.Conn
	seq   uint32
}

// runSteal 覆盖五服务环境中的偷菜主路径：
// 1) 多人额度竞争 → 总量 ≤ 40%，超额 1410
// 2) 主人收获后访客偷 → 1216
// 3) 余额不足预冻 → 1412
// 4) 土狗有粮拦截 → 1411 + 赔付转给主人
func runSteal(baseURL string) error {
	routes, err := loadSmokeRouteTable()
	if err != nil {
		return err
	}
	crop, ok := gameconf.CropByID(stealCropID)
	if !ok {
		return fmt.Errorf("missing crop %d", stealCropID)
	}
	compensation := gameconf.StealCompensation(crop)

	owner, err := openSmokePlayer(baseURL, routes, "farm-0")
	if err != nil {
		return fmt.Errorf("owner: %w", err)
	}
	defer owner.conn.Close()

	visitors := make([]*smokePlayer, 0, 4)
	for i := 0; i < 4; i++ {
		v, err := openSmokePlayer(baseURL, routes, "farm-1")
		if err != nil {
			return fmt.Errorf("visitor[%d]: %w", i, err)
		}
		defer v.conn.Close()
		visitors = append(visitors, v)
	}

	fmt.Printf("smoke steal: owner uid=%d farm=%s; visitors=%d on farm-1\n",
		owner.login.UID, owner.farm, len(visitors))

	if err := befriendAll(owner, visitors); err != nil {
		return err
	}

	if _, err := exchangeResponse(owner.conn, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: owner.seq,
		Payload:   mustJSON(map[string]any{"owner_uid": 0}),
	}); err != nil {
		return fmt.Errorf("owner EnterFarm: %w", err)
	}
	owner.seq++

	// --- 场景 1：额度竞争（无狗）---
	if err := ownerPrepareMaturePlot(owner, baseURL, 0); err != nil {
		return fmt.Errorf("quota mature plot0: %w", err)
	}
	finalYield, err := ownerFinalYield(owner, 0)
	if err != nil {
		return fmt.Errorf("quota final yield: %w", err)
	}
	quotaCap := uint32(finalYield) * 40 / 100
	if quotaCap == 0 {
		return fmt.Errorf("quota cap is zero for final yield %d", finalYield)
	}
	var stolenTotal uint32
	var sawQuotaExhausted bool
	for i, v := range visitors {
		if err := visitorEnter(v, owner.login.UID); err != nil {
			return fmt.Errorf("quota visitor[%d] enter: %w", i, err)
		}
		env, err := exchangeStealEnvelope(v.conn, &v.seq, owner.login.UID, 0, stealCropID)
		if err != nil {
			return fmt.Errorf("quota visitor[%d] steal: %w", i, err)
		}
		switch env.Err {
		case pkgerr.OK:
			var reward stealRewardResponse
			if err := json.Unmarshal(env.Payload, &reward); err != nil {
				return fmt.Errorf("quota visitor[%d] decode: %w", i, err)
			}
			if reward.Amount == 0 || reward.CropID != stealCropID {
				return fmt.Errorf("quota visitor[%d] reward = %#v, want crop=%d amount>0", i, reward, stealCropID)
			}
			stolenTotal += uint32(reward.Amount)
			fmt.Printf("smoke steal quota: visitor[%d] stole %d (total=%d cap=%d)\n",
				i, reward.Amount, stolenTotal, quotaCap)
		case pkgerr.StealQuotaExhausted:
			sawQuotaExhausted = true
			fmt.Printf("smoke steal quota: visitor[%d] got 1410\n", i)
		default:
			return fmt.Errorf("quota visitor[%d] err = %d, want OK or 1410", i, env.Err)
		}
	}
	if stolenTotal == 0 {
		return fmt.Errorf("quota: no successful steal")
	}
	if stolenTotal > quotaCap {
		return fmt.Errorf("quota: stolen total %d > cap %d", stolenTotal, quotaCap)
	}
	if !sawQuotaExhausted {
		extra, err := openSmokePlayer(baseURL, routes, "farm-1")
		if err != nil {
			return fmt.Errorf("quota exhaust visitor: %w", err)
		}
		defer extra.conn.Close()
		if err := befriendPair(owner, extra); err != nil {
			return err
		}
		if err := visitorEnter(extra, owner.login.UID); err != nil {
			return fmt.Errorf("quota exhaust enter: %w", err)
		}
		env, err := exchangeStealEnvelope(extra.conn, &extra.seq, owner.login.UID, 0, stealCropID)
		if err != nil {
			return fmt.Errorf("quota exhaust steal: %w", err)
		}
		switch env.Err {
		case pkgerr.StealQuotaExhausted:
			sawQuotaExhausted = true
		case pkgerr.OK:
			var reward stealRewardResponse
			if err := json.Unmarshal(env.Payload, &reward); err != nil {
				return err
			}
			stolenTotal += uint32(reward.Amount)
			if stolenTotal > quotaCap {
				return fmt.Errorf("quota: stolen total %d > cap %d after extra", stolenTotal, quotaCap)
			}
			extra2, err := openSmokePlayer(baseURL, routes, "farm-1")
			if err != nil {
				return fmt.Errorf("quota exhaust visitor2: %w", err)
			}
			defer extra2.conn.Close()
			if err := befriendPair(owner, extra2); err != nil {
				return err
			}
			if err := visitorEnter(extra2, owner.login.UID); err != nil {
				return err
			}
			env2, err := exchangeStealEnvelope(extra2.conn, &extra2.seq, owner.login.UID, 0, stealCropID)
			if err != nil {
				return err
			}
			if env2.Err != pkgerr.StealQuotaExhausted {
				return fmt.Errorf("quota exhaust2 err = %d, want %d (stolen=%d cap=%d)",
					env2.Err, pkgerr.StealQuotaExhausted, stolenTotal, quotaCap)
			}
			sawQuotaExhausted = true
		default:
			return fmt.Errorf("quota exhaust err = %d", env.Err)
		}
	}
	fmt.Printf("smoke steal quota: ok total=%d cap=%d exhausted=true\n", stolenTotal, quotaCap)

	// --- 场景 2：收获 vs 偷菜 ---
	if err := ownerPrepareMaturePlot(owner, baseURL, 1); err != nil {
		return fmt.Errorf("harvest mature plot1: %w", err)
	}
	if _, err := mustOwnerAction(owner, gateway.CommandHarvest, map[string]any{
		"owner_uid": 0, "plot_index": 1, "arg": 0,
	}); err != nil {
		return fmt.Errorf("owner harvest plot1: %w", err)
	}
	// 用尚未在配额场景里灌满 FarmDelta 缓冲的访客，降低推送干扰。
	harvester := visitors[len(visitors)-1]
	if err := visitorEnter(harvester, owner.login.UID); err != nil {
		return fmt.Errorf("harvest race enter: %w", err)
	}
	harvestEnv, err := exchangeStealEnvelope(harvester.conn, &harvester.seq, owner.login.UID, 1, stealCropID)
	if err != nil {
		return fmt.Errorf("harvest race steal: %w", err)
	}
	if harvestEnv.Err != pkgerr.HarvestedByOwner {
		return fmt.Errorf("harvest race err = %d, want %d", harvestEnv.Err, pkgerr.HarvestedByOwner)
	}
	fmt.Println("smoke steal harvest-race: ok got 1216")

	// --- 场景 3：1412 余额不足 ---
	if err := ownerPrepareMaturePlot(owner, baseURL, 2); err != nil {
		return fmt.Errorf("no-afford mature plot2: %w", err)
	}
	broke, err := openSmokePlayer(baseURL, routes, "farm-1")
	if err != nil {
		return fmt.Errorf("no-afford visitor: %w", err)
	}
	defer broke.conn.Close()
	if err := befriendPair(owner, broke); err != nil {
		return err
	}
	if env, err := visitorExchange(broke, gateway.CommandEnterFarm, map[string]any{"owner_uid": 0}); err != nil {
		return fmt.Errorf("no-afford self enter: %w", err)
	} else if env.Err != pkgerr.OK {
		return fmt.Errorf("no-afford self enter err = %d", env.Err)
	}
	spend := gameconf.InitialCoin - (compensation - 1)
	if spend < 1 {
		spend = 1
	}
	buyEnv, err := visitorExchange(broke, gateway.CommandBuy, map[string]any{
		"item_id":  stealDogFoodItem,
		"quantity": spend,
	})
	if err != nil {
		return fmt.Errorf("no-afford drain coins: %w", err)
	}
	if buyEnv.Err != pkgerr.OK {
		return fmt.Errorf("no-afford drain err = %d", buyEnv.Err)
	}
	var buy actionPayload
	if err := json.Unmarshal(buyEnv.Payload, &buy); err != nil {
		return fmt.Errorf("no-afford decode buy: %w", err)
	}
	if buy.Patch.Coin >= compensation {
		return fmt.Errorf("no-afford coin=%d still >= compensation %d", buy.Patch.Coin, compensation)
	}
	if err := visitorEnter(broke, owner.login.UID); err != nil {
		return fmt.Errorf("no-afford enter owner: %w", err)
	}
	noAffordEnv, err := exchangeStealEnvelope(broke.conn, &broke.seq, owner.login.UID, 2, stealCropID)
	if err != nil {
		return fmt.Errorf("no-afford steal: %w", err)
	}
	if noAffordEnv.Err != pkgerr.StealNoAfford {
		return fmt.Errorf("no-afford err = %d, want %d", noAffordEnv.Err, pkgerr.StealNoAfford)
	}
	fmt.Println("smoke steal no-afford: ok got 1412")

	// --- 场景 4：狗拦截转账 ---
	// 顺序约束：
	// 1) 先攒够买狗钱并种熟一块地，再买狗（避免买狗后金币不够补种）
	// 2) 每次偷：成熟 → 喂狗 → 偷（调时会耗狗粮，必须先熟后喂）
	if err := ownerEarnForDog(owner, baseURL, crop); err != nil {
		return fmt.Errorf("earn for dog: %w", err)
	}
	plotIdx := uint32(0)
	if err := ownerResetAndMature(owner, baseURL, plotIdx); err != nil {
		return fmt.Errorf("pre-dog mature: %w", err)
	}
	if _, err := mustOwnerAction(owner, gateway.CommandBuy, map[string]any{
		"item_id": stealDogMuttItem, "quantity": 1,
	}); err != nil {
		return fmt.Errorf("buy dog: %w", err)
	}
	if env, err := ownerExchange(owner, gateway.CommandPetActivate, map[string]any{
		"dog_type": stealDogTypeMutt,
	}); err != nil {
		return fmt.Errorf("PetActivate: %w", err)
	} else if env.Err != pkgerr.OK {
		return fmt.Errorf("PetActivate err = %d", env.Err)
	}

	intercepted := false
	var interceptReward stealRewardResponse
	var coinBefore, coinAfter int64
	if dbg, err := ownerSnapshotCoin(owner); err == nil {
		fmt.Printf("DEBUG intercept loop start: owner coin=%d\n", dbg)
	} else {
		fmt.Printf("DEBUG intercept loop start: snapshot err=%v\n", err)
	}
	for attempt := 0; attempt < stealMaxIntercept && !intercepted; attempt++ {
		if attempt > 0 {
			plotIdx = (plotIdx + 1) % uint32(gameconf.InitialUnlockedPlots)
			if err := ownerResetAndMature(owner, baseURL, plotIdx); err != nil {
				return fmt.Errorf("intercept mature plot %d: %w", plotIdx, err)
			}
		}
		// 成熟调时之后再补粮，保证偷菜当下狗盆非空。
		if _, err := mustOwnerAction(owner, gateway.CommandBuy, map[string]any{
			"item_id": stealDogFoodItem, "quantity": 80,
		}); err != nil {
			if dbg, e2 := ownerSnapshotCoin(owner); e2 == nil {
				fmt.Printf("DEBUG intercept buy food failed: owner coin=%d (need 80) attempt=%d\n", dbg, attempt)
			}
			return fmt.Errorf("intercept buy food: %w", err)
		}
		if env, err := ownerExchange(owner, gateway.CommandPetFeed, map[string]any{"grams": 80}); err != nil {
			return fmt.Errorf("intercept PetFeed: %w", err)
		} else if env.Err != pkgerr.OK && env.Err != pkgerr.BowlFull {
			return fmt.Errorf("intercept PetFeed err = %d", env.Err)
		}
		var err error
		coinBefore, err = ownerSnapshotCoin(owner)
		if err != nil {
			return fmt.Errorf("owner coin before steal: %w", err)
		}
		v, err := openSmokePlayer(baseURL, routes, "farm-1")
		if err != nil {
			return fmt.Errorf("intercept visitor: %w", err)
		}
		if err := befriendPair(owner, v); err != nil {
			_ = v.conn.Close()
			return err
		}
		if err := visitorEnter(v, owner.login.UID); err != nil {
			_ = v.conn.Close()
			return fmt.Errorf("intercept enter: %w", err)
		}
		env, err := exchangeStealEnvelope(v.conn, &v.seq, owner.login.UID, plotIdx, stealCropID)
		_ = v.conn.Close()
		if err != nil {
			return fmt.Errorf("intercept steal attempt %d: %w", attempt, err)
		}
		switch env.Err {
		case pkgerr.StealIntercepted:
			if err := json.Unmarshal(env.Payload, &interceptReward); err != nil {
				return fmt.Errorf("intercept decode: %w", err)
			}
			intercepted = true
			fmt.Printf("smoke steal intercept: attempt=%d plot=%d compensation=%d dog=%d\n",
				attempt, plotIdx, interceptReward.Compensation, interceptReward.DogType)
		case pkgerr.OK, pkgerr.StealQuotaExhausted, pkgerr.StealAlreadyDone:
			// 下一轮换地重种
		default:
			return fmt.Errorf("intercept attempt %d err = %d payload=%s", attempt, env.Err, string(env.Payload))
		}
	}
	if !intercepted {
		return fmt.Errorf("dog intercept: no 1411 after %d attempts", stealMaxIntercept)
	}
	if int64(interceptReward.Compensation) != compensation {
		return fmt.Errorf("intercept compensation = %d, want %d", interceptReward.Compensation, compensation)
	}
	if interceptReward.DogType != stealDogTypeMutt {
		return fmt.Errorf("intercept dog_type = %d, want %d", interceptReward.DogType, stealDogTypeMutt)
	}
	coinAfter, err = ownerSnapshotCoin(owner)
	if err != nil {
		return fmt.Errorf("owner coin after intercept: %w", err)
	}
	if coinAfter != coinBefore+compensation {
		return fmt.Errorf("owner coin after intercept = %d, want %d (before=%d +%d)",
			coinAfter, coinBefore+compensation, coinBefore, compensation)
	}
	fmt.Println("smoke steal intercept: ok got 1411 + owner credited")
	return nil
}

func openSmokePlayer(baseURL string, routes *routing.RouteTable, wantFarm string) (*smokePlayer, error) {
	login, farmID, err := registerOnFarm(baseURL, routes, wantFarm)
	if err != nil {
		return nil, err
	}
	conn, err := dialAndHandshake(login)
	if err != nil {
		return nil, err
	}
	return &smokePlayer{login: login, farm: farmID, conn: conn, seq: 2}, nil
}

func befriendAll(owner *smokePlayer, visitors []*smokePlayer) error {
	shareEnv, err := exchangeResponse(owner.conn, gateway.Envelope{
		Cmd:       gateway.CommandGenShareLink,
		ClientSeq: owner.seq,
		Payload:   emptyJSONObject,
	})
	if err != nil {
		return fmt.Errorf("GenShareLink: %w", err)
	}
	owner.seq++
	var share genShareLinkResponse
	if err := json.Unmarshal(shareEnv.Payload, &share); err != nil {
		return fmt.Errorf("decode GenShareLink: %w", err)
	}
	token := strings.TrimPrefix(share.Path, "/i/")
	if token == "" {
		return fmt.Errorf("share token empty")
	}
	for i, v := range visitors {
		if _, err := exchangeResponse(v.conn, gateway.Envelope{
			Cmd:       gateway.CommandAcceptInvite,
			ClientSeq: v.seq,
			Payload:   mustJSON(map[string]any{"token": token}),
		}); err != nil {
			return fmt.Errorf("AcceptInvite visitor[%d]: %w", i, err)
		}
		v.seq++
	}
	return nil
}

func befriendPair(owner, visitor *smokePlayer) error {
	if _, err := exchangeResponse(visitor.conn, gateway.Envelope{
		Cmd:       gateway.CommandAddFriendByUID,
		ClientSeq: visitor.seq,
		Payload:   mustJSON(map[string]any{"peer_uid": owner.login.UID}),
	}); err != nil {
		return fmt.Errorf("AddFriendByUID: %w", err)
	}
	visitor.seq++
	return nil
}

func visitorEnter(v *smokePlayer, ownerUID uint64) error {
	enterEnv, err := visitorExchange(v, gateway.CommandEnterFarm, map[string]any{"owner_uid": ownerUID})
	if err != nil {
		return err
	}
	if enterEnv.Err != pkgerr.OK {
		return fmt.Errorf("EnterFarm err = %d", enterEnv.Err)
	}
	var enter enterFarmResponse
	if err := json.Unmarshal(enterEnv.Payload, &enter); err != nil {
		return err
	}
	if enter.Relation != "FRIEND" {
		return fmt.Errorf("relation = %q, want FRIEND", enter.Relation)
	}
	return nil
}

func visitorExchange(v *smokePlayer, cmd uint32, payload map[string]any) (gateway.Envelope, error) {
	request := gateway.Envelope{
		Cmd:       cmd,
		ClientSeq: v.seq,
		Payload:   mustJSON(payload),
	}
	v.seq++
	data, err := gateway.EncodeEnvelope(request)
	if err != nil {
		return gateway.Envelope{}, err
	}
	if err := v.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return gateway.Envelope{}, err
	}
	if err := v.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return gateway.Envelope{}, err
	}
	defer func() { _ = v.conn.SetReadDeadline(time.Time{}) }()
	for attempt := 0; attempt < 32; attempt++ {
		messageType, frame, err := v.conn.ReadMessage()
		if err != nil {
			return gateway.Envelope{}, fmt.Errorf("read response: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		env, err := gateway.DecodeEnvelope(frame)
		if err != nil {
			return gateway.Envelope{}, err
		}
		if env.Cmd == request.Cmd && env.ClientSeq == request.ClientSeq {
			return env, nil
		}
	}
	return gateway.Envelope{}, fmt.Errorf("no matching response for cmd=%d seq=%d", request.Cmd, request.ClientSeq)
}

func ownerPrepareMaturePlot(owner *smokePlayer, baseURL string, plotIndex uint32) error {
	if _, err := mustOwnerAction(owner, gateway.CommandBuy, map[string]any{
		"item_id": stealCropID, "quantity": 1,
	}); err != nil {
		return fmt.Errorf("buy seed: %w", err)
	}
	if _, err := mustOwnerAction(owner, gateway.CommandTill, map[string]any{
		"owner_uid": 0, "plot_index": plotIndex, "arg": 0,
	}); err != nil {
		return fmt.Errorf("till: %w", err)
	}
	if _, err := mustOwnerAction(owner, gateway.CommandPlant, map[string]any{
		"owner_uid": 0, "plot_index": plotIndex, "arg": stealCropID,
	}); err != nil {
		return fmt.Errorf("plant: %w", err)
	}
	return ownerAdvanceToMature(owner, baseURL, plotIndex)
}

func ownerFinalYield(owner *smokePlayer, plotIndex uint32) (uint16, error) {
	env, err := exchangeResponse(owner.conn, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: owner.seq,
		Payload:   mustJSON(map[string]any{"owner_uid": 0}),
	})
	owner.seq++
	if err != nil {
		return 0, err
	}
	var enter enterFarmResponse
	if err := json.Unmarshal(env.Payload, &enter); err != nil {
		return 0, err
	}
	if int(plotIndex) >= len(enter.Snapshot.Plots) {
		return 0, fmt.Errorf("plot %d missing from snapshot", plotIndex)
	}
	return enter.Snapshot.Plots[plotIndex].FinalYield, nil
}

func ownerResetAndMature(owner *smokePlayer, baseURL string, plotIndex uint32) error {
	// Clear 残留/枯萎 → 已翻地；荒地则 Till。失败时尝试 Till。
	clearEnv, err := ownerExchange(owner, gateway.CommandClear, map[string]any{
		"owner_uid": 0, "plot_index": plotIndex, "arg": 0,
	})
	if err != nil {
		return err
	}
	if clearEnv.Err != pkgerr.OK {
		_, _ = ownerExchange(owner, gateway.CommandTill, map[string]any{
			"owner_uid": 0, "plot_index": plotIndex, "arg": 0,
		})
	}
	if _, err := mustOwnerAction(owner, gateway.CommandBuy, map[string]any{
		"item_id": stealCropID, "quantity": 1,
	}); err != nil {
		return fmt.Errorf("buy seed: %w", err)
	}
	plantEnv, err := ownerExchange(owner, gateway.CommandPlant, map[string]any{
		"owner_uid": 0, "plot_index": plotIndex, "arg": stealCropID,
	})
	if err != nil {
		return err
	}
	if plantEnv.Err != pkgerr.OK {
		// 可能仍是成熟地：先收获再清。
		_, _ = ownerExchange(owner, gateway.CommandHarvest, map[string]any{
			"owner_uid": 0, "plot_index": plotIndex, "arg": 0,
		})
		_, _ = ownerExchange(owner, gateway.CommandClear, map[string]any{
			"owner_uid": 0, "plot_index": plotIndex, "arg": 0,
		})
		if _, err := mustOwnerAction(owner, gateway.CommandPlant, map[string]any{
			"owner_uid": 0, "plot_index": plotIndex, "arg": stealCropID,
		}); err != nil {
			return fmt.Errorf("replant: %w", err)
		}
	}
	return ownerAdvanceToMature(owner, baseURL, plotIndex)
}

func ownerAdvanceToMature(owner *smokePlayer, baseURL string, plotIndex uint32) error {
	crop, ok := gameconf.CropByID(stealCropID)
	if !ok {
		return fmt.Errorf("missing crop")
	}
	seasonMS := gameconf.SeasonDurationMs(crop, 0, gameconf.TimeProfileDemo)
	waterSpan := seasonMS * 35 / 100
	if err := debugAdvance(baseURL, waterSpan); err != nil {
		return fmt.Errorf("advance water1: %w", err)
	}
	if _, err := mustOwnerAction(owner, gateway.CommandWater, map[string]any{
		"owner_uid": 0, "plot_index": plotIndex, "arg": 0,
	}); err != nil {
		return fmt.Errorf("water1: %w", err)
	}
	if err := debugAdvance(baseURL, waterSpan); err != nil {
		return fmt.Errorf("advance water2: %w", err)
	}
	if _, err := mustOwnerAction(owner, gateway.CommandWater, map[string]any{
		"owner_uid": 0, "plot_index": plotIndex, "arg": 0,
	}); err != nil {
		return fmt.Errorf("water2: %w", err)
	}
	remain := seasonMS - 2*waterSpan
	if remain < 0 {
		remain = 0
	}
	if err := debugAdvance(baseURL, remain+1); err != nil {
		return fmt.Errorf("advance mature: %w", err)
	}
	return nil
}

func ownerEarnForDog(owner *smokePlayer, baseURL string, crop gameconf.CropConf) error {
	fruitKey := string(farm.FruitItem(stealCropID))
	// 土狗 2000 + 拦截重试（种子+狗粮）余量；用 Sell patch 跟踪金币，少打 EnterFarm。
	// 土狗 2000 + 多次补种/狗粮（80×约 10 次 + 种子 125×约 8 次）
	const needCoin int64 = 3600
	var coin int64 = -1
	for plot := uint32(0); plot < uint32(gameconf.InitialUnlockedPlots); plot++ {
		env, err := ownerExchange(owner, gateway.CommandHarvest, map[string]any{
			"owner_uid": 0, "plot_index": plot, "arg": 0,
		})
		if err != nil {
			return err
		}
		if env.Err != pkgerr.OK {
			continue
		}
		var harvest actionPayload
		if err := json.Unmarshal(env.Payload, &harvest); err != nil {
			return err
		}
		if qty := harvest.Patch.Warehouse[fruitKey]; qty > 0 {
			sell, err := mustOwnerAction(owner, gateway.CommandSell, map[string]any{
				"item_id": stealCropID, "quantity": qty,
			})
			if err != nil {
				return fmt.Errorf("earn sell existing plot%d: %w", plot, err)
			}
			coin = sell.Patch.Coin
		}
	}
	for attempt := 0; attempt < 24; attempt++ {
		if coin >= needCoin {
			fmt.Printf("smoke steal: owner earned coin=%d for dog\n", coin)
			return nil
		}
		if coin < 0 || attempt%4 == 0 {
			var err error
			coin, err = ownerSnapshotCoin(owner)
			if err != nil {
				return err
			}
			if coin >= needCoin {
				fmt.Printf("smoke steal: owner earned coin=%d for dog\n", coin)
				return nil
			}
		}
		plot := uint32(attempt % int(gameconf.InitialUnlockedPlots))
		if err := ownerResetAndMature(owner, baseURL, plot); err != nil {
			return fmt.Errorf("earn grow plot%d: %w", plot, err)
		}
		harvest, err := mustOwnerAction(owner, gateway.CommandHarvest, map[string]any{
			"owner_uid": 0, "plot_index": plot, "arg": 0,
		})
		if err != nil {
			return fmt.Errorf("earn harvest plot%d: %w", plot, err)
		}
		qty := harvest.Patch.Warehouse[fruitKey]
		if qty == 0 {
			qty = uint32(crop.Yield)
		}
		sell, err := mustOwnerAction(owner, gateway.CommandSell, map[string]any{
			"item_id": stealCropID, "quantity": qty,
		})
		if err != nil {
			return fmt.Errorf("earn sell plot%d qty=%d: %w", plot, qty, err)
		}
		coin = sell.Patch.Coin
	}
	return fmt.Errorf("earn for dog: coin=%d, want >= %d", coin, needCoin)
}

func ownerSnapshotCoin(owner *smokePlayer) (int64, error) {
	enterEnv, err := ownerExchange(owner, gateway.CommandEnterFarm, map[string]any{"owner_uid": 0})
	if err != nil {
		return 0, err
	}
	if enterEnv.Err != pkgerr.OK {
		return 0, fmt.Errorf("EnterFarm err = %d", enterEnv.Err)
	}
	var enter enterFarmResponse
	if err := json.Unmarshal(enterEnv.Payload, &enter); err != nil {
		return 0, err
	}
	return enter.Snapshot.Coin, nil
}

// mustOwnerAction / ownerExchange 在读响应时排空 FarmDelta(9000) 推送，
// 因为访客偷菜会向主人房间广播，打断严格 cmd/seq 匹配。
func mustOwnerAction(owner *smokePlayer, cmd uint32, payload map[string]any) (actionPayload, error) {
	env, err := ownerExchange(owner, cmd, payload)
	if err != nil {
		return actionPayload{}, err
	}
	if env.Err != pkgerr.OK {
		return actionPayload{}, fmt.Errorf("err = %d, want %d", env.Err, pkgerr.OK)
	}
	var out actionPayload
	if err := json.Unmarshal(env.Payload, &out); err != nil {
		return actionPayload{}, fmt.Errorf("decode action payload: %w", err)
	}
	return out, nil
}

func ownerExchange(owner *smokePlayer, cmd uint32, payload map[string]any) (gateway.Envelope, error) {
	for rateTry := 0; rateTry < 12; rateTry++ {
		request := gateway.Envelope{
			Cmd:       cmd,
			ClientSeq: owner.seq,
			Payload:   mustJSON(payload),
		}
		owner.seq++
		data, err := gateway.EncodeEnvelope(request)
		if err != nil {
			return gateway.Envelope{}, fmt.Errorf("encode request: %w", err)
		}
		if err := owner.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return gateway.Envelope{}, fmt.Errorf("write request: %w", err)
		}
		if err := owner.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return gateway.Envelope{}, err
		}
		var matched *gateway.Envelope
		for attempt := 0; attempt < 32; attempt++ {
			messageType, frame, err := owner.conn.ReadMessage()
			if err != nil {
				_ = owner.conn.SetReadDeadline(time.Time{})
				return gateway.Envelope{}, fmt.Errorf("read response: %w", err)
			}
			if messageType != websocket.TextMessage {
				continue
			}
			env, err := gateway.DecodeEnvelope(frame)
			if err != nil {
				_ = owner.conn.SetReadDeadline(time.Time{})
				return gateway.Envelope{}, err
			}
			if env.Cmd == request.Cmd && env.ClientSeq == request.ClientSeq {
				matched = &env
				break
			}
			// 忽略 FarmDelta 等服务端推送。
		}
		_ = owner.conn.SetReadDeadline(time.Time{})
		if matched == nil {
			return gateway.Envelope{}, fmt.Errorf("no matching response for cmd=%d seq=%d", request.Cmd, request.ClientSeq)
		}
		if matched.Err == pkgerr.RateLimited {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return *matched, nil
	}
	return gateway.Envelope{}, fmt.Errorf("rate limited after retries cmd=%d", cmd)
}

func exchangeStealEnvelope(conn *websocket.Conn, seq *uint32, ownerUID uint64, plotIndex, cropID uint32) (gateway.Envelope, error) {
	request := gateway.Envelope{
		Cmd:       gateway.CommandSteal,
		ClientSeq: *seq,
		Payload:   mustJSON(map[string]any{"owner_uid": ownerUID, "plot_index": plotIndex, "crop_id": cropID}),
	}
	*seq++
	data, err := gateway.EncodeEnvelope(request)
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("encode request: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return gateway.Envelope{}, fmt.Errorf("write request: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return gateway.Envelope{}, fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for attempt := 0; attempt < 32; attempt++ {
		messageType, frame, err := conn.ReadMessage()
		if err != nil {
			return gateway.Envelope{}, fmt.Errorf("read response: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		env, err := gateway.DecodeEnvelope(frame)
		if err != nil {
			return gateway.Envelope{}, fmt.Errorf("decode response: %w", err)
		}
		if env.Cmd == request.Cmd && env.ClientSeq == request.ClientSeq {
			return env, nil
		}
	}
	return gateway.Envelope{}, fmt.Errorf("no matching Steal response seq=%d", request.ClientSeq)
}
