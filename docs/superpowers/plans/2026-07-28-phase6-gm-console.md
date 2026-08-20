# 期 6 开发 GM 控制台 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在开发环境提供可折叠 GM 控制台，安全地管理任意玩家的权威数据、切换目标农场、预览昼夜画面，并整合 Net 诊断。

**Architecture:** Gateway 暴露仅开发环境注册的 `/api/gm/v1/`，以独立 GM secret 鉴权；写操作经过目标分片的 FarmRPC，在 `actor.Runtime.Do` 内调用显式 GM 领域命令并同步落盘。客户端 GM 抽屉通过 HTTP 管理数据、通过既有 `EnterFarm` 的可选开发 grant 订阅目标农场；视觉时段只覆盖 `FarmScene` 的渲染 phase。

**Tech Stack:** Go 1.22+、现有 Gateway/FarmRPC/Actor/MySQL/Redis、Vue 3、Vite、Three.js、Node 内置测试。

## Global Constraints

- 规格：`docs/superpowers/specs/2026-07-28-phase6-gm-console.md`。
- 只有 `FARM_ENV=dev` 且 `FARM_ENABLE_GM=1` 才注册 GM 路由；生产环境必须没有 API 路径和 GM UI。
- 服务端使用高熵 `FARM_GM_SECRET`、客户端开发配置 `VITE_FARM_GM_SECRET`；不得记录 secret、Authorization、密码哈希或完整 session token。
- 所有目标玩家 Aggregate 读写只能在目标分片的 `actor.Runtime.Do(uid, ...)` 内进行；资产和农场修改必须 `RequireFlush()`。
- 一次 mutation 只修改一个数据域：农场 Aggregate、好友、任务或邮件；禁止在一个请求中组合跨域写入。
- 不直接 patch `farm.Aggregate` 导出字段；GM 写入必须通过本计划定义的领域命令并维护 `FarmSeq`、snapshot 和 Delta。
- GM 视图只扩展现有 `EnterFarm` 的开发可选 grant；不新增正式 WS 命令号或普通客户端协议。
- 画面预览不请求服务端、不调用 `/api/debug/advance`，也不改变作物进度。
- 提交使用中文 Conventional Commit。

---

## File Structure

```text
server/internal/gm/types.go              # GM 操作、响应、view grant 的共享类型
server/internal/gm/service.go            # Actor 内领域操作编排与审计输入
server/internal/gm/service_test.go
server/internal/farm/gm.go               # 玩家/背包/农场/地块/宠物 GM 领域命令
server/internal/farm/gm_test.go
server/internal/store/gm.go              # 好友、任务、邮件 GM 事务 API
server/internal/store/gm_test.go
server/internal/farmrpc/gm.go            # Gateway 到目标 Farm 的 GM RPC
server/internal/farmrpc/gm_test.go
server/internal/gateway/gm.go            # 公共 GM HTTP、secret 鉴权、grant 与审计
server/internal/gateway/gm_test.go
server/internal/gateway/ws_room.go       # EnterFarm 可选 gm_view_grant
client/src/net/gmClient.js                # GM HTTP 客户端与类型化操作函数
client/src/net/gmClient.test.js
client/src/game/gmView.js                 # 目标切换、快照应用、昼夜预览覆盖
client/src/game/gmView.test.js
client/src/game/main.js                   # 暴露可测试的 GM bridge 与 tick 预览覆盖
client/src/components/GmConsole.vue       # 可折叠 GM 抽屉及 Net 诊断
client/src/components/gmConsoleModel.js   # 无 Vue 依赖的表单、确认和操作状态
client/src/components/gmConsoleModel.test.js
client/src/App.vue                        # DEV 异步加载 GmConsole
client/src/components/DevNetPanel.vue     # 删除，诊断迁移完成后不再挂载
```

### Task 1: 开发门控、GM 鉴权与共享操作类型

**Files:**
- Create: `server/internal/gm/types.go`, `server/internal/gm/types_test.go`
- Create: `server/internal/gateway/gm.go`, `server/internal/gateway/gm_test.go`
- Modify: `server/cmd/farm-server/main.go`, `server/cmd/farm-server/main_test.go`, `.env.example`

**Interfaces:**
- Produces:

