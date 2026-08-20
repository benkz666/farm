# Phase 0+1 Bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 落地工程骨架与期 1 联通切片：Vite+Vue 客户端可玩本地 demo，Go 单进程服务完成注册/登录 → Handshake → EnterFarm 快照，Redis+MySQL 可重启恢复，`make smoke` 通过。

**Architecture:** 单二进制 `farm-server` 内嵌 Gateway/Auth/Actor/Farm；存储经 `FarmStore`/`SessionStore` 接口访问 MySQL（权威）与 Redis（session + 农场热缓存）；客户端 `client/` 为 Vite+Vue3，three.js 场景与本地权威玩法暂留，`src/net/` 仅联调。

**Tech Stack:** Go 1.22+、MySQL 8、Redis 7、Docker Compose、Vite、Vue 3、three.js、JSON Envelope（`farm.v1.json`）

## Global Constraints

- 上游规格：`docs/superpowers/specs/2026-07-26-engineering-standards.md`、`docs/superpowers/specs/2026-07-26-phase1-login-farm-snapshot.md`
- 协议/错误码照抄 `docs/design/protocol.md`；禁止自创错误码
- Go module path：`farm/server`
- HTTP：`POST /api/register`、`POST /api/login`；WS cmd：`100 Handshake`、`102 Ping`、`200 EnterFarm`
- 初始态：金币 1000、解锁 6 块荒地、共 18 块地；`client_config_ver` 常量 `1`
- 进他人农场：`ERR_NOT_FRIEND`（1401）；期 1 不做种植动作/Delta/压测
- 前端根目录必须是 `client/`；仓库根不得保留散落的 `js/`/`css/`/`index.html`/`vendor/`
- 提交信息：约定式前缀（`feat:`/`fix:`/`docs:`/`chore:` 等）+ **中文**说明；每 Task 结束提交一次

---

## Progress

> 更新于 2026-07-26 · 分支 `feat/phase0-phase1-bootstrap` · 账本 `.superpowers/sdd/progress.md`

| Task | 状态 | 关键提交 |
| ---: | --- | --- |
| 1 前端迁入 Vite+Vue | ✅ 完成 | `0afec0b` |
| 2 Compose/Makefile/README | ✅ 完成 | `dcaed81` |
| 3 Go 模块 / 错误码 / 迁移 | ✅ 完成 | `9912a52` |
| 4 FarmStore / SessionStore | ✅ 完成 | `d4173bc` → 修复 `3af04f0` |
| 5 Auth 注册登录 | ✅ 完成 | `46a902f` |
| 6 Actor 运行时 | ✅ 完成 | `aba17ec` → panic 修复 `69e1275` |
| 7 FarmSnapshot | ✅ 完成 | `dba0e80` |
| 8 HTTP+WS Gateway | ✅ 完成 | `4f49e00` → 网关修复 `94403e1` |
| 9 migrate / smoke | ✅ 完成 | `cde4cc8` |
| 10 net 联调面板 | ✅ 完成 | `0c05e9f` → 生产排除 `3871045` 等 |

**整期结论：** Task 1–10 全部完成；`make smoke` 与 Vite proxy 主路径已验收。后续为期 2（种植权威）另开 plan。

---

## File Structure (target)

```text
client/                         # Vite + Vue 3
  package.json
  vite.config.js
  index.html
  src/main.js
  src/App.vue
  src/net/client.js             # HTTP + WS Envelope
  src/game/                     # 迁入的原 demo（本地权威）
deploy/compose.yml
Makefile
README.md
proto/farm/v1/envelope.proto    # 文档化 schema（期 1 运行时用手写 JSON，不强制 codegen）
server/
  go.mod
  cmd/farm-server/main.go
  migrations/001_init.sql
  internal/pkgerr/codes.go
  internal/store/{store.go,mysql.go,redis.go,memory_testhelpers.go}
  internal/auth/auth.go
  internal/actor/runtime.go
  internal/farm/{plot.go,aggregate.go,snapshot.go}
  internal/gateway/{http.go,ws.go,envelope.go}
  internal/gameconf/const.go
  scripts/smoke.sh
```

---

### Task 1: 前端迁入 `client/` + Vite Vue 3 壳 · ✅ 完成

