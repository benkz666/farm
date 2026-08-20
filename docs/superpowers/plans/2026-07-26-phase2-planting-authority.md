# Phase 2 Planting Authority Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 落地服务端种植权威（advance + 主路径动作 + Buy/Sell）与客户端 online「发意图、等 Rsp」接线；期末补化肥与多季。

**Architecture:** 在现有 Actor 串行模型上扩展 `farm` 动作 validate/commit；Gateway 增加 PlotAction/Buy/Sell cmd；`item` 表持久化背包仓库；客户端保留本地模式，登录进房后切 online，工具栏改发 WS，成功后再 `applyPatch`。

**Tech Stack:** 现有 Go farm-server、MySQL/Redis、Vue3 + Vite、JSON Envelope（`farm.v1.json`）

## Global Constraints

- 规格：`docs/superpowers/specs/2026-07-26-phase2-planting-authority.md`
- 错误码/命令号照抄 `docs/design/protocol.md`；禁止自造码
- 提交：约定式前缀 + **中文**主题（见 `AGENTS.md`）
- 期 2 **不**做 FarmDelta 广播、好友浇水/偷菜、完整乐观回滚、CSV 双端生成
- 子阶段顺序：2a → 2b → 2c；本地模式过渡保留
- 时间：单测/smoke **注入 now**；播种时折算 SeasonDuration
- 白萝卜参考（与客户端 config 对齐）：seedPrice=125，cycleH=10，yield=16，fruitPrice=17，unlock=0

---

## File Structure (target additions)

```text
server/migrations/002_items.sql
server/internal/gameconf/crops.go          # 最小作物表
server/internal/gameconf/time.go           # TIME_SCALE / 档位
server/internal/gameconf/health.go         # 权重等
server/internal/farm/advance.go
server/internal/farm/actions.go            # Till/Plant/... validate+commit
server/internal/farm/actions_test.go
server/internal/farm/advance_test.go
server/internal/farm/economy.go            # Buy/Sell
server/internal/farm/patch.go              # ActionPatch / 扩展 Snapshot
server/internal/store/items.go
server/internal/gateway/cmds.go            # 命令常量扩展
server/internal/gateway/ws_actions.go
server/cmd/smoke/main.go                   # 扩展种植闭环
client/src/net/session.js
client/src/net/client.js                   # 扩展动作 API
client/src/game/applyPatch.js
client/src/game/main.js                    # online 分支
```

---

## 2a — 服务端权威

### Task 1: 扩展 gameconf（作物/时间/健康度子集）

**Files:**
- Create: `server/internal/gameconf/crops.go`, `time.go`, `health.go`
- Test: `server/internal/gameconf/crops_test.go`

**Interfaces:**
- Produces: `CropByID(id uint16) (CropConf, bool)`；`HourMs(profile string) int64`；`WaterSpan`/`WitherSpan`/`YieldFloor` 等常量
- Consumes: 无

- [x] **Step 1: 写失败测试** — 断言白萝卜 `SeedPrice==125`、`CycleHours==10`、`Yield==16`

- [x] **Step 2: 运行确认失败**

```bash
cd server && go test ./internal/gameconf/ -run TestWhiteRadish -v
```

Expected: FAIL

- [x] **Step 3: 实现最小作物表**（至少白萝卜 + 1–2 种便于解锁测试；字段：ID、UnlockLevel、SeedPrice、FruitPrice、Yield、Seasons、CycleHours、HarvestExp）

- [x] **Step 4: 测试通过并提交**

```bash
git add server/internal/gameconf
git commit -m "feat: 添加期 2 作物与时间配置子集"
```

---

### Task 2: 实现 `advance` / `settleTo`（可注入 now）

**Files:**
- Create: `server/internal/farm/advance.go`, `advance_test.go`
- Modify: `server/internal/farm/plot.go`（仅当需辅助方法）

**Interfaces:**
- Produces: `func Advance(p *Plot, now int64, cfg AdvanceConfig)`；跨 MatureAt → Mature + FinalYield；跨 Wither → Withered；Growing 结算 AccruedWeighted
- Consumes: gameconf 权重与跨度