```go
package gm

const (
    MutationPlayerCurrency = "player.currency"
    MutationInventory      = "inventory"
    MutationFarm           = "farm"
    MutationPlot           = "plot"
    MutationPet            = "pet"
    MutationFriend         = "friend"
    MutationTask           = "task"
    MutationMail           = "mail"
)

type MutationRequest struct {
    RequestID string          `json:"request_id"`
    Kind      string          `json:"kind"`
    Payload   json.RawMessage `json:"payload"`
}

type Config struct {
    Enabled bool
    Secret  []byte
}
```

- `gateway.WithGM(config gm.Config, service gm.Service) Option`
- `func (g *Gateway) GMEnabled() bool`

- [ ] **Step 1: 写 GM 环境门控与 secret 鉴权的失败测试**

在 `server/internal/gateway/gm_test.go` 写表驱动测试：非 `dev`、`FARM_ENABLE_GM!=1`、空 secret、短 secret 均不会注册 `/api/gm/v1/players`；启用后缺失或错误 Bearer token 返回 401，正确 token 才进入 handler。

```go
func TestGMRouteRequiresDevEnableAndSecret(t *testing.T) {
    gateway := newGMGateway(t, gm.Config{Enabled: false})
    response := httptest.NewRecorder()
    gateway.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/gm/v1/players?username=a", nil))
    if response.Code != http.StatusNotFound {
        t.Fatalf("status = %d, want 404", response.Code)
    }
}
```

- [ ] **Step 2: 运行红测**

Run: `cd server && go test ./internal/gateway -run TestGMRouteRequiresDevEnableAndSecret -count=1`
Expected: FAIL，因为 `WithGM`、GM handler 与路由尚不存在。

- [ ] **Step 3: 实现最小门控与类型**

在 `main.go` 只在 `FARM_ENV == "dev" && FARM_ENABLE_GM == "1"` 时读取 `FARM_GM_SECRET`；拒绝少于 32 字节或开发示例值的 secret。`gateway.gmAuthorized` 使用 `subtle.ConstantTimeCompare`，并只在完整 `gm.Config.Enabled` 时将 GM mux 注册到 `Handler()`。

```go
func (g *Gateway) gmAuthorized(header string) bool {
    const prefix = "Bearer "
    if !strings.HasPrefix(header, prefix) || len(g.gmSecret) == 0 {
        return false
    }
    got := []byte(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
    return len(got) == len(g.gmSecret) &&
        subtle.ConstantTimeCompare(got, g.gmSecret) == 1
}
```

- [ ] **Step 4: 运行目标测试与全量配置入口测试**

Run: `cd server && go test ./internal/gateway ./cmd/farm-server -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```text
feat: 添加开发环境GM门控与鉴权
```

### Task 2: GM 玩家、背包、农场、地块和宠物领域命令

**Files:**
- Create: `server/internal/farm/gm.go`, `server/internal/farm/gm_test.go`
- Modify: `server/internal/farm/aggregate.go`, `server/internal/farm/delta.go`, `server/internal/farm/snapshot.go`

**Interfaces:**
- Produces:

```go
type GMPlayerCurrency struct {
    CoinDelta *int64
    CoinSet   *uint64
    ExpDelta  *int64
    ExpSet    *uint64
}

type GMInventory struct {
    ItemKey string
    Mode    string // "add" | "subtract" | "set" | "clear"
    Count   uint32
}

type GMFarmOperation struct {
    Kind string // "set_unlocked" | "reset" | "water_all" | "clear_weeds" | "clear_pests" | "mature_all" | "wither_all"
    Value uint32
}

type GMPlotOperation struct {
    PlotIndex uint8
    Kind      string
    CropID    uint16
    Value     uint32
}

func (a *Aggregate) ApplyGMPlayerCurrency(op GMPlayerCurrency) (FarmSnapshotJSON, error)
func (a *Aggregate) ApplyGMInventory(op GMInventory) (FarmSnapshotJSON, error)
func (a *Aggregate) ApplyGMFarm(now int64, op GMFarmOperation) ([]PlotChange, error)
func (a *Aggregate) ApplyGMPlot(now int64, op GMPlotOperation) ([]PlotChange, error)
func (a *Aggregate) ApplyGMPet(now int64, op GMPetOperation) (FarmSnapshotJSON, error)

