# Farm

本地联调用的农场游戏工程：Vite + Vue 3 客户端（`client/`）与 Go 服务端（`server/`，待实现）。

## 前置依赖

- Docker（MySQL 8.4、Redis 7）
- Node.js 18+（客户端）
- Go 1.22+（服务端，后续 Task）

## 本地启动

1. **配置环境变量**

   ```bash
   cp .env.example .env
   ```

   按需修改 `.env` 中的连接串与密钥（开发环境可保持默认）。

2. **启动依赖**

   ```bash
   make compose-up
   ```

   启动 MySQL 与 Redis。可用 `docker compose -f deploy/compose.yml ps` 查看状态。

3. **迁移并启动服务端**

   ```bash
   make migrate && make run
   ```

   > 当前 `migrate` / `run` 为占位，后续 Task 会接入 Go 服务。

4. **另开终端启动客户端**

   ```bash
   make client-dev
   ```

   浏览器打开 Vite 提示的本地地址即可。

## Makefile 目标

| 目标 | 说明 |
|------|------|
| `make compose-up` | 启动 MySQL + Redis |
| `make compose-down` | 停止并移除 compose 容器 |
| `make migrate` | 执行数据库迁移（待实现） |
| `make run` | 启动 `farm-server`（待实现） |
| `make client-dev` | 启动 Vite 开发服务器 |
| `make test` | 运行测试（待实现） |
| `make smoke` | 冒烟测试（待实现） |
