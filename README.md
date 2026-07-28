# Farm · 经典农场

本地联调工程：Vite + Vue 3 客户端（`client/`）与 Go 单进程服务端（`server/`）。

## 前置依赖

- Docker（MySQL 8.4、Redis 7、Kafka；双分片可选容器化 farm/gateway）
- Node.js 18+
- Go 1.22+
- `curl`、`lsof`（启动脚本用）

## 一键启动（推荐）

```bash
chmod +x scripts/run.sh scripts/stop.sh   # 只需一次
./scripts/run.sh
```

> 请用 `./scripts/run.sh`（走 bash shebang），不要用 `sh scripts/run.sh`。

脚本会：

1. 检查并安装前端依赖（`client/npm install`，若缺失）
2. 检查并下载后端 Go 模块（`go mod download`）
3. 启动 MySQL / Redis / Kafka，执行迁移（含 `001` / `002` / `003`）
4. **固定端口**启动：前端 `9001`、后端 `9002`（占用则先 kill 再起）

成功后打开：

- 登录页：http://127.0.0.1:9001/login（**正式入口**：注册 / 登录后进入农场）
- 前端根路径会按会话跳到登录或农场：http://127.0.0.1:9001/
- 后端：http://127.0.0.1:9002/
- Admin（默认仅 loopback，与业务端口隔离）：http://127.0.0.1:9300/
  - `/healthz` 进程存活；`/readyz` 就绪（draining 或依赖不可用 → 503）
  - `/metrics` Prometheus 文本；`/debug/pprof/*` 性能分析
  - 关闭：`FARM_ADMIN_ADDR=off`；改端口：`FARM_ADMIN_ADDR=127.0.0.1:19300`
  - 指标要点：`farm_delta_broadcast_batches_total` 按 PushBatch 计数（跨 Gateway 拆分时每个 Gateway +1；本地 RoomHub 一次广播 +1）；`farm_delta_broadcast_targets` 为单次 publish 的目标连接总数

未登录不能进入 `/farm`。开发态右下角「Net 诊断」仅作联调诊断，**不是**注册/进房入口。日志在 `.run/logs/`。

停止前后端：

```bash
./scripts/stop.sh
```

连同数据库一起停：

```bash
./scripts/stop.sh --compose
```

也可用 Make：`make run-all` / `make stop-all`。

## 期 4a：双分片本地联调

单进程 `all` 适合日常开发；验收跨 Farm 路由时用双分片拓扑：

| 角色 | 端口 | 职责 |
|------|------|------|
| MySQL / Redis / Kafka | 3306 / 6379 / 9094 | 共享存储与跨农场总线 |
| farm-0 | 9100 | 逻辑片 `0..511` |
| farm-1 | 9101 | 逻辑片 `512..1023` |
| gateway-0 | 9200 | 客户端 HTTP/WS（可任意接入） |
| gateway-1 | 9201 | 同上 |

**方式 A — 宿主机进程（推荐联调 / smoke）**

```bash
./scripts/run.sh shards
# 或 make run-shards

cd server && go run ./cmd/smoke shards
# 或 make smoke-shards
```

`smoke shards` 会让 A/B 落在不同物理 Farm、分别接入 gateway-0 / gateway-1，
覆盖 Farm RPC 种植/Pet/SyncFarm/任务邮件，再经 Kafka 验证跨 Farm 互助、偷菜及访客权威背包结算。

**方式 B — 全容器（compose profile）**

```bash
# 先起依赖并 migrate，再构建 farm/gateway 镜像
make migrate   # 需已 make compose-up（或 compose 已含 mysql）
make compose-shards
# Gateway: :9200 / :9201 ；Farm 内部: :9100 / :9101
make smoke-shards
```

路由表示例：`deploy/route-table.example.json`。环境变量见 `.env.example`（`FARM_ROLE` / `FARM_FARM_URLS` / `FARM_GATEWAY_URLS`）。

## 分步启动