var ErrGMInsufficientItem = errors.New("farm: GM item subtraction exceeds count")
```

- [ ] **Step 1: 写领域红测**

在 `gm_test.go` 覆盖：设置金币后 `RecalcLevel` 和 snapshot 一致；未知 item、扣除下溢、越界地块和不合法 crop ID 均无副作用；批量修改失败不产生部分 Delta；强制成熟后 harvest 返回产物；宠物设置后 `PetStatus()` 一致。

```go
func TestGMInventorySubtractDoesNotUnderflow(t *testing.T) {
    agg := NewAggregate(1, "gm")
    agg.Items[SeedItem(1)] = 2
    _, err := agg.ApplyGMInventory(GMInventory{
        ItemKey: string(SeedItem(1)), Mode: "subtract", Count: 3,
    })
    if !errors.Is(err, ErrGMInsufficientItem) {
        t.Fatalf("err = %v, want ErrGMInsufficientItem", err)
    }
    if got := agg.Items[SeedItem(1)]; got != 2 {
        t.Fatalf("count = %d, want unchanged 2", got)
    }
}
```

- [ ] **Step 2: 运行红测**

Run: `cd server && go test ./internal/farm -run 'TestGM' -count=1`
Expected: FAIL，因为 GM 领域方法、错误和值校验尚未实现。

- [ ] **Step 3: 实现显式、原子化领域操作**

每个 `ApplyGM*` 先验证所有参数，再复制需要批量修改的地块状态，最后一次性提交；成功时调用现有 `PlotChangeOf`、递增 `FarmSeq` 并返回变更。不得让 handler 直接赋值 `Coin`、`Plots`、`Items` 或 `Pet`。

```go
func (a *Aggregate) ApplyGMPlayerCurrency(op GMPlayerCurrency) (FarmSnapshotJSON, error) {
    nextCoin, nextExp, err := gmCurrencyValues(a.Coin, a.Exp, op)
    if err != nil {
        return FarmSnapshotJSON{}, err
    }
    a.Coin, a.Exp = nextCoin, nextExp
    a.RecalcLevel()
    a.FarmSeq++
    return a.Snapshot(), nil
}
```

- [ ] **Step 4: 运行领域包测试**

Run: `cd server && go test ./internal/farm -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```text
feat: 添加GM农场与物品领域操作
```

### Task 3: GM 好友、任务与邮件事务 API

**Files:**
- Create: `server/internal/store/gm.go`, `server/internal/store/gm_test.go`
- Modify: `server/internal/store/task.go`, `server/internal/store/mail.go`, `server/internal/store/friends.go`
- Test: `server/internal/store/task_mail_integration_test.go`

**Interfaces:**
- Produces:

```go
type GMStore interface {
    SetTaskProgress(ctx context.Context, uid uint64, taskID, progress uint32) error
    ResetDailyTasks(ctx context.Context, uid uint64, dayID int64) error
    CreateGMMail(ctx context.Context, uid uint64, title, body string, coin uint64, items map[string]uint32) (uint64, error)
    DeleteGMMail(ctx context.Context, uid, mailID uint64) error
    ClearGMMails(ctx context.Context, uid uint64) error
    ResetDailyLogin(ctx context.Context, uid uint64, dayID int64) error
}
```

- [ ] **Step 1: 写 Store 红测**

用事务 fake 或集成测试覆盖：投递附件邮件后 `ListMails` 可见且领取一次才发奖；删除不存在邮件无副作用；设任务进度不跨 UID；重置每日登录只删除目标服务器本地自然日的领取记录；加好友对称创建且重复添加返回既有语义。

- [ ] **Step 2: 运行红测**

Run: `cd server && go test ./internal/store -run 'TestGM' -count=1`
Expected: FAIL，因为 GM Store API 尚不存在。

- [ ] **Step 3: 实现事务边界**

邮件创建、删除、清空和每日状态重置必须使用已有 Store 事务模式；公开 `CreateGMMail` 复用而不是复制附件序列化和插入逻辑。任务进度 update 必须校验目标任务属于指定 uid。好友写继续使用 `AddFriends`/`RemoveFriends` 的既有对称事务。

- [ ] **Step 4: 运行 Store 测试**

Run: `cd server && go test ./internal/store -count=1`
Expected: PASS；若本机没有 MySQL，带 `integration` tag 的用例明确 skip，普通单元测试仍通过。

- [ ] **Step 5: Commit**

```text
feat: 添加GM任务邮件与好友管理接口
```

### Task 4: 分片 GM 服务、同步落盘和 Delta

**Files:**
- Create: `server/internal/gm/service.go`, `server/internal/gm/service_test.go`
- Create: `server/internal/farmrpc/gm.go`, `server/internal/farmrpc/gm_test.go`
- Modify: `server/internal/farmrpc/server.go`, `server/internal/gateway/gm.go`, `server/internal/gateway/gateway_test.go`
- Modify: `server/internal/actor/runtime.go`, `server/internal/actor/runtime_test.go`