**Files:**
- Create: `client/package.json`, `client/vite.config.js`, `client/index.html`, `client/src/main.js`, `client/src/App.vue`, `client/src/style.css`
- Move: 根目录 `js/*` → `client/src/game/`；`css/style.css` → 合并或 `@import`；`vendor/*` 改为 npm `three`
- Delete: 根目录 `index.html`, `js/`, `css/`, `vendor/`（迁完后）

**Interfaces:**
- Produces: `npm run dev` 可打开本地权威 3D demo；`three` 从 npm 解析

- [x] **Step 1: 创建 Vite Vue 工程文件**

在仓库根执行（不要用交互式 create）：

```bash
mkdir -p client/src/game
```

写入 `client/package.json`：

```json
{
  "name": "farm-client",
  "private": true,
  "version": "0.1.0",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "vite build",
    "preview": "vite preview"
  },
  "dependencies": {
    "three": "^0.170.0",
    "vue": "^3.5.13"
  },
  "devDependencies": {
    "@vitejs/plugin-vue": "^5.2.1",
    "vite": "^6.0.0"
  }
}
```

写入 `client/vite.config.js`：

```js
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  server: { port: 5173, proxy: { '/api': 'http://127.0.0.1:8080', '/ws': { target: 'ws://127.0.0.1:8080', ws: true } } },
})
```

写入 `client/index.html`：

```html
<!DOCTYPE html>
<html lang="zh-CN">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>经典农场 · 3D</title>
  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/src/main.js"></script>
  </body>
</html>
```

写入 `client/src/main.js`：

```js
import { createApp } from 'vue'
import App from './App.vue'
import './style.css'

createApp(App).mount('#app')
```

- [x] **Step 2: 迁移游戏资源并改 three 引用**

```bash
git mv js client/src/game
git mv css/style.css client/src/style.css
# vendor 不再需要：改 farm3d 与 OrbitControls 的 import
rm -rf vendor css
git rm -f index.html 2>/dev/null || rm -f index.html
```

修改 `client/src/game/farm3d.js` 顶部：把

```js
import * as THREE from 'three'
import { OrbitControls } from './path-to-orbit'
```

改为（按文件原 import 实际内容改）：

```js
import * as THREE from 'three'
import { OrbitControls } from 'three/examples/jsm/controls/OrbitControls.js'
```

删除对 `vendor/` 的依赖。`main.js` 内相对 import 保持 `./config.js` 等即可。

- [x] **Step 3: App.vue 挂载原 UI + 启动原 main**

将原 `index.html` 的 `#scene-container` 与 `#ui` 结构贴进 `App.vue` 的 template；`onMounted` 中动态 `import './game/main.js'`（若 main 有顶层副作用则直接 import）。

确保 `#scene-container` 在 DOM 中后再加载游戏模块。

- [x] **Step 4: 安装并验证**

```bash
cd client && npm install && npm run dev
```

Expected: 浏览器打开 `http://127.0.0.1:5173` 可看到 3D 农场且可本地操作。

- [x] **Step 5: Commit**

```bash
git add client .gitignore
git add -u
git commit -m "chore: 将前端迁入 Vite + Vue 3 的 client 目录"
```

---

### Task 2: Compose、Makefile、README 骨架 · ✅ 完成

**Files:**
- Create: `deploy/compose.yml`, `Makefile`, `README.md`, `.env.example`
- Create: `config/.gitkeep`, `proto/farm/v1/.gitkeep`

**Interfaces:**
- Produces: `make compose-up` 起 MySQL+Redis；文档写清三步启动

- [x] **Step 1: 写 compose**

`deploy/compose.yml`：

```yaml
services:
  mysql:
    image: mysql:8.4
    environment:
      MYSQL_ROOT_PASSWORD: farm
      MYSQL_DATABASE: farm
      MYSQL_USER: farm
      MYSQL_PASSWORD: farm
    ports: ["3306:3306"]
    command: ["--default-authentication-plugin=caching_sha2_password", "--character-set-server=utf8mb4"]
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "-h", "127.0.0.1", "-ufarm", "-pfarm"]
      interval: 5s
      timeout: 5s
      retries: 20
  redis:
    image: redis:7-alpine
    ports: ["6379:6379"]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 20
```

`.env.example`：

