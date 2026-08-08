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

`sync` 测试已追平的轻量增量路径；`sync-snapshot` 固定发送超前序号，测试
服务端返回完整快照的恢复路径。两者必须分开报告。

`benchstub` 只在 Gateway 隔离测试时使用，提供固定大小的 Farm 响应和固定
Social 响应，不得部署到生产环境。结果中的 `actual_qps` 按成功请求数除以包含
排空时间的总耗时计算；`timed_out=true` 表示压力明显越过容量，不应把目标 QPS
当成服务吞吐。

快速测试只需选择一个预期高点和一个越界点。观察 CPU、内存、MySQL 与 Redis
利用率后给出大致容量，不做细密的二分边界搜索。
