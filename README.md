# Farm · 经典农场

本地联调工程：Vite + Vue 3 客户端（`client/`）与 Go 单进程服务端（`server/`）。

## 前置依赖

- Docker（MySQL 8.4、Redis 7）
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
3. 启动 MySQL / Redis，执行迁移（含 `001` / `002` / `003`）
4. **固定端口**启动：前端 `9001`、后端 `9002`（占用则先 kill 再起）

成功后打开：

- 登录页：http://127.0.0.1:9001/login（**正式入口**：注册 / 登录后进入农场）
- 前端根路径会按会话跳到登录或农场：http://127.0.0.1:9001/
- 后端：http://127.0.0.1:9002/

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

## Makefile 目标

| 目标 | 说明 |
|------|------|
| `make run-all` | 一键启动（`./scripts/run.sh`） |
| `make stop-all` | 停止前后端（`./scripts/stop.sh`） |
| `make compose-up` | 启动 MySQL + Redis |
| `make compose-down` | 停止 compose 容器 |
| `make migrate` | 执行迁移（`001_init.sql` + `002_items.sql` + `003_friendship.sql`） |
| `make run` | 前台启动 `farm-server`（默认 `FARM_ALLOW_DEBUG_TIME=1`，供 smoke 调时） |
| `make client-dev` | 前台启动 Vite |
| `make test` | `go test ./...` |
| `make smoke` | 种植闭环冒烟（需服务已启动且 debug 调时可用） |
| `make smoke-friends` | 期 3 加好友冒烟（注册 A/B → 分享链接 → 列表 → 重复加得 1402） |
| `make smoke-room` | 期 3 房间同步冒烟（好友拜访 → 主人动作 → 访客收 `FarmDelta`） |
| `make smoke-all` | 种植 + 加好友 + 房间同步全链路（需 `FARM_ALLOW_DEBUG_TIME=1`） |