```bash
FARM_HTTP_ADDR=:8080
FARM_MYSQL_DSN=farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local
FARM_REDIS_ADDR=127.0.0.1:6379
FARM_TOKEN_SECRET=dev-only-change-me
```

- [x] **Step 2: Makefile + README**

`Makefile` 最小目标：`compose-up`、`compose-down`、`migrate`、`run`、`client-dev`、`test`、`smoke`（smoke/migrate/run 可先 stub `echo TODO`，后续 Task 填实）。

`README.md` 写：复制 `.env.example` → `.env`；`make compose-up`；`make migrate && make run`；另开 `make client-dev`。

- [x] **Step 3: 验证 compose**

```bash
docker compose -f deploy/compose.yml up -d
docker compose -f deploy/compose.yml ps
```

Expected: mysql/redis healthy 或至少 running。

- [x] **Step 4: Commit**

```bash
git add deploy Makefile README.md .env.example config proto
git commit -m "chore: 添加 compose、Makefile 与本地依赖说明"
```

---

### Task 3: Go module + 错误码 + 迁移 SQL · ✅ 完成

**Files:**
- Create: `server/go.mod`, `server/internal/pkgerr/codes.go`, `server/migrations/001_init.sql`, `server/internal/gameconf/const.go`
- Test: `server/internal/pkgerr/codes_test.go`

**Interfaces:**
- Produces: `pkgerr` 常量与 protocol 数字一致；`ConfigVer = 1`；初始金币/地块常量

- [x] **Step 1: 写失败测试**

```go
package pkgerr_test

import (
  "testing"
  "farm/server/internal/pkgerr"
)

func TestProtocolCodes(t *testing.T) {
  if pkgerr.UsernameTaken != 1103 {
    t.Fatalf("ERR_USERNAME_TAKEN want 1103 got %d", pkgerr.UsernameTaken)
  }
  if pkgerr.BadCredential != 1104 {
    t.Fatalf("want 1104")
  }
  if pkgerr.Unauthorized != 1101 {
    t.Fatalf("want 1101")
  }
  if pkgerr.NotFriend != 1401 {
    t.Fatalf("want 1401")
  }
  if pkgerr.RateLimited != 1003 {
    t.Fatalf("want 1003")
  }
  if pkgerr.ConfigStale != 1007 {
    t.Fatalf("want 1007")
  }
}
```

- [x] **Step 2: 运行确认失败**

```bash
cd server && go mod init farm/server && go test ./internal/pkgerr/ -v
```

Expected: FAIL（包不存在或常量缺失）

- [x] **Step 3: 实现 codes + gameconf + SQL**

`codes.go` 导出上述常量及 `OK=0`、`BadRequest=1002`、`TokenExpired=1102`、`Internal=1001`。

`gameconf/const.go`：

```go
const (
  ConfigVer = 1
  InitialCoin = 1000
  InitialUnlockedPlots = 6
  MaxPlots = 18
)
```

`migrations/001_init.sql`：按期 1 规格建 `account`/`player`/`farm_plot` 三表。

- [x] **Step 4: 测试通过**

```bash
cd server && go test ./internal/pkgerr/ -v
```

Expected: PASS

- [x] **Step 5: Commit**

```bash
git add server
git commit -m "feat: 添加 Go 模块、协议错误码与 MySQL 迁移"
```

---

### Task 4: Store 接口 + MySQL/Redis 实现（含回填） · ✅ 完成

**Files:**
- Create: `server/internal/store/store.go`, `mysql.go`, `redis.go`, `farm_codec.go`
- Create: `server/internal/farm/plot.go`, `aggregate.go`（最小结构，供 store 编解码）
- Test: `server/internal/store/store_integration_test.go`（build tag `integration`）与纯单测 `farm_codec_test.go`

**Interfaces:**
- Produces:
  - `type SessionStore interface { Put(ctx, token string, uid uint64, ttl time.Duration) error; Get(ctx, token string) (uint64, error); Delete(ctx, token string) error }`
  - `type FarmStore interface { SaveAccount(...); LoadFarm(ctx, uid uint64) (*farm.Aggregate, error); SaveFarm(ctx, *farm.Aggregate) error }`
  - Redis key：`session:{token}`、`farm:{uid}`；farm 缓存 JSON；plot blob 单一编解码函数
