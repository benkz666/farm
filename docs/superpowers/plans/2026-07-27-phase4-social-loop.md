# Phase 4 Social Loop & Multiplayer Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 落地多 Gateway + 双 Farm 分片、Kafka 跨农场总线，以及互助/偷菜+狗/可偷摘要/用户名搜索/薄任务·邮件·每日登录的产品闭环。

**Architecture:** `hash(uid)%1024` 逻辑分片经路由表到 FarmServer；Gateway 无状态任意接入并用连接注册表回推；跨农场一律 `CrossAction`→Kafka→主人裁决→`CrossResult`（测试可注入内存 EventBus）；禁止 Actor 同步等待另一 Actor。

**Tech Stack:** Go 1.22+、现有 MySQL/Redis、新增 Kafka（compose）、Vue3+Vite 客户端、二进制批 Envelope `farm.v2.bin`

## Global Constraints

- 规格：`docs/superpowers/specs/2026-07-27-phase4-social-loop.md`
- 命令/错误码以 `docs/design/protocol.md` 为准；本期钉死并回写：`SearchUser=410`、`ClaimDailyLogin=614`；偷菜/狗码 `1408–1412`、`1501–1504`；推送 `PlayerDelta=9002`
- 提交：约定式前缀 + **中文**主题（`AGENTS.md`）
- 子阶段顺序：**4a → 4b → 4c → 4d**；未完成 4a 不得开始依赖 Kafka 的玩法联调
- 分支：自当前 `main` 开 `feat/phase4-social-loop`
- 子代理模型按 `AGENTS.md` 显式指定
- 期 4 **不**做：分片热迁移、狗升级树、模糊搜用户、压测报告

---

## File Structure (target)

```text
deploy/compose.yml                         # + Kafka (+ 可选多 farm/gateway 服务)
deploy/route-table.example.json            # 1024 逻辑片 → farm-0/farm-1
server/cmd/farm-server/main.go             # 角色：gateway | farm | all（开发）
server/cmd/smoke/main.go                   # help / steal / daily 子命令
server/internal/routing/shard.go           # hash(uid)%1024 + RouteTable
server/internal/routing/shard_test.go
server/internal/bus/bus.go                 # EventBus 接口
server/internal/bus/memory.go              # 测试/单测
server/internal/bus/kafka.go               # 默认实现
server/internal/cross/types.go             # CrossAction / CrossResult / req_id
server/internal/cross/visitor.go           # 预占 / 结算 / 超时
server/internal/cross/owner.go             # 裁决（互助+偷菜入口）
server/internal/connreg/registry.go        # uid/conn → gatewayId（Redis）
server/internal/gateway/...                # 路由转发、连接注册、跨农场入口
server/internal/farm/steal.go              # 偷菜额度 / HarvestRound
server/internal/farm/pet.go                # 狗状态、粮、拦截
server/internal/store/steal_hint.go        # Redis 可偷摘要
server/internal/store/mail.go / task.go
server/migrations/004_pet_mail_task.sql
docs/design/protocol.md                    # 登记 410 / 614 等
client/src/net/client.js                   # Cross 动作、SearchUser、Task/Mail/Daily、Pet
client/src/game/main.js / ui.js            # 拜访互助/偷菜、面板
```

**内部 Farm RPC（钉死）：** Gateway→Farm 使用 **HTTP JSON** `POST /internal/v1/cmd`（带服务间 token）；跨农场 **不**走该 RPC，只走 EventBus/Kafka。

---

## 4a — 分片骨架 + Kafka 总线

### Task 1: 分支 + 路由表

**Files:**
- Create: `server/internal/routing/shard.go`, `shard_test.go`, `deploy/route-table.example.json`
- Modify: `.env.example`（`FARM_ROLE`, `FARM_INSTANCE_ID`, `FARM_ROUTE_TABLE`, `FARM_LOGICAL_SHARDS=1024`）

**Interfaces:**
- Produces: `func LogicalShard(uid uint64) int`；`type RouteTable` with `FarmID(uid uint64) (string, error)`

- [x] **Step 1:** 检出 `feat/phase4-social-loop`

- [x] **Step 2:** 红测 — 同一 uid 稳定映射；示例表把 0..511→`farm-0`、512..1023→`farm-1`

- [x] **Step 3:** 实现并 Commit `feat: 实现 uid 逻辑分片与路由表`

---

### Task 2: EventBus 接口 + 内存实现

**Files:**
- Create: `server/internal/bus/bus.go`, `memory.go`, `memory_test.go`

**Interfaces:**
- Produces:
```go
type EventBus interface {
  Publish(ctx context.Context, topic string, key string, payload []byte) error
  Subscribe(ctx context.Context, topic string, handler func(key string, payload []byte) error) error
  Close() error
}
```
- Topics 常量：`cross.action`, `cross.result`（可按环境加前缀）

