# 经典农场 · 工程规范

> 状态：已评审（头脑风暴 2026-07-26）  
> 上游真相源：`docs/design/game-design-full.md`、`architecture.md`、`protocol.md`  
> 配套首期切片：`2026-07-26-phase1-login-farm-snapshot.md`

本文约定**怎么组织仓库与开发**，不重复策划数值与完整架构推导。实现时与本文冲突，以本文的「落地裁剪」为准，并回写修订说明。

---

## 1. 目标与原则

- **转正可读**：模块边界、Actor、协议、存储分层能对照架构文档讲清。
- **开发不拖**：接口按目标形态写；实现可先轻量。期 4 起跨农场默认 Kafka、逻辑分片路由表落地；完整热迁移 / ABC write-behind / 压测后置。
- **单一真相**：玩法与容量仍在 `docs/design/*`；本目录只写工程约束与分期切片。

---

## 2. 分期路线图

| 阶段 | 目标 | 主要内容 | 验收 |
| --- | --- | --- | --- |
| 0 | 规范与骨架 | 工程规范、期 1 规格、`client/` 迁入、Compose、仓库骨架 | 文档评审完成；目录就位（已合入 `main`） |
| 1 | 登录与农场快照 | 注册登录、WS 握手、EnterFarm 快照、Redis/MySQL 读写、客户端联调 | `2026-07-26-phase1-login-farm-snapshot.md`（已合入 `main`） |
| 2 | 种植权威 | `advance`、种植循环、Buy/Sell、在线意图、化肥与多季 | `2026-07-26-phase2-planting-authority.md`（已合入 `main`） |
| 3 | 多人同步与好友拜访 | 服务器权威、好友、房间、FarmDelta、序列同步、访客只读 | `2026-07-26-phase3-room-sync-friends.md`（已合入 `main`） |
| 4 | 社交闭环与多人骨架 | 分片 Gateway/Farm、Kafka 跨农场、互助、偷菜与狗、可偷摘要、搜索、任务/邮件/每日登录 | `2026-07-27-phase4-social-loop.md`（规格已评审） |
| 5 | 工程化加固 | 可靠性、配置生成、可观测性、基准、连接租约、客户端韧性 | `2026-07-27-phase5-repair-batch.md`（已合入 `main`） |
| 后续 | 压测与材料 | 机器人、容量拐点、答辩与上线材料 | 有数据与演示脚本 |

**默认后置（需要时再插入对应期）**：ABC 分档 write-behind、逻辑分片热迁移、图鉴里程碑、狗升级树、隐藏种子。

分期规格：期 1–3 已有文档；期 4、期 5 见上表链接。

---

## 3. 仓库结构

```text
farm/
├── docs/
│   ├── design/                 # 策划 / 架构 / 协议（真相源）
│   └── superpowers/
│       ├── specs/              # 工程规范与分期规格
│       └── plans/              # implementation plan
├── proto/farm/v1/              # 协议 schema（首期子集）
├── config/                     # CSV（完整管线后期）；首期可空或 stub
├── deploy/
│   └── compose.yml             # Redis 7 + MySQL 8
├── server/                     # Go module
│   ├── cmd/farm-server/
│   ├── internal/
│   │   ├── auth/
│   │   ├── gateway/
│   │   ├── actor/
│   │   ├── farm/
│   │   ├── store/
│   │   ├── gameconf/
│   │   └── pkgerr/
│   └── migrations/
├── client/                     # 全部前端（Vite + Vue 3 工程根）
│   ├── index.html
│   ├── package.json
│   ├── vite.config.js
│   ├── src/                    # Vue 应用、net、桥接 three 场景
│   ├── public/                 # 静态资源（可由原 vendor 迁入）
│   └── ...                     # 过渡期可暂留旧 js/，迁完删除
├── Makefile
└── README.md
```

生产静态资源只从 `client/` 提供。服务端通过文件服务或嵌入挂载 `client/`；`demo/` 是独立归档演示，不参与 Vite 或服务端生产资源构建。

---

## 4. 模块依赖（硬约束）

| 规则 | 说明 |
| --- | --- |
| `cmd` 只装配 | 不含业务逻辑 |
| `gateway` 不直连 MySQL 玩法表 | 鉴权后转发 Actor / 调用 auth 接口 |
| `farm` 不依赖 `gateway` | 领域可单测 |
| 上层只依赖 `store` 接口 | 禁止业务包直接持有 `redis.Client` / `sql.DB`（装配层注入实现） |
| 期 1 玩法状态仍本地权威 | Vue 应用可先壳化现有 demo；`src/net/` 只做联调，**不**接管种植权威（权威反转在期 3） |

