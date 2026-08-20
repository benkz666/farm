# 经典农场（Farm）

以 2009 年经典网页 QQ 农场为蓝本还原的多人在线农场游戏后端 + H5 前端，支持账号登录、种植经营、好友互动、多人同农场实时同步等经典农场玩法。项目在"玩法复刻"之外，把**状态正确性、故障可恢复性、性能可验证性**作为同等重要的交付标准，围绕 3000 万 DAU 的容量目标做架构设计与压测验证。

详细架构设计见 [`docs/design/architecture.md`](docs/design/architecture.md)，玩法与参数设计见 [`docs/design/game-design-full.md`](docs/design/game-design-full.md)。

## 整体架构

系统由 3 个可独立部署的 Go 服务、1 套 MySQL 权威存储、3 套职责分离的 Redis 实例组成，整体呈"无状态接入层 + 按 UID 有状态分片的领域服务 + 权威持久化"的三层结构：

- **Gateway**：无状态接入层，负责鉴权 / 限流 / WebSocket 会话，按 UID 一致性哈希路由到 Farm/Social。
- **Farm**：按 UID 分片的有状态 Actor 服务，承载种植、收获、商店、仓库、宠物、任务、邮件、图鉴等核心玩法，负责状态推进调度、Delta 增量同步与跨农场 gRPC Saga。
- **Social**：好友关系权威服务，负责好友邀请 / 搜索 / 鉴权。
- **MySQL**：账号 / 农场 / 好友 / 邮件 / 任务 / outbox 等数据的权威持久化，通过 Redis Streams 写日志 + Projector 异步批量落库。
- **Redis ×3**：会话与业务缓存、写日志（journal）、在线状态（presence）三套实例职责分离，避免单线程命令队列互相阻塞。
- **客户端**：Vue 3 + Three.js 的 H5 应用，HTTPS+JSON 处理注册/登录/邀请落地，WSS+Protobuf 处理游戏内实时通信。

## 目录结构

```
.
├── client/           # Vue3 + Three.js 前端（H5）
├── server/           # Go 后端 monorepo
│   ├── cmd/          # 各可执行入口：gateway / farmsvr / socialsvr / farmctl / servicebench 等
│   ├── gateway/       # Gateway 服务实现
│   ├── farmsvr/       # Farm 服务实现（Actor / 房间 / 跨农场调度）
│   ├── socialsvr/     # Social 服务实现
│   ├── shared/        # 跨服务共享库（store、gameconfig、servicehost 等）
│   ├── proto/         # gRPC/Protobuf 接口定义
│   ├── gen/           # 由 proto 生成的代码（勿手改）
│   └── migrations/    # MySQL 数据库迁移脚本
├── deploy/           # Docker Compose、K8s、Nginx、可观测性配置
├── docs/             # 架构设计、玩法设计、API 文档、容量压测方案
├── bench/            # 容量/压测脚本与配置
├── config/           # 服务端与前端共享的游戏配置源（CSV）
├── tools/            # 配置代码生成工具（gen-config）
└── scripts/          # 本地开发辅助脚本（启动/停止/压测数据重置）
```

## 技术栈

- **后端**：Go 1.25，gRPC/Protobuf 内部通信，WebSocket + Protobuf 对外实时通信，MySQL 权威存储，Redis（会话缓存 / 写日志 / 在线状态）。
- **前端**：Vue 3、Three.js、Vite。
- **基础设施**：Docker Compose（本地一体化）、Kubernetes（生产/压测部署）、Prometheus + Grafana（可观测性）。
- **压测**：k6（HTTP/WS 业务压测）、ghz（gRPC 压测）、自研 `servicebench`（服务边界压测）。

## 快速开始

### 依赖

- Go 1.25+
- Node.js（用于 `client/` 与 `docs/api/`）
- Docker + Docker Compose

### 1. 准备环境变量

```bash
cp .env.example .env
# 按需修改 .env 中的密钥（FARM_INVITE_SECRET / FARM_HAZARD_SECRET / FARM_INTERNAL_TOKEN 等）
```

### 2. 启动本地依赖（MySQL / Redis）

```bash
make infra-up
```

### 3. 执行数据库迁移

```bash
make migrate
```

### 4. 启动后端三件套（Gateway / Farm / Social）

```bash
make run
```

### 5. 启动前端

```bash
make client-dev
```

### 6. 一键容器化启动（可选）

```bash
make compose-up   # 启动迁移 + 三个后端 + Web 及全部依赖
make compose-down # 停止
```

更多命令（生成配置、生成 proto、烟雾测试、压测基线等）见 [`Makefile`](Makefile)。

## 常用命令

| 命令 | 说明 |
| --- | --- |
| `make run` / `make stop` | 启动 / 停止本地开发的 Gateway/Farm/Social |
| `make test` | 运行后端 Go 测试 |
| `make gen` / `make gen-check` | 从 `config/*.csv` 生成服务端与前端共享配置 |
| `make proto` / `make proto-check` | 从 proto 生成 gRPC 代码并校验无漂移 |
| `make smoke-all` | 运行端到端烟雾测试（`farmctl`） |
| `make obs-up` / `make obs-down` | 启停 Prometheus/Grafana 等可观测性组件 |
| `make api-baseline` / `make api-ladder` | 接口容量基线与阶梯压测 |
| `make api-docs` | 生成 OpenAPI/AsyncAPI/gRPC 离线文档 |

## 时间缩放机制

原版农场按小时结算，为便于开发/演示/压测验证，服务端通过 `FARM_TIME_PROFILE` 统一下发时间档位，只压缩挂钟时间、不改动经济数值：

| 档位 | 比例 | 用途 |
| --- | --- | --- |
| `authentic` | 1:1 | 产品验收 |
| `fast` | 1:60 | 日常开发 |
| `demo` | 1:600 | 功能演示 |
| `bench` | 1:3600 | 压力测试 |

## 文档索引

- [架构设计](docs/design/architecture.md)
- [玩法与参数设计](docs/design/game-design-full.md)
- [每日任务配置说明](docs/design/daily-tasks.md)
- [接口压测方案](docs/plan/接口压测.md)
- [API 文档（OpenAPI/AsyncAPI）](docs/api)

## 贡献规范

- 提交信息使用「约定式前缀 + 中文描述主题」，例如 `feat: 优化写日志批量落库`、`fix: 修复偷菜幂等结算`。
- 涉及 JS/Go 混合数值计算时注意两者数值精度差异，避免跨语言精度丢失。
