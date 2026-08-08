# Farm k6 压测脚本

脚本默认访问 `http://127.0.0.1:9002`，也可以通过 `BASE_URL` 覆盖。
先安装 k6（例如 macOS 使用 `brew install k6`，Linux 请按 k6 官方
仓库安装），再从仓库根目录运行：

```bash
# 注册后登录；默认使用 ramping-vus
BASE_URL=http://127.0.0.1:9002 \
  k6 run bench/k6/auth_load.js

# 只压注册
SCENARIO=register-only TARGET_VUS=100 \
  k6 run bench/k6/auth_load.js

# 只压登录，需要已有账号
SCENARIO=login-only USERNAME=alice PASSWORD='secret12' \
  k6 run bench/k6/auth_load.js

# 长连接承载：每个 VU 会创建独立账号并保持 WS
# ACTIVITY=idle 仅 ping；ACTIVITY=sync 时每条连接按 SYNC_INTERVAL_MS 循环 SyncFarm
TARGET_CONNECTIONS=500 CONNECTION_DURATION=10m \
  k6 run bench/k6/ws_capacity.js

ACTIVITY=sync SYNC_INTERVAL_MS=1000 TARGET_CONNECTIONS=3000 CONNECTION_DURATION=10m \
  k6 run bench/k6/ws_capacity.js

# 农场读吞吐：默认每个 VU 自行注册并登录
TARGET_VUS=100 QPS=8 \
  k6 run bench/k6/ws_read_throughput.js

# 也可复用已有账号
TARGET_VUS=100 USERNAME_TEMPLATE='k6-read-{vu}' PASSWORD='secret12' \
  k6 run bench/k6/ws_read_throughput.js
```

## Prometheus remote write

k6 的 Prometheus remote write 是实验性输出，需要同时设置地址和
`--out`：

```bash
K6_PROMETHEUS_RW_SERVER_URL=http://127.0.0.1:9090/api/v1/write \
  k6 run --out experimental-prometheus-rw bench/k6/ws_capacity.js
```

该环境变量由 k6 读取，脚本无需额外 exporter。`auth_load.js`、
`ws_capacity.js` 和 `ws_read_throughput.js` 的自定义 Trend/Counter/Gauge
都会随 k6 输出。

## 场景说明

- `auth_load.js`：SLO 为 HTTP p95 `<200ms`、HTTP 失败率 `<0.1%`。
- `ws_capacity.js`：每条连接握手后进入自己的农场，每 30 秒 ping；
  握手 p95 `<500ms` 且不允许掉线。
- `ws_read_throughput.js`：进入农场后串行 SyncFarm，单连接发送间隔
  至少 125ms（不超过 8 commands/sec），同步 p95 `<100ms`、p99 `<200ms`。
  默认每个 VU 自行注册；也可用 `USERNAME`/`USERNAME_TEMPLATE` 复用账号。
- WebSocket 必须协商 `farm.v3.pb`。`ws_url` 优先使用登录响应中的值，
  缺省时从 `BASE_URL` 推导 `/ws`。
- `client_seq` 始终是安全范围内的数字；`uid` 和 `farm_seq` 原样保留，
  避免 JavaScript 数字精度损失。
- EnterFarm、SyncFarm 与 FarmDelta 使用类型化 Protobuf；k6 仅提取热路径
  校验所需的序号/关系元数据，完整快照正确性由 Go 和浏览器契约测试覆盖。
- 注册用户名必须 ≤32 字符（`account.username VARCHAR(32)`）。
  脚本已用短前缀 + `Date.now().toString(36)` 生成。

当前 Gateway 的 SyncFarm 请求 wire 字段是 `from_seq`，返回字段是
`farm_seq`；脚本的 `syncFarm(ownerUid, farmSeq, ...)` 方法按此实际服务端
契约发送请求。

完整的前端接口基线、夹具格式和统一结果目录见
[`bench/api/README.md`](../api/README.md)。