- Consumes: `pkgerr`, `gameconf`, MySQL DSN / Redis addr

- [x] **Step 1: 写 Plot/Aggregate 与 codec 单测（先红）**

`plot.go`：`StateWasteland = 0`；`type Plot struct { State uint8; CropID uint16 /* 其余字段按架构置零预留 */ }`

测试：18 块荒地 round-trip gob/binary → 再解码一致。

- [x] **Step 2: 实现 codec 使单测绿**

- [x] **Step 3: 集成测试（需 compose）**

`store_integration_test.go`：

```go
//go:build integration

func TestFarmSaveLoadAndRedisBackfill(t *testing.T) {
  // 注册路径：Save 新聚合 → Redis DEL → LoadFarm 应从 MySQL 回填且 Redis 再命中
}
```

```bash
cd server && go test -tags=integration ./internal/store/ -v -count=1
```

Expected: PASS（DSN 从环境变量读，缺省用 `.env.example`）

- [x] **Step 4: Commit**

```bash
git add server/internal/store server/internal/farm
git commit -m "feat: 实现基于 MySQL 与 Redis 的 FarmStore/SessionStore"
```

---

### Task 5: Auth 注册/登录 · ✅ 完成

**Files:**
- Create: `server/internal/auth/service.go`, `token.go`
- Test: `server/internal/auth/service_test.go`（可用 sqlmock 或 integration）

**Interfaces:**
- Produces: `Register(ctx, username, password) (uid, token, err)`；`Login(...)`；密码 bcrypt；冲突 → `pkgerr.UsernameTaken`；错密 → `BadCredential`
- Token：HMAC-SHA256 随机 token 字符串，写入 `SessionStore` TTL 7d；uid 雪花或 `UNIX_ms<<10 | rand` 简化生成即可（文档注明非生产级）

- [x] **Step 1: 失败测试 — 重复用户名**

断言第二次 `Register` 返回码/错误为 `UsernameTaken`。

- [x] **Step 2: 实现最小 Auth + 测试绿**

注册事务写 `account`+`player`+18 `farm_plot`（解锁 6，状态荒地）。

- [x] **Step 3: Commit**

```bash
git commit -am "feat: 实现注册登录与会话 token"
```

---

### Task 6: Actor 运行时 · ✅ 完成

**Files:**
- Create: `server/internal/actor/runtime.go`, `actor.go`
- Test: `server/internal/actor/runtime_test.go`

**Interfaces:**
- Produces: `Runtime.Do(uid uint64, fn func(*FarmActor) error) error` 保证同 uid 串行；首次 Do 经 `FarmStore.LoadFarm`；空闲 10min 后 flush+卸载（测试可用短 TTL）

- [x] **Step 1: 测试并发同 uid 串行**

两个 goroutine 对同一 uid `Do` 递增计数，最终为 2 且无 data race（`go test -race`）。

- [x] **Step 2: 实现 Runtime 使测试通过**

- [x] **Step 3: Commit**

```bash
git commit -am "feat: 实现按 uid 串行的 Actor 运行时"
```

---

### Task 7: EnterFarm 快照 · ✅ 完成

**Files:**
- Modify: `server/internal/farm/snapshot.go`, `aggregate.go`
- Test: `server/internal/farm/snapshot_test.go`

**Interfaces:**
- Produces: `Aggregate.Snapshot() FarmSnapshotJSON` — 18 plots，`unlocked_plots`，`coin=1000` 等；`relation` 由 gateway 填 `SELF`

- [x] **Step 1: 单测初始快照形状**

断言 `len(plots)==18`、`plots[0].State==0`、`UnlockedPlots==6`、`Coin==1000`。

- [x] **Step 2: 实现 NewAggregate / Snapshot**

- [x] **Step 3: Commit**

```bash
git commit -am "feat: 实现 EnterFarm 所需的 FarmSnapshot"
```

---

### Task 8: Gateway HTTP + WebSocket · ✅ 完成

**Files:**
- Create: `server/internal/gateway/envelope.go`, `http_auth.go`, `ws.go`, `limiter.go`
- Create: `server/cmd/farm-server/main.go`
- Test: `server/internal/gateway/envelope_test.go`；可选 `httptest` 表测 register

