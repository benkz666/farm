# 经典农场

经典农场采用 Vue 3 + Three.js 客户端，以及 Go 单仓多模块后端。后端固定由五种独立服务组成，不再提供单进程 `all` 模式。

## 后端结构

```text
server/
├── go.work                    # 本地统一开发与测试工作区
├── api/                       # 服务间 JSON-RPC 信封、客户端与协议测试
├── platform/                  # Actor、农场领域核心、存储、Kafka、观测等共享内核
├── services/
│   ├── gateway/               # 外部 HTTP/WS、连接、限流、路由、推送
│   ├── auth/                  # 注册、登录、密码哈希、会话签发
│   ├── farm/                  # UID Actor 与农场权威状态
│   ├── social/                # 好友关系与好友申请
│   └── worker/                # 任务、邮件、图鉴奖励
├── migrations/
│   ├── auth/                  # Auth 拥有的数据变更
│   ├── farm/                  # Farm 拥有的数据变更
│   ├── social/                # Social 拥有的数据变更
│   └── worker/                # Worker 拥有的数据变更
└── tools/smoke/               # 端到端冒烟客户端，不参与线上部署
```

每个 `services/*` 目录都有独立 `go.mod` 和唯一 `cmd/<service>` 入口，可以单独构建、测试和部署。`server/go.work` 只服务于单仓开发，不会生成第六个后端进程。

五个服务的职责如下：

| 服务 | 默认端口 | 职责 |
| --- | ---: | --- |
| Gateway | 9002 | 浏览器唯一入口；HTTP/WS、会话校验、连接注册、分片路由与推送 |
| Auth | 9003 | 唯一接触密码的服务；注册、登录、密码哈希、签发会话 |
| Social | 9004 | 好友权威关系、好友申请及公开用户查询 |
| Worker | 9005 | 离线可推进的任务、邮件、附件领取和图鉴里程碑奖励 |
| Farm | 9100 | UID Actor、农场状态机、经济与跨农场动作裁决 |

Gateway/Farm 与 Social/Worker/Auth 之间通过带内部令牌的 JSON-RPC 调用；跨农场动作通过 Kafka 回环。UID、邮件 ID 等 64 位标识在 JSON 中始终编码为十进制字符串。

## 本地启动

需要 Docker、Go 1.25、Node.js 和 npm。

```bash
cp .env.example .env
./scripts/run.sh
```

脚本按以下顺序执行：

1. 启动 MySQL、Redis、Kafka；
2. 按全局版本号执行尚未应用的迁移；
3. 启动 Auth、Social、Worker；
4. 启动 Farm；
5. 最后启动 Gateway 和 Vite。

页面地址为 <http://127.0.0.1:9001/>，运行日志位于 `.run/logs/`。

停止进程：

```bash
./scripts/stop.sh
./scripts/stop.sh --compose  # 同时关闭 MySQL、Redis、Kafka
```

启动脚本不再接受 `all` 或 `shards` 参数。需要验证多个 Farm 或 Gateway 实例时，应给实例配置独立监听地址、实例 ID、服务发现映射和路由表，再启动同一种服务的多个实例。

## Docker 部署

不需安装本地 Go 或 Node.js，直接用 Compose 启动完整环境：

```bash
make compose-up
# 或 docker compose -f deploy/compose.yml --profile app up -d --build
```

这会启动 MySQL、Redis、Kafka、Auth、Social、Worker、Farm、Gateway 和 Web 容器。`migrate` 是一次性迁移容器，成功后显示为 Exited 是正常的；它会按版本表跳过已应用的迁移。浏览器只需访问 <http://127.0.0.1:9001/> ，Web 容器会反向代理 `/api` 和 `/ws` 到 Gateway。

关闭整套容器：

```bash
make compose-down
```

## 常用命令

```bash
make run             # 五服务 + Vite
make stop
make test            # 全部 Go 模块测试
make gen-check       # 配置生成物漂移检查
make smoke-all       # 完整玩法冒烟
make compose-up      # 容器化启动迁移、五服务、Web 与依赖
make compose-down
```

也可以单独运行一个服务：

```bash
make run-auth
make run-social
make run-worker
make run-farm
make run-gateway
```

单独运行时仍需先准备它依赖的基础设施与下游服务；不存在把其他模块嵌回当前进程的兼容路径。

## 数据库迁移

迁移是数据库结构的版本历史。文件名中的三位数字是全仓唯一版本号，目录表示数据所有权。例如：

```text
server/migrations/social/010_friend_request.sql
```

表示第 10 个数据库版本由 Social 服务拥有，用于新增好友申请表。已经应用的迁移不得改名或修改内容；后续变化必须新增更大的版本号。`scripts/run.sh` 与 Docker 的 `migrate` 容器都会递归收集四个目录并按文件名排序，通过 `schema_migrations` 表保证每个版本只执行一次。

## 配置与安全

完整配置见 [.env.example](.env.example)。重点约束：

- 五个服务使用各自的 `FARM_<SERVICE>_HTTP_ADDR` 与 `FARM_<SERVICE>_ADMIN_ADDR`；
- `FARM_INTERNAL_TOKEN` 保护所有服务间接口；
- 非 `dev` 环境的邀请签名密钥和农场风险盐至少为 32 字节随机值；
- `FARM_ALLOW_DEBUG_TIME=1` 只允许本地测试，生产必须关闭；
- 默认本地路由表 `deploy/route-table.local.json` 将 1024 个逻辑分片全部交给 `farm-0`。

架构细节见 [architecture.md](docs/design/architecture.md)，协议约束见 [protocol.md](docs/design/protocol.md)。