**Interfaces:**
- Produces:

```go
type Service interface {
    SearchPlayer(ctx context.Context, username string) (PlayerRef, error)
    Snapshot(ctx context.Context, uid uint64) (GMPlayerSnapshot, error)
    Mutate(ctx context.Context, uid uint64, request MutationRequest) (GMPlayerSnapshot, error)
}

type GMPlayerSnapshot struct {
    Target   PlayerRef             `json:"target"`
    Farm     farm.FarmSnapshotJSON `json:"farm"`
    Tasks    []store.Task          `json:"tasks"`
    Mails    []store.Mail          `json:"mails"`
    Friends  []store.Friend        `json:"friends"`
}
```

- [ ] **Step 1: 写分片与 Actor 红测**

覆盖 Gateway 将 target UID 路由到正确 FarmRPC；`Mutate` 在 `Runtime.Do` 内执行一次、调用 `RequireFlush`、发布地块 Delta；持久化错误使 HTTP 返回 `ERR_INTERNAL` 且不广播成功 Delta。

```go
func TestGMMutateFlushesBeforeSuccess(t *testing.T) {
    service := newGMServiceWithFailingFlush(t)
    _, err := service.Mutate(context.Background(), 42, validCurrencyMutation())
    if !errors.Is(err, actor.ErrFlush) {
        t.Fatalf("err = %v, want flush failure", err)
    }
    if service.broadcasts() != 0 {
        t.Fatal("broadcast happened before durable mutation")
    }
}
```

- [ ] **Step 2: 运行红测**

Run: `cd server && go test ./internal/gm ./internal/farmrpc ./internal/gateway -run 'TestGM' -count=1`
Expected: FAIL，因为 GM Service/FarmRPC 路径和发布顺序尚不存在。

- [ ] **Step 3: 实现服务编排与 API**

`gateway/gm.go` 只做 Bearer 鉴权、路径/JSON 校验与 HTTP 编码；它不得持有 Aggregate。`farmrpc/gm.go` 只处理农场 Aggregate 域：路由到目标 Actor，成功同步写入后才调用既有 `publishDelta` / `publishPlayerDelta`。好友、任务、邮件的单域操作直接调用其事务化 Store API；HTTP 层拒绝任何混合多个数据域的 payload，因此不存在“Store 已提交、Aggregate 又失败”的跨域半成功路径。

- [ ] **Step 4: 运行并发与分片测试**

Run: `cd server && go test -race ./internal/gm ./internal/actor ./internal/farmrpc ./internal/gateway -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```text
feat: 接入分片GM服务与权威同步
```

### Task 5: 单次目标视图 grant 与 EnterFarm 扩展

**Files:**
- Create: `server/internal/gm/grant.go`, `server/internal/gm/grant_test.go`
- Modify: `server/internal/gateway/gm.go`, `server/internal/gateway/ws_room.go`, `server/internal/gateway/ws_room_test.go`
- Modify: `server/internal/gateway/envelope.go`, `server/internal/wireenv/envelope.go`
- Modify: `client/src/net/client.js`, `client/src/net/client.test.js`

**Interfaces:**
- Produces:

```go
type ViewGrantService interface {
    Issue(uid uint64, gmSessionID string, ttl time.Duration) (string, error)
    Consume(token string, uid uint64, gmSessionID string) error
}
```

```js
// client/src/net/client.js
enterFarm(ownerUid, { gmViewGrant = '' } = {})
```

- [ ] **Step 1: 写 grant 与视图切换红测**

覆盖 grant 首次使用成功、第二次使用失败、过期失败、目标 UID 不匹配失败；普通 `EnterFarm` 不带 grant 时保留 self/friend 权限；GM grant 成功后进入房间并接收 Delta。

- [ ] **Step 2: 运行红测**

Run: `cd server && go test ./internal/gm ./internal/gateway -run 'TestGMViewGrant' -count=1`
Expected: FAIL，因为 grant 存储与 `EnterFarm` 可选字段尚不存在。

- [ ] **Step 3: 实现一次性 grant 与现有 EnterFarm 可选字段**

grant 使用 Redis 或注入式短 TTL Store，消费必须原子删除。扩展现有 EnterFarm 请求体：

```go
type EnterFarmRequest struct {
    OwnerUID     uint64 `json:"owner_uid"`
    FromSeq      uint64 `json:"from_seq"`
    GMViewGrant  string `json:"gm_view_grant,omitempty"`
}
```

Gateway 只在 GM 开启时读取该字段；先走现有 owner/friend 授权，失败后才校验一次性 grant。成功路径必须复用 `enterRoom()`、`handleEnterFarm()` 和既有 snapshot 响应。

- [ ] **Step 4: 运行服务端与客户端网络测试**

Run:

```bash
cd server && go test ./internal/gm ./internal/gateway -count=1
cd ../client && node --test src/net/client.test.js
```

Expected: PASS。

- [ ] **Step 5: Commit**

```text
feat: 支持GM授权查看任意农场
```

### Task 6: 客户端 GM API、目标视图与画面 phase 覆盖

**Files:**
- Create: `client/src/net/gmClient.js`, `client/src/net/gmClient.test.js`
- Create: `client/src/game/gmView.js`, `client/src/game/gmView.test.js`
- Modify: `client/src/game/main.js`, `client/src/game/reconnectRestore.js`, `client/src/game/farmMirror.js`, `client/src/net/session.js`
- Modify: `client/src/game/farm3d.js`, `client/src/game/farm3d.dispose.test.js`

**Interfaces:**
- Produces:

```js
export function createGMClient({ baseURL, secret, fetchImpl = fetch }) {
  return {
    searchPlayer(username) {},
    getSnapshot(uid) {},
    mutate(uid, request) {},
    createViewGrant(uid) {},
  }
}

