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
3. 启动 MySQL / Redis，执行迁移
4. **固定端口**启动：前端 `9001`、后端 `9002`（占用则先 kill 再起）

成功后打开：

- 前端：http://127.0.0.1:9001/
- 后端：http://127.0.0.1:9002/

开发页右下角有 **Net 联调** 面板。日志在 `.run/logs/`。

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
# 终端 1：后端默认 :9002
make run
# 终端 2：前端默认 :9001（见 client/vite.config.js）
make client-dev
```

## Makefile 目标

| 目标 | 说明 |
|------|------|
| `make run-all` | 一键启动（`./scripts/run.sh`） |
| `make stop-all` | 停止前后端（`./scripts/stop.sh`） |
| `make compose-up` | 启动 MySQL + Redis |
| `make compose-down` | 停止 compose 容器 |
| `make migrate` | 执行迁移（`001_init.sql` + `002_items.sql`） |
| `make run` | 前台启动 `farm-server`（默认 `FARM_ALLOW_DEBUG_TIME=1`，供 smoke 调时） |
| `make client-dev` | 前台启动 Vite |
| `make test` | `go test ./...` |
| `make smoke` | 种植闭环冒烟（需服务已启动且 debug 调时可用） |
