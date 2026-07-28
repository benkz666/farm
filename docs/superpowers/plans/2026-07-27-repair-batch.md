# 已完成修复批次提交重构计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to execute this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 `d35a4f0` 之后已完成但未提交的修复重组为 15 个独立、可验证的提交。

**Architecture:** 本计划不重新实现功能。每个任务仅从当前工作树选择属于同一行为的 diff hunks，连同已有回归测试、生成物和必要文档暂存；共享文件用 `git add -p` 分割。任务按依赖顺序提交，使每个提交都能通过本任务验证。

**Tech Stack:** Go 1.22+、MySQL、Redis、Prometheus、Three.js、Node.js、Git。

## Global Constraints

- 规格：`docs/superpowers/specs/2026-07-27-repair-batch.md`。
- 基线固定为 `d35a4f0`；禁止改写已有历史或用 reset/checkout 丢弃工作树内容。
- 所有提交必须是中文 Conventional Commit，且不使用 `--no-verify`。
- 这是既有修复的历史重构：不增加 Claude 最终审查的 Important 项，不以“整理提交”为名修改业务语义。
- 对共享文件只暂存任务对应 hunks；每次提交后运行任务列出的命令并保留输出。
- `docs/2026-07-27-repair-task.md` 与 `server/cmd/mig9/main.go` 删除并随本计划的文档提交落库；`.superpowers/sdd/progress.md` 不提交。

---

## File Structure

```text
docs/superpowers/specs/2026-07-27-repair-batch.md  # 本批次范围和验收
docs/superpowers/plans/2026-07-27-repair-batch.md  # 本文件：提交边界和验证
server/...                                         # 已完成的服务端修复，按任务选择 hunks
client/...                                         # 已完成的客户端修复，按任务选择 hunks
config/crops.csv + tools/gen-config/...            # P2-2 唯一配置来源与生成器
demo/...                                           # 最后独立归档，不和生产修复混合
```

### Task 0: 建立修复批次规格与重构计划

**Files:**
- Create: `docs/superpowers/specs/2026-07-27-repair-batch.md`
- Create: `docs/superpowers/plans/2026-07-27-repair-batch.md`
- Delete: `docs/2026-07-27-repair-task.md`
- Delete: `server/cmd/mig9/main.go`

**Interfaces:**
- Produces: P0-1 至 P3-4 与 demo 提交的唯一范围、顺序和验收命令。

- [ ] **Step 1:** 暂存本任务的两个新文档和两个删除，不暂存任何业务修复。
- [ ] **Step 2:** 检查 `git diff --cached --check` 无空白错误，确认计划中不含未决 Important 项。
- [ ] **Step 3:** 提交：

```text
docs: 记录已完成修复批次的规格与提交计划
```

### Task 1: 无损邮件迁移与迁移执行

**Files:**
- Modify: `server/migrations/008_mail_schema_align.sql`
- Modify: `Makefile`（仅 `migrate` 的全量 SQL 循环 hunk）
- Modify: `scripts/run.sh`（仅全量 SQL 循环 hunk）
- Modify: `README.md`（仅迁移执行说明 hunk）

**Interfaces:**
- Produces: 可重复执行且不会无条件删除 `mail` 表的 008 迁移；所有 SQL 文件按字典序执行。

- [ ] **Step 1:** 仅暂存邮件表形态检查、条件改名/备份及迁移循环 hunks；排除 Makefile 的 `gen` hunks 和 README 的 admin 文档。
- [ ] **Step 2:** 在 MySQL 验证新表、旧表、重复执行和异常表结构；异常形态必须失败而不 DROP 数据。
- [ ] **Step 3:** 提交：

```text
fix: 防止邮件迁移重复执行时删除数据
```

### Task 2: 非开发环境密钥启动校验

**Files:**
- Modify: `.env.example`（仅 token、invite、hazard 密钥说明 hunk）
- Modify: `server/cmd/farm-server/main.go`（仅环境与密钥校验 hunk）
- Modify: `server/cmd/farm-server/main_test.go`（密钥校验测试）

**Interfaces:**
- Produces: `checkSecrets` 拒绝非开发环境的默认值或不足长度密钥。

- [ ] **Step 1:** 暂存 `checkSecrets`、配置读取和表驱动回归测试；不带入关停、admin、连接 ID 或草虫盐注入 hunks。
- [ ] **Step 2:** 运行：