跨「服务」调用在单进程内用 **Go interface 本地实现**；不引入 gRPC。接口命名按「将来可拆进程」设计（如 `FarmService.Enter`）。

---

## 5. 语言与工具

| 项 | 约定 |
| --- | --- |
| Go | 1.22+ |
| 前端 | **Vite** 打包；**Vue 3** 作为 UI 框架；three.js 场景继续由独立模块承载（可从现有 `farm3d.js` 迁入） |
| 序列化 | Schema 用 Protobuf；期 1 传输 **JSON**（`farm.v1.json`） |
| 本地依赖 | `docker compose` 启动 Redis 7、MySQL 8 |
| 配置 | 环境变量，见第 8 节 |

Go module 路径固定为：`farm/server`（本地模块路径；若日后推远程再改成完整 module path）。

---

## 6. 命名与术语

- 协议命令号、错误码：**照抄** `docs/design/protocol.md`，禁止自创同义名。
- 领域术语：遵循策划名词表（农场、地块、缩放小时、逻辑日等）。
- Go：包名短小；导出接口如 `FarmStore`、`SessionStore`；错误与协议名对应。
- Git 提交：约定式前缀（`feat`/`fix`/`docs`/`chore`/`refactor`/`test` 等）+ **中文**主题，例如 `feat: 实现注册登录与会话 token`；一次提交一个主题。完整 Agent 派工与风格协议见 `AGENTS.md` 与 `.cursor/rules/agent-protocol.mdc`。

---

## 7. Makefile 最小目标

```text
make compose-up | compose-down
make migrate              # 001_init.sql + 002_items.sql
make run                  # farm-server；本地默认 FARM_ALLOW_DEBUG_TIME=1
make client-dev           # cd client && npm run dev
make test                 # go test ./...
make proto                # 从 proto 生成代码（可后期接线）
make smoke                # 种植闭环（需服务已启动且 debug 调时可用）
```

---

## 8. 配置与密钥

| 变量 | 用途 |
| --- | --- |
| `FARM_MYSQL_DSN` | MySQL 连接 |
| `FARM_REDIS_ADDR` | Redis 地址 |
| `FARM_HTTP_ADDR` | HTTP/WS 监听（本地固定 `:9002`） |
| `FARM_TOKEN_SECRET` | 会话签名密钥 |
| `FARM_ALLOW_DEBUG_TIME` | 为 `1` 时开放 `POST /api/debug/advance`（仅本地/smoke；生产勿开） |

禁止将生产密钥提交入库。`deploy/compose.yml` 可含**开发默认**账号口令，并在 README 标明仅限本地。

---

## 9. 测试约定

| 层 | 要求 |
| --- | --- |
| 单测 | 初始农场、`store` 回填路径；Actor 同 uid 串行 |
| 集成 / smoke | 依赖 compose；本地 `make smoke` 必须能跑 |
| 不做 | 为刷覆盖率写空测试；期 1 不做压测 |

---

## 10. 文档职责

| 位置 | 写什么 |
| --- | --- |
| `docs/design/*` | 玩法、架构、协议、容量（少改） |
| `docs/superpowers/specs/*` | 工程规范、分期切片规格 |
| `docs/superpowers/plans/*` | 可执行的 implementation plan |
| `README.md` | 如何把当前期跑起来（三步内） |

---

## 11. 期 0 清单

- [ ] 审阅并确认本文件与期 1 规格
- [ ] 将现有前端迁入 `client/`，并用 Vite + Vue 3 初始化工程（three.js demo 可先原样挂载）
- [ ] 创建 `server/`、`deploy/compose.yml`、`proto/farm/v1/` 骨架
- [ ] 编写 README 本地启动说明
- [ ] 规格与骨架的 `docs:` / `chore:` 提交

---

## 12. 修订记录

| 日期 | 说明 |
| --- | --- |
| 2026-07-26 | 初稿：双文档方案、五期路线图、`client/` 根、Redis/MySQL + compose |
| 2026-07-26 | 前端改为期 1 起 Vite + Vue 3；three.js 场景模块并存 |