- [x] **Step 1:** 红测 — Publish 后 Subscribe handler 收到同一 key/payload

- [x] **Step 2:** 实现 MemoryBus；Commit `feat: 添加可注入的跨农场 EventBus 接口`

---

### Task 3: Kafka EventBus + compose

**Files:**
- Create: `server/internal/bus/kafka.go`
- Modify: `deploy/compose.yml`、`.env.example`（`FARM_KAFKA_BROKERS`, `FARM_BUS=kafka|memory`）

- [x] **Step 1:** compose 增加 Kafka（单 broker 开发配置即可）

- [x] **Step 2:** KafkaBus 实现；集成测可用 build tag `kafka` 或跳过无 broker

- [x] **Step 3:** Commit `feat: 接入 Kafka 作为默认跨农场总线`

---

### Task 4: 进程角色拆分（gateway | farm | all）

**Files:**
- Modify: `server/cmd/farm-server/main.go`、`Makefile`/`scripts/run.sh`
- Create: 内部 HTTP mux `server/internal/farmrpc/...`

**Interfaces:**
- `FARM_ROLE=all`：单进程开发默认（内存 bus 可）
- `FARM_ROLE=gateway`：只连客户端 + 转发 `/internal`
- `FARM_ROLE=farm`：只载本实例逻辑片 Actor + 消费 Kafka + 提供 `/internal/v1/cmd`

- [x] **Step 1:** 红测/冒烟 — `all` 模式原有 `go test` 与登录 EnterFarm 不回归

- [x] **Step 2:** 实现角色开关与 farm 内部 cmd 转发骨架（先转发现有 EnterFarm/Till）

- [x] **Step 3:** Commit `feat: 拆分 gateway 与 farm 进程角色`

---

### Task 5: 连接注册表 + Delta 回推跨 Gateway

**Files:**
- Create: `server/internal/connreg/registry.go`
- Modify: `gateway` WS 生命周期、Farm Delta 推送路径

**Interfaces:**
- `Register(ctx, uid, connID, gatewayID)` / `Unregister` / `Lookup(uid) []ConnRef`
- Farm 广播时查注册表，HTTP/回调推到目标 Gateway 的 push 端点

- [x] **Step 1:** 红测 — 注册后 Lookup 命中；Unregister 后空

- [x] **Step 2:** 接入 WS connect/disconnect；双 Gateway 手工或 smoke：A 连 gw0、B 连 gw1 同房收 Delta

- [x] **Step 3:** Commit `feat: 实现跨 Gateway 连接注册与 Delta 回推`

---

### Task 6: compose 双实例编排 + 4a smoke

**Files:**
- Modify: `deploy/compose.yml`、`scripts/run.sh`、`README.md`、`server/cmd/smoke`

- [x] **Step 1:** 文档化启动：MySQL/Redis/Kafka + farm-0 + farm-1 + gateway-0 + gateway-1

- [x] **Step 2:** smoke：两用户落不同逻辑片，互相 EnterFarm（好友）成功

- [x] **Step 3:** Commit `feat: 编排双分片本地联调并扩展 smoke`

---

## 4b — 互助（浇水/除草/除虫）

### Task 7: Cross 类型与访客预占状态机

**Files:**
- Create: `server/internal/cross/types.go`, `visitor.go`, `visitor_test.go`
- Modify: `pkgerr` 若缺超时路径确认 `1004`

**Interfaces:**
```go
type CrossAction struct {
  ReqID, Kind, VisitorUID, OwnerUID uint64
  PlotIndex uint8
  // Kind: Water / RemoveWeed / RemovePest / Steal
}
type Pending struct { /* Reserved → Settled|RolledBack */ }
```
- 超时 5s 回滚

- [x] **Step 1:** 红测 — 预占后超时回滚；重复 Result 幂等

- [x] **Step 2:** 实现；Commit `feat: 实现跨农场访客预占与超时回滚`

---

### Task 8: 主人裁决互助 + 接线 Gateway

**Files:**
- Create: `server/internal/cross/owner.go`, `owner_test.go`
- Modify: `gateway` 拜访态 Water/Weed/Pest 走 Cross 而非本地 NotOwner
- Modify: 维护次数字段（player/日计数，按策划 150）

- [x] **Step 1:** 红测 — 非好友拒绝；已浇过返回已有码；成功 Commit + 返回 Result

- [x] **Step 2:** Kafka/Memory 往返联调；房间 Delta 仍广播

- [x] **Step 3:** Commit `feat: 接入好友互助浇水除草除虫`

---

### Task 9: smoke 互助

- [x] **Step 1:** A/B 跨片；B 拜访 A；B 浇水成功得经验；失败回滚计数

- [x] **Step 2:** Commit `feat: 扩展 smoke 覆盖跨农场互助`

---

## 4c — 偷菜 + 狗 + 可偷摘要