```bash
cp -n .env.example .env
make compose-up
make migrate
# 终端 1：后端默认 :9002（本地默认 FARM_ALLOW_DEBUG_TIME=1，供 smoke 调时）
make run
# 终端 2：前端默认 :9001（见 client/vite.config.js）
make client-dev
```

## 期 3：双客户端演示（好友 + 拜访）

准备两个浏览器（或一个正常窗口 + 一个隐私窗口），服务已按上文启动。

1. **账号 A**：打开 http://127.0.0.1:9001/login →「注册」→ 进入自己的农场。
2. 打开右侧 **好友** 面板，记下「我的 UID」，或点「复制分享链接」。
3. **账号 B**：另一窗口同样注册并进入农场。
4. B 在好友面板输入 A 的 **UID**（或粘贴分享链接）→「添加」。也可让 B 打开分享落地链接 `/i/:invite`，登录后自动加好友。
5. B 在好友列表点「访问农场」进入 A 的农场：**只读**（写工具 / 商店写入隐藏，可见「返回我的农场」）。
6. A 在自己农场锄地 / 播种；B 应通过房间同步看到地块变化（`FarmDelta`）。若浏览器不便双开对照，可用下方 `make smoke-room` / `make smoke-all` 覆盖 Delta 路径。

## 期 4：双分片社交演示（搜好友 → 拜访 → 互助 → 偷菜 → 每日登录）

验收对齐 `docs/superpowers/specs/2026-07-27-phase4-social-loop.md` §9。剧本覆盖跨片加好友、互助、偷菜（含狗拦截）、用户名搜索、每日登录与任务邮件。

### 1. 启动双分片拓扑

```bash
./scripts/run.sh shards        # farm-0:9100 / farm-1:9101 / gateway-0:9200 / gateway-1:9201
# 或全容器：make compose-shards（需先 make migrate）
```

确认 4 个端口监听、Kafka 在 9094、MySQL/Redis 已迁移到 `006_account_uid_auto_increment.sql`。

### 2. 双浏览器演示剧本

`run.sh shards` 会启动前端 `:9001` 并代理到 gateway-0。另开终端启动第二个
Vite 入口并代理到 gateway-1：

```bash
cd client
FARM_GATEWAY_URL=http://127.0.0.1:9201 npm run dev -- --host 0.0.0.0 --port 9003 --strictPort
```

准备两个浏览器（或正常 + 隐私窗口）：

- **窗口 A** → http://127.0.0.1:9001/（Vite → gateway-0）
- **窗口 B** → http://127.0.0.1:9003/（Vite → gateway-1）

> 演示时让 A、B 注册的用户名尽量不同，便于「按用户名搜索」步骤区分。逻辑分片 `hash(uid) % 1024` 决定落点，A/B 大概率落在不同物理 Farm，正可验证跨农场总线。
>
> 当前浏览器客户端断线后不会自动重连，尚未实现 gateway-0 / gateway-1
> 故障后的自动跨 Gateway 切换。跨 Gateway 接入与服务端
> `(gateway_id, conn_id)` 路由由上述双入口演示；故障转移不在本演示的能力声明内。

1. **注册并进入**：A、B 各自注册登录进入自己的农场。
2. **按用户名搜索加好友**：B 打开好友面板 → 输入 A 的**用户名**（不是 UID）→「搜索并添加」。命中后即出现在好友列表；搜不到会得到明确错误码。也可继续用 UID / 分享链接（`/i/:invite`）路径。
3. **拜访**：B 在好友列表点「访问农场」进入 A 的农场。好友列表里若 A 有成熟可偷地块，会显示「🥷 有菜可偷」徽标（FriendList 可偷摘要，Redis 弱一致）。
4. **互助**：A 在自家锄地 + 播种 + 浇水让作物进入需维护状态；B 在拜访态选「浇水 / 除草 / 除虫」工具点地块。每次互助三段式跨农场：访客预占 → 主人裁决 → 访客结算（成功 +经验/+金币，失败回滚计数，超时 5s 回滚 `ERR_TIMEOUT`）。同一地块立即重复浇水会得 `AlreadyWatered`。
5. **偷菜**：等作物成熟（debug 调时：`make run` 默认 `FARM_ALLOW_DEBUG_TIME=1`，可调时加速）。B 选「偷菜」工具点成熟地块：
   - 额度 = floor(产量 × 40%) − 已偷；耗尽得 `1410`，每轮每地块最多成功一次得 `1409`。
   - 与主人收获竞争：主人先收得 `1216`。
   - A 在商店买「看家狗」并启用、喂狗粮：B 偷菜时可能被拦截得 `1411`，预冻金转给主人；B 余额不足预冻得 `1412`。