```bash
cd server && go test ./cmd/farm-server -run 'Test(CheckSecrets|LoadConfig)'
```

- [ ] **Step 3:** 提交：

```text
fix: 非开发环境强制校验鉴权密钥
```

### Task 3: Actor 周期写回与优雅疏散

**Files:**
- Modify: `server/internal/actor/actor.go`（`RequireFlush` 与同步落盘 hunk）
- Modify: `server/internal/actor/runtime.go`（周期 flush、shutdown、超时和日志 hunk）
- Modify: `server/internal/actor/runtime_test.go`
- Create: `server/internal/obs/logger.go`
- Create: `server/internal/obs/logger_test.go`
- Modify: `server/cmd/farm-server/main.go`（drain 顺序与超时 hunk）
- Modify: `server/internal/gateway/ws_actions.go`（买卖 `RequireFlush` hunk）
- Modify: `server/internal/farmrpc/server.go`（买卖 `RequireFlush` hunk）

**Interfaces:**
- Produces: `Runtime.Shutdown(ctx)` 在 HTTP 停服后尝试 flush 在线 Actor；资产变动路径可请求同步写入。

- [ ] **Step 1:** 只暂存写回、drain 和同步资产提交相关 hunks；不带入 metrics、hazard 注入或 cross 处理。
- [ ] **Step 2:** 运行：

```bash
cd server && go test -race ./internal/actor ./internal/gateway ./internal/farmrpc ./cmd/farm-server
```

- [ ] **Step 3:** 提交：

```text
fix: Actor 周期落盘并在退出时疏散状态
```

### Task 4: 持久化跨农场预占与冻结金币

**Files:**
- Create: `server/internal/farm/cross_pending.go`
- Modify: `server/internal/farm/aggregate.go`（`CrossPending`、`AddItem` hunk）
- Modify: `server/internal/farm/actions.go`（lazy expire hunk）
- Modify: `server/internal/farm/daily.go`, `server/internal/farm/daily_test.go`
- Create: `server/migrations/009_player_cross_pending.sql`
- Modify: `server/internal/store/mysql.go`, `server/internal/store/store.go`
- Delete: `server/internal/cross/visitor.go`
- Modify: `server/internal/cross/types.go`, `server/internal/cross/visitor_actor.go`, `server/internal/cross/visitor_test.go`
- Modify: `server/internal/gateway/ws_cross.go`, `server/internal/gateway/gateway_test.go`
- Modify: `server/internal/farmrpc/server.go`

**Interfaces:**
- Produces: `Aggregate.ReserveCross`、`TakeCrossReservation`、`RollbackCross`、`ExpireCrossPending`；`player.cross_blob` 保存 pending。

- [ ] **Step 1:** 暂存预占状态机、MySQL `cross_blob` 读写、009 迁移和对应测试；排除同文件的逻辑日、结构化日志与其他协议 hunks。
- [ ] **Step 2:** 运行：

```bash
cd server && go test -race ./internal/store ./internal/farm ./internal/cross ./internal/gateway ./internal/farmrpc
```

- [ ] **Step 3:** 提交：

```text
fix: 持久化跨农场预占以保护冻结金币
```

### Task 5: 移除主人侧全局锁并限制去重缓存

**Files:**
- Modify: `server/internal/cross/owner.go`, `server/internal/cross/owner_test.go`
- Modify: `server/internal/actor/actor.go`（结果缓存 hunk）
- Modify: `server/internal/actor/runtime_test.go`（结果缓存测试 hunk）
- Modify: `server/internal/farm/actions.go`（`clonePlot` hunk）
- Modify: `server/internal/farm/snapshot_test.go`（Stealers 深拷贝测试 hunk）

**Interfaces:**
- Produces: Owner 裁决在 Actor 串行区内完成；返回的 `Plot` 不与 Aggregate 的 `Stealers` 底层数组共享；去重表容量受限。

- [ ] **Step 1:** 暂存 owner 去锁、有界去重和深拷贝 hunks；不宣称该内存缓存跨重启持久。
- [ ] **Step 2:** 运行：

```bash
cd server && go test -race ./internal/actor ./internal/cross ./internal/farm
```

- [ ] **Step 3:** 提交：

```text
fix: 收紧跨农场裁决并隔离地块快照
```

### Task 6: 固化已提交动作的成功语义与 WS 写时限

