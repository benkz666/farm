# 服务级性能测试

服务级测试绕过前端协议，直接压测服务边界，用来区分 Gateway、Farm、Social
以及存储层的容量。它用于定位瓶颈，不能替代 HTTP/WebSocket 全链路测试。

## 测试边界

| 被测服务 | 入口 | 下游处理 | 主要用途 |
|---|---|---|---|
| Gateway | WebSocket | Farm/Social 使用 `benchstub`；会话仍使用真实 Redis | 测 Gateway 协议转换、路由与连接处理 |
| Farm | Farm gRPC 双向流 | 真实 MySQL/Redis，账号预热后测试热 Actor | 测 Actor、业务处理、序列化与流式 RPC |
| Social | Social gRPC unary | 真实 MySQL | 测社交逻辑与数据库联合容量 |

固定资源基线为 Gateway、Farm、Social 各 1 核 1 GiB；MySQL 2 核 4 GiB；
Redis 1 核 1 GiB。负载机应至少 8 核，且测试时 CPU 不超过 70%。

## 工具

```bash
make service-bench-build
```

`servicebench` 使用开环调度，可直接测试 Farm 或 Social：

```bash
.run/service-bench/bin/servicebench \
  -mode farm-stream -target localhost:9210 -operation sync \
  -qps 80000 -duration 20s -concurrency 2048

.run/service-bench/bin/servicebench \
  -mode social-are-friends -target localhost:9204 \
  -qps 20000 -duration 20s -concurrency 1024
```

需要让 Gateway 与真实 Farm 一起接受前端二进制协议时，可使用 Go WebSocket
驱动，避免 k6 对大型嵌套快照的 JavaScript 解码先耗尽负载机：

```bash
.run/service-bench/bin/servicebench \
  -mode gateway-ws -accounts .run/bench-data/accounts.json \
  -operation enter -qps 16000 -duration 20s -concurrency 2600
```

`water`、`harvest`、`steal`、`water-visitor` 属于一次性合法状态转换：驱动会先在
测量窗口外完成登录、WebSocket、EnterFarm 与 Actor 预热，再按目标 QPS 均匀发送，
不会把全部账号在同一时刻释放。正式夹具的 `plot_indexes` 可给每个账号提供多个
独立合法地块；驱动按轮次交错账号、按账号轮转地块，每块地只执行一次。

统一接口比较时推荐使用 15,000 个账号、每个账号 18 个合法地块，并为所有接口
设置 `-fixed-connections 15000 -concurrency 15000`。这样 Gateway 连接数、热 Actor
数量和 UID 分布保持一致，一次重置最多提供 270,000 次合法状态转换。例如 15 秒
窗口足以覆盖 18,000 QPS；超过该值应缩短窗口或增加合法动作容量，驱动会直接
拒绝容量不足的档位，避免把“发完夹具”误报为吞吐极限。重复档位无需重新创建
账号，只需复用账号文件并在测量窗口外重置业务状态；重置脚本也会续期原 Token，
无需因会话 TTL 重新创建账号。

Water 的生长时长必须与 Farm 的 `FARM_TIME_PROFILE` 一致，否则 EnterFarm 预热会
重算全部地块并制造额外落盘。正式性能环境推荐使用 `authentic`，生成、重置与
Farm 容器保持同值。账号池只需创建一次：

```bash
FARM_TIME_PROFILE=authentic docker compose -p benkz -f deploy/compose.yml --profile app --profile fixture \
  run --rm --no-deps benchfixture \
  -mysql-dsn 'farm:farm@tcp(mysql:3306)/farm?parseTime=true&loc=Local' \
  -redis-addr redis:6379 -count 15000 -uid-base 17000000 \
  -concurrency 32 -profile water -time-profile authentic \
  -output /fixtures/hot-write-15000x18.json
```

之后每个测试档只重置状态并执行压测：

```bash
FARM_TIME_PROFILE=authentic make bench-fixture-reset \
  PROFILE=water FIXTURE=/fixtures/hot-write-15000x18.json

docker compose -p benkz -f deploy/compose.yml --profile app --profile loadtool \
  run --rm --no-deps servicebench \
  -mode gateway-ws -accounts /fixtures/hot-write-15000x18.json \
  -gateway-urls ws://gateway-1:9002/ws,ws://gateway-2:9002/ws,ws://gateway-3:9002/ws \
  -operation water -qps 6000 -duration 15s \
  -concurrency 15000 -fixed-connections 15000 \
  -warmup-concurrency 512 -per-connection-qps 8
```

`sync` 测试已追平的轻量增量路径；`sync-snapshot` 固定发送超前序号，测试
服务端返回完整快照的恢复路径。两者必须分开报告。

`benchstub` 只在 Gateway 隔离测试时使用，提供固定大小的 Farm 响应和固定
Social 响应，不得部署到生产环境。结果中的 `actual_qps` 按成功请求数除以包含
排空时间的总耗时计算；`timed_out=true` 表示压力明显越过容量，不应把目标 QPS
当成服务吞吐。

快速测试只需选择一个预期高点和一个越界点。观察 CPU、内存、MySQL 与 Redis
利用率后给出大致容量，不做细密的二分边界搜索。