**Interfaces:**
- Envelope JSON：`{"cmd":n,"client_seq":n,"err":n,"payload":{}}`
- `POST /api/register|login` → `{uid,token,ws_url}`；错误 `{err:code}`
- WS：`/ws`；子协议 `farm.v1.json`；Handshake 校验 token+config_ver；Ping→Pong；EnterFarm 调 Actor
- 限流：每连接 20 容量 / 10 rps → `1003`

- [x] **Step 1: Envelope 编解码单测**

- [x] **Step 2: 实现 HTTP handlers + WS 状态机**

Handshake 成功后绑定 `conn.uid`。EnterFarm：`owner==0 || owner==uid` 否则回 `err:1401`。

- [x] **Step 3: main 装配**

读环境变量；连接 MySQL/Redis；跑 migrate（或文档要求先 `make migrate`）；listen `:8080`。

静态：可选 `client/dist`；开发期前端走 Vite proxy，不必强依赖。

- [x] **Step 4: 手工冒烟（可选）**

```bash
curl -s localhost:8080/api/register -H 'content-type: application/json' -d '{"username":"a","password":"secret12"}'
```

- [x] **Step 5: Commit**

```bash
git commit -am "feat: 实现 HTTP 鉴权与 WebSocket 网关"
```

---

### Task 9: migrate / smoke / Makefile 接线 · ✅ 完成

**Files:**
- Create: `server/scripts/smoke.sh` 或 `server/cmd/smoke/main.go`
- Modify: `Makefile`

**Interfaces:**
- `make migrate` 执行 `001_init.sql`
- `make smoke`：注册→登录→WS Handshake→EnterFarm，断言 `err==0`、`coin==1000`、`unlocked_plots==6`
- 另测：停掉 server、再起、登录 EnterFarm 数据仍在；`redis-cli FLUSHDB` 后仍能回填

- [x] **Step 1: 实现 smoke 客户端（Go 或脚本）**

推荐小 Go 程序：`server/cmd/smoke/main.go`，用 `gorilla/websocket` 或 ` nhooyr.io/websocket`。

- [x] **Step 2: 跑通**

```bash
make compose-up
make migrate
make run &  # 或另终端
make smoke
```

Expected: exit 0

- [x] **Step 3: 重启与 Redis 清空回归（手写检查清单勾掉）**

- [x] **Step 4: Commit**

```bash
git commit -am "feat: 接通 migrate 与期 1 主路径 smoke"
```

---

### Task 10: 客户端 `src/net` 联调面板 · ✅ 完成

**Files:**
- Create: `client/src/net/client.js`, `client/src/components/DevNetPanel.vue`
- Modify: `client/src/App.vue`

**Interfaces:**
- `register/login` → 存 token；`connect()` WS；`handshake()`；`enterFarm(0)` 返回 snapshot
- UI：角落小面板「注册/登录/拉快照」，结果 JSON 展示；**不**写入本地 `game/state`

- [x] **Step 1: 实现 net client**

Envelope 发送与 `client_seq` 自增；按 cmd 匹配响应。

- [x] **Step 2: 挂上 DevNetPanel（仅 import.meta.env.DEV 显示亦可）**

- [x] **Step 3: 手动验证 Vite proxy 下拉到快照**

- [x] **Step 4: Commit**

```bash
git add client
git commit -m "feat: 添加登录与 EnterFarm 快照联调面板"
```

---

## Self-Review Checklist (author)

1. **Spec coverage:** 期 0 目录/compose/Vue；期 1 注册登录 Handshake EnterFarm Redis/MySQL smoke — 均有 Task。压测/种植/Delta — 明确不在计划内。
2. **Placeholders:** 无 TBD 步骤；smoke/migrate 在 Task 9 填实。
3. **Types:** `FarmStore`/`SessionStore`/`Aggregate`/`Envelope` 命名前后一致；错误码与 `pkgerr` 一致。

---

## Execution Handoff

本 plan 已于 2026-07-26 在分支 `feat/phase0-phase1-bootstrap` 上执行完毕（Subagent-Driven）。

- 进度账本：`.superpowers/sdd/progress.md`
- Agent 协议：`AGENTS.md` / `.cursor/rules/agent-protocol.mdc`
- 下一期：种植权威（期 2）——另写 `docs/superpowers/plans/` 新 plan，勿在本文件继续堆任务