### Task 10: 偷菜领域规则

**Files:**
- Create: `server/internal/farm/steal.go`, `steal_test.go`
- Modify: `plot`/`aggregate` 增加 `StolenCount` / `HarvestRound` 若缺
- Modify: `pkgerr` 确认 `1408–1412`

- [x] **Step 1:** 红测 — 40% 上限、一轮一次、收获竞争 `1216`、数量截断

- [x] **Step 2:** 实现；Commit `feat: 实现偷菜额度与收获竞争规则`

---

### Task 11: 狗 + 赔付冻结

**Files:**
- Create: `server/internal/farm/pet.go`, `pet_test.go`
- Modify: migrations `004_*.sql`、商店 Buy 狗/狗粮、`PetStatus/Activate/Feed`
- Cross Steal：预冻单价×10；拦截 `1411` 实扣转主人；不足 `1412`

- [x] **Step 1:** 红测 — 空盆拦截率 0；拦截转账；不足发起失败

- [x] **Step 2:** 实现命令 500/502/504；Commit `feat: 实现看家狗与偷菜赔付冻结`

---

### Task 12: 可偷摘要 Redis

**Files:**
- Create: `server/internal/store/steal_hint.go`
- Modify: FriendList 响应字段；成熟/收获/偷后异步更新

- [x] **Step 1:** 红测 — 写 hint 后 FriendList 读到 `has_stealable`

- [x] **Step 2:** Commit `feat: 实现 FriendList 可偷摘要`

---

### Task 13: smoke 偷菜场景

- [x] **Step 1:** 额度竞争、收获竞争、狗拦截转账

- [x] **Step 2:** Commit `feat: 扩展 smoke 覆盖偷菜与狗拦截`

---

## 4d — 搜索 / 任务邮件每日登录 / 客户端

### Task 14: SearchUser=410

**Files:**
- Modify: `protocol.md`、`envelope.go`、`store` 按 username 查、`ws_friends`、限流
- Client: `client.js` + 好友面板

- [x] **Step 1:** 红测 — 精确命中 / 未命中 / 限流

- [x] **Step 2:** Commit `feat: 实现按用户名精确搜索加好友`

---

### Task 15: 邮件 + 任务 + 每日登录=614

**Files:**
- Create: migrations、`store/mail.go`、`store/task.go`、gateway handlers
- Modify: `protocol.md` 登记 `ClaimDailyLogin=614`
- 系统邮件附件走 `MailClaim`；任务与每日登录通过任务页直接入账，`TaskClaim` / `MailClaim` / `ClaimDailyLogin` 均幂等

- [x] **Step 1:** 红测 — 同一服务器本地自然日重复每日登录 → `1005`；任务领奖直接入账

- [x] **Step 2:** Commit `feat: 实现薄任务邮件与每日登录奖励`

---

### Task 16: PlayerDelta 推送

**Files:**
- Modify: gateway push、`client` onPush；互助成功经验、偷菜仓库、赔付金币

- [x] **Step 1:** 单测/联调 — 访客收到 `9002` 且 UI 金币/仓更新

- [x] **Step 2:** Commit `feat: 接入 PlayerDelta 推送个人状态`

---

### Task 17: 客户端拜访互助/偷菜/面板

**Files:**
- Modify: `main.js`/`ui.js`/`onlineActions.js`/`FriendsPanel` 或内联 UI
- 拜访开放 Water/Weed/Pest/Steal；等 Rsp；可偷标记；搜索；任务/邮件/每日登录入口；狗面板

- [x] **Step 1:** `npm run build` + 冒烟关键路径

- [x] **Step 2:** Commit `feat: 客户端接入互助偷菜与日常面板`

---

### Task 18: README 期 4 演示 + 双分片验收

**Files:**
- Modify: `README.md`

- [x] **Step 1:** 文档：双实例启动、演示剧本（搜好友→拜访→互助→偷菜→每日登录）

- [x] **Step 2:** 双浏览器按规格 §9 勾验；写报告

- [x] **Step 3:** Commit `docs: 更新 README 期 4 双分片社交演示步骤`

---

## Self-Review (author)

1. **Spec coverage:** 4a 拓扑/Kafka/注册表；4b 互助；4c 偷菜+狗+摘要；4d 搜索/任务邮件每日登录/客户端 — 均有 Task。热迁移/狗升级/模糊搜在非目标。  
2. **Placeholders:** 内部 RPC 钉死 HTTP JSON；cmd `410`/`614` 钉死。  
3. **Types:** CrossAction/EventBus/RouteTable 在 Interfaces 给出。  

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-27-phase4-social-loop.md`.

**Two execution options:**

**1. Subagent-Driven (recommended)** — 每 Task 派实现子代理 + 审查，连续跑完 4a→4d  

**2. Inline Execution** — 本会话按 executing-plans 批次推进  

Which approach?