**Files:**
- Modify: `server/internal/gateway/ws.go`（write deadline hunk）
- Modify: `server/internal/gateway/ws_actions.go`（任务推进失败记录日志 hunk）
- Modify: `server/internal/gateway/ws_room.go`（访问任务失败记录日志 hunk）
- Modify: `server/internal/gateway/ws_task_mail_test.go`
- Modify: `server/internal/farmrpc/server.go`（任务推进失败记录日志 hunk）

**Interfaces:**
- Produces: 已提交的操作保持业务成功响应；辅助任务失败写日志；每次 WebSocket 写入设置 deadline。

- [ ] **Step 1:** 暂存成功响应与慢客户端保护 hunks，避免混入批量广播或连接租约改动。
- [ ] **Step 2:** 运行：

```bash
cd server && go test -race ./internal/gateway ./internal/farmrpc
```

- [ ] **Step 3:** 提交：

```text
fix: 已提交动作不再因辅助任务失败而报错
```

### Task 7: 确定性草虫与权威地块字段

**Files:**
- Modify: `server/go.mod`, `server/go.sum`（xxhash 直接依赖 hunk）
- Modify: `server/internal/gameconf/health.go`
- Modify: `server/internal/farm/advance.go`, `server/internal/farm/advance_test.go`
- Modify: `server/internal/farm/actions.go`, `server/internal/farm/actions_test.go`
- Modify: `server/internal/farm/aggregate.go`（`HazardSalt` hunk）
- Modify: `server/internal/farm/delta.go`, `server/internal/farm/snapshot.go`, `server/internal/farm/snapshot_test.go`, `server/internal/farm/steal.go`
- Modify: `server/internal/cross/owner.go`, `server/internal/gateway/ws_actions.go`, `server/internal/farmrpc/server.go`（`PlotChangeOf` hunks）
- Modify: `server/cmd/farm-server/main.go`, `server/cmd/farm-server/main_test.go`
- Modify: `server/internal/actor/runtime.go`, `server/internal/actor/runtime_test.go`
- Create: `client/src/game/plotInfo.js`, `client/src/game/plotInfo.test.js`
- Modify: `client/src/game/main.js`（地块信息计算 hunk）

**Interfaces:**
- Produces: 由 secret salt、用户、地块、季节和植株 nonce 派生的可重放草虫；Snapshot/Delta 包含 `health`、`stolen_count`、`fert_mask`。

- [ ] **Step 1:** 暂存草虫、权威字段、盐注入和枯萎安全渲染 hunks；排除作物 CSV、客户端重连与页面生命周期 hunks。
- [ ] **Step 2:** 运行：

```bash
cd server && go test ./cmd/farm-server ./internal/actor ./internal/farm ./internal/cross ./internal/gateway ./internal/farmrpc
cd ../client && node --test src/game/plotInfo.test.js
```

- [ ] **Step 3:** 提交：

```text
feat: 实现确定性草虫并下发权威地块状态
```

### Task 8: 生成双端作物配置并保持历史 ID

**Files:**
- Create: `config/crops.csv`, `tools/go.mod`, `tools/gen-config/main.go`, `tools/gen-config/main_test.go`
- Delete: `config/.gitkeep`
- Create: `server/internal/gameconf/gen_crops.go`, `client/src/game/gen/crops.js`
- Modify: `server/internal/gameconf/crops.go`, `server/internal/gameconf/crops_test.go`, `server/internal/gameconf/time.go`
- Modify: `server/internal/farm/actions.go`, `server/internal/farm/economy.go`, `server/internal/farm/economy_test.go`, `server/internal/farm/level_test.go`
- Modify: `client/src/game/config.js`, `client/src/game/applyPatch.js`
- Create: `client/src/game/cropsConfig.test.js`
- Modify: `Makefile`（`gen`、`gen-check` hunks）

**Interfaces:**
- Produces: `config/crops.csv` 为唯一数据源；生成的 Go/JS 作物表；`SeasonDurationMs` 使用整数分钟。

- [ ] **Step 1:** 同时暂存 CSV、生成器、两端生成物和消费者 hunks；保留既有 ID `4=apple`，不把 `migrate` hunks 混入。
- [ ] **Step 2:** 运行：

```bash
cd tools && go test ./gen-config/ -count=1
cd .. && make gen-check
cd client && node --test src/game/cropsConfig.test.js src/game/applyPatch.test.js && npm run build
```

- [ ] **Step 3:** 提交：

```text
feat: 生成双端作物配置并兼容历史 ID
```