- [x] **Step 1: 红测** — 种植后 `now = MatureAt` 应变 Mature；`now = MatureAt + 3*SeasonDuration` 变 Withered

- [x] **Step 2: 实现最小 advance**（风险窗口可先简化：仅按缺水时长扣健康度，草/虫可在 Water 路径外用固定规则逐步加）

- [x] **Step 3: `go test ./internal/farm/ -run Advance -v` 绿

- [x] **Step 4: Commit** `feat: 实现地块惰性 advance 与成熟枯萎`

---

### Task 3: 动作 validate/commit 主路径

**Files:**
- Create: `server/internal/farm/actions.go`, `actions_test.go`
- Modify: `server/internal/pkgerr/codes.go`（补 120x/130x 本期用到的码）

**Interfaces:**
- Produces:
```go
type ActionResult struct {
    Err   pkgerr.Code
    Patch ActionPatch // 变更地块 index、coin、items 摘要等
}
func (a *Aggregate) ApplyPlotAction(act PlotAction, now int64) ActionResult
```
- `PlotAction`含 Kind(Till/Clear/Plant/Water/Weed/Pest/Harvest)、PlotIndex、Arg
- 非法状态返回 protocol 码且聚合不变（可拷贝前后对比）

- [x] **Step 1: 表驱动红测** — Wasteland+Till→Tilled；Tilled+Plant(白萝卜)+有种子→Growing；Mature+Harvest→Residue/清空；Growing+Till→错误码

- [x] **Step 2: 实现动作**（Plant 扣种子；Harvest 按 FinalYield/健康度算产量入仓库；Water 更新 LastWaterAt；Clear 处理 Residue/Withered）

- [x] **Step 3: 测试绿；补 `Bag` map 到 Aggregate（`map[ItemKey]uint32` 或分 Seeds/Fruits）

- [x] **Step 4: Commit** `feat: 实现种植主路径 validate/commit 动作`

---

### Task 4: item 表 + Store 读写

**Files:**
- Create: `server/migrations/002_items.sql`, `server/internal/store/items.go`
- Modify: `server/internal/store/mysql.go`, `store.go`, `redis` 缓存序列化以含 Items
- Test: integration 或单测 codec

- [x] **Step 1: DDL**