6. **每日登录 / 任务邮件**：A、B 各自打开「任务」面板 → 点「领取每日登录」；完成任务后奖励进「邮件」面板领取。重复领每日登录为幂等（重复得 `ERR_DUPLICATE_OK` 或等价码）。

### 3. smoke 验收（不依赖浏览器）

浏览器/ego 不可用时，用 smoke 覆盖规格 §9 必须场景：

```bash
make smoke-shards    # 4a 跨片加好友 + 互访 EnterFarm
make smoke-help      # 4b 互助成功 / AlreadyWatered 失败回滚（需 make run，all 模式 :9002）
make smoke-steal     # 4c 额度 / 收获竞争 1216 / 余额不足 1412 / 狗拦截 1411
make smoke-all       # 种植 + 好友 + 房间 + 互助 + 偷菜 全链路（all 模式）
```

`smoke-help` / `smoke-steal` 走单进程内存总线，验证三段式状态机与去重/超时回滚；
`smoke-shards` 走 Kafka + 双 Gateway/Farm，验证种植、任务/邮件、跨农场互助/偷菜及 Player 状态落地。
规格 §9.1 中「FriendList 摘要最终一致」「用户名搜索 + 限流」「每日登录幂等」由对应 Task 的单测/集成覆盖，演示剧本中肉眼复核。

## Makefile 目标

| 目标 | 说明 |
|------|------|
| `make run-all` | 一键启动（`./scripts/run.sh`） |
| `make stop-all` | 停止前后端（`./scripts/stop.sh`） |
| `make compose-up` | 启动 MySQL + Redis + Kafka |
| `make compose-shards` | 启动双分片 farm-0/1 + gateway-0/1（compose profile） |
| `make compose-down` | 停止 compose（含 shards profile） |
| `make migrate` | 按文件名顺序执行 `server/migrations/` 下全部迁移（当前 001 至 008） |
| `make run` | 前台启动 `farm-server`（默认 `FARM_ALLOW_DEBUG_TIME=1`，供 smoke 调时） |
| `make run-gateway` | 独立 Gateway 进程（`FARM_ROLE=gateway`） |
| `make run-farm` | 独立 Farm 进程（`FARM_ROLE=farm`，`FARM_INSTANCE_ID` 需在路由表中） |
| `make run-shards` | 双分片一键启动（`./scripts/run.sh shards`） |
| `make client-dev` | 前台启动 Vite |
| `make test` | `go test ./...` |
| `make smoke` | 种植闭环冒烟（需服务已启动且 debug 调时可用） |
| `make smoke-friends` | 期 3 加好友冒烟（注册 A/B → 分享链接 → 列表 → 重复加得 1402） |
| `make smoke-room` | 期 3 房间同步冒烟（好友拜访 → 主人动作 → 访客收 `FarmDelta`） |
| `make smoke-shards` | 双分片冒烟（RPC 种植/任务邮件 + Kafka 互助/偷菜） |
| `make smoke-help` | 期 4b 互助冒烟（浇水成功 / AlreadyWatered 失败回滚，all 模式 :9002） |
| `make smoke-steal` | 期 4c 偷菜冒烟（额度 / 收获竞争 1216 / 余额不足 1412 / 狗拦截 1411） |
| `make smoke-all` | 种植 + 加好友 + 房间同步 + 互助 + 偷菜 全链路（需 `FARM_ALLOW_DEBUG_TIME=1`） |