### Task 9: 客户端超时、退避重连与权威房间恢复

**Files:**
- Modify: `client/src/net/client.js`
- Create: `client/src/net/client.reconnect.test.js`
- Create: `client/src/game/reconnectRestore.js`, `client/src/game/reconnectRestore.test.js`
- Modify: `client/src/game/main.js`（重连绑定与 EnterFarm 恢复 hunks）

**Interfaces:**
- Produces: `NetClient` 请求超时和带抖动的退避重连；`bindFarmReconnectRestore` 应用服务端权威快照。

- [ ] **Step 1:** 暂存网络状态机、恢复模块和对应 main 接线；不带入 pagehide/dispose hunks。
- [ ] **Step 2:** 运行：

```bash
cd client && node --test src/net/client.test.js src/net/client.reconnect.test.js src/game/reconnectRestore.test.js && npm run build
```

- [ ] **Step 3:** 提交：

```text
feat: 客户端支持请求超时和权威重连恢复
```

### Task 10: FarmDelta 批量推送与严格信封

**Files:**
- Create: `server/internal/wireenv/envelope.go`, `server/internal/wireenv/envelope_test.go`, `server/internal/wireenv/envelope_strict_test.go`
- Modify: `server/internal/farmrpc/push.go`, `server/internal/farmrpc/push_test.go`
- Create: `server/internal/farmrpc/push_batch_test.go`
- Modify: `server/internal/gateway/envelope.go`, `server/internal/gateway/http_auth.go`, `server/internal/gateway/room.go`, `server/internal/gateway/room_test.go`, `server/internal/gateway/ws.go`, `server/internal/gateway/ws_room.go`, `server/internal/gateway/gateway_test.go`
- Create: `server/internal/gateway/envelope_strict_test.go`, `server/internal/gateway/push_batch_test.go`
- Modify: `server/internal/farm/delta.go`（`DeltaRingCapacity=200` hunk）

**Interfaces:**
- Produces: `wireenv.DecodeEnvelope` 和 `DecodeFarmDelta` 单值严格解码；按目标 Gateway 的 `PushBatch` 单次编码发送。

- [ ] **Step 1:** 暂存 wireenv、批处理、接收端验证和 ring 容量 hunks；不带入连接租约或 metrics hunks。
- [ ] **Step 2:** 运行：

```bash
cd server && go test -race ./internal/wireenv ./internal/farmrpc ./internal/gateway
```

- [ ] **Step 3:** 提交：

```text
feat: 按网关批量推送 FarmDelta 并严格校验信封
```

### Task 11: 可观测性基线

**Files:**
- Create: `server/internal/obs/admin.go`, `server/internal/obs/admin_test.go`, `server/internal/obs/metrics.go`, `server/internal/obs/metrics_test.go`
- Modify: `server/go.mod`, `server/go.sum`（Prometheus 依赖 hunk）
- Modify: `.env.example`（admin 地址 hunk）, `README.md`（admin/metrics hunk）
- Modify: `server/cmd/farm-server/main.go`, `server/cmd/farm-server/main_test.go`
- Modify: `server/internal/actor/runtime.go`
- Create: `server/internal/actor/runtime_metrics_test.go`
- Modify: `server/internal/store/store.go`, `server/internal/cross/owner.go`, `server/internal/gateway/http_auth.go`, `server/internal/gateway/room.go`, `server/internal/gateway/room_test.go`, `server/internal/gateway/ws.go`, `server/internal/gateway/ws_cross.go`, `server/internal/farmrpc/push.go`, `server/internal/farmrpc/push_batch_test.go`, `server/internal/farmrpc/server.go`

**Interfaces:**
- Produces: 独立 admin listener 的 `/healthz`、`/readyz`、`/metrics`、`/debug/pprof/`；Prometheus 指标和 `slog` 记录。

- [ ] **Step 1:** 暂存 obs 包、装配、指标观测和 admin 文档 hunks；不带入密钥、drain、批推送或租约实现 hunks。
- [ ] **Step 2:** 运行：

```bash
cd server && go test -race ./internal/obs ./internal/actor ./internal/store ./internal/gateway ./internal/farmrpc ./cmd/farm-server
```

- [ ] **Step 3:** 提交：

```text
feat: 接入健康探针指标与结构化日志
```

### Task 12: 农场核心基准与零分配检查