export function createGMViewController({
  netClient, gmClient, applyAuthoritativeFarmEnter, getCurrentPhase, setScenePhase,
}) {
  return {
    async switchTarget(username) {},
    async returnHome() {},
    setPreviewPhase(phase) {},
    followGameTime() {},
    renderPhase(gamePhase) {},
  }
}
```

- [ ] **Step 1: 写客户端红测**

`gmClient.test.js` 验证每个 HTTP 请求发送 GM Bearer secret、URL 编码用户名、错误码保留；`gmView.test.js` 验证切换 B 时只应用 grant 对应 `EnterFarm` 响应、返回 home 使用 uid 0、选择夜晚后 `renderPhase('day')` 仍渲染 night、恢复后渲染 day。

```js
test('GM 夜晚预览覆盖正常 tick，恢复跟随后回到游戏 phase', () => {
  const scenePhases = []
  const view = createGMViewController({
    setScenePhase: (phase) => scenePhases.push(phase),
    getCurrentPhase: () => 'day',
  })
  view.setPreviewPhase('night')
  view.renderPhase('day')
  view.followGameTime()
  view.renderPhase('day')
  assert.deepEqual(scenePhases, ['night', 'night', 'day'])
})
```

- [ ] **Step 2: 运行红测**

Run: `cd client && node --test src/net/gmClient.test.js src/game/gmView.test.js`
Expected: FAIL，因为模块与预览覆盖尚不存在。

- [ ] **Step 3: 实现 HTTP 客户端和 view controller**

GM HTTP client 只在 Vite DEV 创建；所有 mutation 使用 `crypto.randomUUID()` request ID。`main.js` 的 `tick()` 计算正常 phase 后调用 `gmView.renderPhase(phase)`，不再直接调用 `scene.setDayPhase(phase)`；GM bridge 暴露 `switchTarget`、`returnHome` 和 preview controller，但不暴露 secret。

- [ ] **Step 4: 运行客户端相关测试**

Run: `cd client && node --test src/net/gmClient.test.js src/game/gmView.test.js src/game/reconnectRestore.test.js src/game/farmMirror.test.js src/net/client.test.js`
Expected: PASS。

- [ ] **Step 5: Commit**

```text
feat: 接入GM目标视图与昼夜预览
```

### Task 7: 可折叠 GM 控制台与 Net 诊断迁移

**Files:**
- Create: `client/src/components/GmConsole.vue`
- Create: `client/src/components/gmConsoleModel.js`, `client/src/components/gmConsoleModel.test.js`
- Modify: `client/src/App.vue`
- Delete: `client/src/components/DevNetPanel.vue`
- Modify: `client/src/game/main.js`

**Interfaces:**
- Produces:

```js
export function createGMConsoleModel({ gmClient, gmView, getDiagnostics }) {
  return {
    isOpen: false,
    activeSection: 'player',
    target: null,
    pendingConfirmation: null,
    result: null,
    async searchAndSwitch(username) {},
    requestDangerousAction(action, targetUsername) {},
    confirmDangerousAction(typedUsername) {},
    async runMutation(kind, payload) {},
  }
}
```

- [ ] **Step 1: 写 UI 状态红测**

覆盖：默认收起；切换目标后标题更新；错误展示；危险操作在输入精确目标用户名之前不发送 mutation；Net 诊断数据读取与旧 `DevNetPanel` 一致；生产配置不加载组件。

```js
test('危险操作只有输入目标用户名后才提交', async () => {
  const calls = []
  const model = createGMConsoleModel({ gmClient: { mutate: async (...args) => calls.push(args) } })
  model.target = { uid: 7, username: 'player-b' }
  model.requestDangerousAction({ kind: 'farm.reset' }, 'player-b')
  await model.confirmDangerousAction('other')
  assert.equal(calls.length, 0)
  await model.confirmDangerousAction('player-b')
  assert.equal(calls.length, 1)
})
```

- [ ] **Step 2: 运行红测**

Run: `cd client && node --test src/components/gmConsoleModel.test.js`
Expected: FAIL，因为状态模型尚不存在。

- [ ] **Step 3: 实现模型和 Vue 抽屉**

`App.vue` 仅在 `import.meta.env.DEV` 异步加载 `GmConsole.vue`。组件收起时只显示 `GM` 按钮；展开时按规格的八个折叠组渲染，表单从显式 mutation kind 映射生成。Net 诊断迁入最后一组并复用现有 `window.__farm` 只读桥接数据。删除独立 `DevNetPanel.vue` 与它在 `App.vue` 的引用。

- [ ] **Step 4: 运行客户端测试与构建**

Run:

```bash
cd client && node --test src/components/gmConsoleModel.test.js src/net/gmClient.test.js src/game/gmView.test.js
cd client && npm run build
```

Expected: PASS。

- [ ] **Step 5: Commit**

```text
feat: 添加可折叠GM控制台并整合Net诊断
```

### Task 8: 端到端验收、文档与开发浏览器验证

**Files:**
- Modify: `.env.example`, `README.md`
- Modify: `docs/superpowers/specs/2026-07-28-phase6-gm-console.md`
- Test: `server/internal/gateway/gm_test.go`, `server/internal/farmrpc/gm_test.go`, `client/src/components/gmConsoleModel.test.js`

- [ ] **Step 1: 补开发配置和文档测试说明**

在 `.env.example` 列出不含真实值的 `FARM_ENV=dev`、`FARM_ENABLE_GM=1`、`FARM_GM_SECRET`、`VITE_FARM_GM_SECRET`；README 明确 GM 仅限本机开发、不得将 secret 写入生产环境，并给出目标玩家切换与“恢复跟随画面”的最短演示步骤。

- [ ] **Step 2: 运行完整自动化验证**

Run:

```bash
cd server && go test ./...
cd ../tools && go test ./gen-config/ -count=1
cd .. && make gen-check
cd client && node --test src/**/*.test.js && npm run build
```

Expected: PASS。

- [ ] **Step 3: 使用浏览器完成开发环境冒烟**

以 `FARM_ENV=dev`、`FARM_ENABLE_GM=1` 和本地 GM secret 启动服务与客户端；在独立浏览器 task space 登录 A，展开 GM，搜索并切换到 B，修改金币和一块地，确认页面刷新后数据保留；选择夜晚后确认场景变暗，再恢复跟随；确认 Net 诊断仅存在于 GM 抽屉。

- [ ] **Step 4: 验证生产门控**

以不含 `FARM_ENABLE_GM=1` 的配置启动 Gateway，断言 `GET /api/gm/v1/players?username=a` 返回 404，且生产构建不包含 GM 控制台入口。

- [ ] **Step 5: Commit**

```text
docs: 补充期六GM控制台开发说明
```

## Plan Self-Review

- [ ] 规格第 3 节由 Task 1、4、5 实现：环境门控、GM secret、分片 Actor 与单次 grant。
- [ ] 规格第 4 节由 Task 4、5、6 实现：HTTP mutation、EnterFarm grant、权威快照覆盖。
- [ ] 规格第 5 节由 Task 2、3、4 实现：领域操作、事务 Store API、同步持久化与 Delta。
- [ ] 规格第 6 节由 Task 6、7 实现：纯客户端昼夜覆盖、GM 抽屉和 Net 诊断迁移。
- [ ] 规格第 7、8 节由每个任务的失败无副作用测试与 Task 8 完整验收实现。
- [ ] 执行前确认 `gm_view_grant` 仅是现有 EnterFarm 的开发可选字段，且不写入 `docs/design/protocol.md`。