```sql
CREATE TABLE IF NOT EXISTS item (
  uid BIGINT UNSIGNED NOT NULL,
  kind TINYINT UNSIGNED NOT NULL,
  item_id SMALLINT UNSIGNED NOT NULL,
  count INT UNSIGNED NOT NULL,
  PRIMARY KEY (uid, kind, item_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- [x] **Step 2: LoadFarm/SaveFarm 同步 items**；集成测试：种一粒种子 count-1 持久化

- [x] **Step 3: Commit** `feat: 新增 item 表并接入农场存取`

---

### Task 5: Buy / Sell

**Files:**
- Create: `server/internal/farm/economy.go`, `economy_test.go`

- [x] **Step 1: 红测** — 金币 1000 买白萝卜种子 count=1 → coin=875、种子+1；金币不足 → 1302；卖果实加币

- [x] **Step 2: 实现 Buy/Sell（仅种子/果实；校验 unlock）

- [x] **Step 3: Commit** `feat: 实现商店买种子与出售果实`

---

### Task 6: Gateway 接线 + 扩展 Snapshot

**Files:**
- Modify: `server/internal/gateway/ws.go`（或新建 `ws_actions.go`）、`envelope` 旁命令常量
- Modify: `server/internal/farm/snapshot.go` — plots 带成熟时间等 online 所需字段；bag/warehouse
- Test: gateway 测试 Till 成功/失败

**Interfaces:**
- cmd：206,208,210,212,214,216,220,302,304
- 非本人 owner → 1401；先 Advance 再动作
- Clock：可用服务端 `time.Now().UnixMilli()`；测试可注入 Clock 接口

- [x] **Step 1–4: TDD 接线、测试、Commit** `feat: 网关接入种植与商店命令`

---

### Task 7: smoke 种植闭环

**Files:**
- Modify: `server/cmd/smoke/main.go`（或 `smoke_plant.go`）
- Modify: Makefile/`scripts` 若需 `FARM_TIME_SCALE=bench` 或 debug 调时 API

**说明：** 若无调时 API，smoke 可在同进程测 farm 包；或 Gateway 增加仅测试构建的 `DebugSetNow`（**禁止生产默认开启**）。推荐：**smoke 以包测+轻量 WS**——对 Growing 地块用 store 直接改 MatureAt 再 Enter/Harvest 仅作持久化验证时要小心。更干净：`farm` 单测覆盖时间；smoke WS 路径用 `bench` 档 + `sleep` 短等待，或服务端 `FARM_SMOKE_CLOCK=1` 时接受 `DebugAdvance(ms)` 仅非生产。

**推荐实现：** 在 `farm-server` 读 `FARM_ALLOW_DEBUG_TIME=1` 时注册内部 cmd 或 HTTP `POST /api/debug/advance` 把 Actor 内 now 偏移；smoke 默认开启该环境变量。

- [x] **Step 1: 实现 debug 调时（受环境变量门控）**

- [x] **Step 2: smoke：注册→买种子→Till→Plant→advance→Harvest→Sell 断言 coin/仓库**

- [x] **Step 3: Commit** `feat: 扩展 smoke 覆盖种植与买卖闭环`

---

## 2b — 客户端 online

### Task 8: session + net 动作 API + applyPatch

**Files:**
- Create: `client/src/net/session.js`, `client/src/game/applyPatch.js`
- Modify: `client/src/net/client.js`

- [x] **Step 1: `session.js`** — `isOnline`、`uid`、`token`、`enterOnline()`

- [x] **Step 2: client 增加 `plotAction(cmd, plotIndex, arg)`、`buy`、`sell`**

- [x] **Step 3: `applyPatch` 把 snapshot/plot/coin/inventory 写入现有 state 形状（state 字段名映射：`matureTime`←`mature_at` 等）

- [x] **Step 4: Commit** `feat: 客户端增加 online 会话与动作补丁`

---

### Task 9: main.js online 分支

**Files:**
- Modify: `client/src/game/main.js`、可选 `DevNetPanel`（登录成功后 `enterOnline`）
- Modify: UI 商店购买路径

- [x] **Step 1: 登录+EnterFarm 成功后 `applyPatch(snapshot)` 并 `session.isOnline=true`**

- [x] **Step 2: `onPlotClick`：online 时发对应 cmd，等 Rsp；失败 toast；成功 applyPatch**

- [x] **Step 3: 商店 Buy/Sell 同样**

- [x] **Step 4: 未登录保持本地 doTill 等路径不变**

- [x] **Step 5: Vite proxy 脚本验证一轮种收；Commit** `feat: 游戏主逻辑接入 online 种植意图`

---

## 2c — 期末

### Task 10: Fertilize + 多季

**Files:**
- Modify: `actions.go`、`advance.go`、`gameconf`、gateway cmd 218、client online 施肥
- Test: 施肥前移 MatureAt 且不改 SeasonDuration；多季 Harvest 进入下一季 Growing

- [x] **Step 1–4: TDD 实现、smoke 可选扩展、Commit** `feat: 实现施肥与多季作物跨季`

---

## Self-Review (author)

1. **Spec coverage:** 2a/2b/2c 均有 Task；非目标未写入实现任务  
2. **Placeholders:** 调时方案已选定为环境变量门控 debug advance  
3. **Types:** Aggregate 将含 Items；Plot 布局保持架构 5.2；错误码扩展集中在 pkgerr  

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-26-phase2-planting-authority.md`.

**两种执行方式：**

**1. Subagent-Driven（推荐）** — 每 Task 新子代理 + 审查；指派用中文；模型按 `AGENTS.md`

**2. Inline Execution** — 本会话 executing-plans 连续推进

选哪个？