**Files:**
- Create: `server/internal/farm/advance_alloc_test.go`, `server/internal/farm/advance_bench_test.go`
- Modify: `docs/design/capacity-and-benchmark.md`

**Interfaces:**
- Produces: `BenchmarkAdvance`、`BenchmarkHazard` 等基准以及能发现额外分配的回归检查。

- [ ] **Step 1:** 暂存基准、分配检查与对应容量验收文档。
- [ ] **Step 2:** 运行：

```bash
cd server && go test ./internal/farm && go test ./internal/farm -run '^$' -bench 'Benchmark(Advance|Settle|Hazard|Yield)' -benchmem -count=1
```

- [ ] **Step 3:** 提交：

```text
test: 增加农场热路径基准与零分配检查
```

### Task 13: Redis 连接成员租约与随机连接 ID

**Files:**
- Modify: `server/internal/connreg/registry.go`, `server/internal/connreg/registry_test.go`
- Modify: `server/internal/gateway/http_auth.go`, `server/internal/gateway/ws.go`, `server/internal/gateway/gateway_test.go`
- Create: `server/internal/gateway/conn_id_test.go`, `server/internal/gateway/connreg_lease_test.go`
- Modify: `server/internal/farmrpc/push_test.go`

**Interfaces:**
- Produces: ZSET score 表示成员过期时间；`allocateConnID` 以 `crypto/rand` 种子防止 Gateway 重启复用 ID。

- [ ] **Step 1:** 暂存 Redis 租约、续租、随机连接 ID 及测试；排除批量 push 的接口改造 hunks。
- [ ] **Step 2:** 运行：

```bash
cd server && go test -race ./internal/connreg ./internal/gateway ./internal/farmrpc
```

- [ ] **Step 3:** 提交：

```text
feat: 使用成员租约并避免重用连接 ID
```

### Task 14: Three.js 资源与页面生命周期释放

**Files:**
- Create: `client/src/game/dispose3d.js`, `client/src/game/dispose3d.test.js`
- Modify: `client/src/game/crops.js`
- Create: `client/src/game/crops.sharedMat.test.js`
- Modify: `client/src/game/farm3d.js`
- Create: `client/src/game/farm3d.dispose.test.js`, `client/src/game/pageLifecycle.js`, `client/src/game/pageLifecycle.test.js`
- Modify: `client/src/game/main.js`（`pagehide`、tick、pointermove、dispose hunks）

**Interfaces:**
- Produces: `FarmScene.dispose()` 与 `bindPageUnload()` 幂等释放独占对象；共享材质由 `farmSharedMat` 识别而不释放。

- [ ] **Step 1:** 暂存 dispose 工具、场景释放、共享材质标记、pagehide 接线和测试；不带入重连恢复 hunks。
- [ ] **Step 2:** 运行：

```bash
cd client && node --test src/game/dispose3d.test.js src/game/crops.sharedMat.test.js src/game/farm3d.dispose.test.js src/game/pageLifecycle.test.js && npm run build
```

- [ ] **Step 3:** 提交：

```text
fix: 释放 Three.js 场景与页面生命周期资源
```

### Task 15: 独立归档 demo

**Files:**
- Create: `demo/index.html`, `demo/assets/style.css`
- Create: `demo/js/audio.js`, `demo/js/config.js`, `demo/js/game.js`, `demo/js/main.js`, `demo/js/sprites.js`, `demo/js/store.js`, `demo/js/ui.js`

**Interfaces:**
- Produces: 根目录演示的完整静态资源；不被 Vite 生产客户端或服务端构建引用。

- [ ] **Step 1:** 仅暂存 `demo/`，确认没有把任何 repair 文件带入索引。
- [ ] **Step 2:** 用静态文件服务器打开 `demo/index.html`，确认控制台无加载错误。
- [ ] **Step 3:** 提交：

```text
chore: 归档独立农场演示资源
```

## Final Verification

- [ ] **Step 1:** 运行服务端全量测试：

```bash
cd server && go test ./...
```

- [ ] **Step 2:** 运行客户端全量测试与构建：

```bash
cd client && node --test src/**/*.test.js && npm run build
```

- [ ] **Step 3:** 运行生成检查并检查工作树：

```bash
cd .. && make gen-check && git status --short
```

- [ ] **Step 4:** 使用与实现者不同的审查模型，按任务提交范围进行最终审查；未解决的 Claude Important 项只记录为后续工作，不视为本批次回归。
