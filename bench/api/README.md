# 前端 API 性能基线

这里的脚本用于形成单接口基线，不等价于 3000 万 DAU 的全链路容量验证。

## 覆盖方式

- `http_api_baseline.js` 覆盖 4 个前端 HTTP 操作。
- `ws_api_baseline.js` 的操作表覆盖 38 个前端 WebSocket 请求；`SCENARIO=all` 每个请求执行一次，单接口可用 operationId 单独运行。
- WebSocket 驱动按 `cmd + client_seq` 匹配二进制批帧中的响应，并记录收到的 9000/9002/9004/9006/9008 推送命令。
- `are_friends.sh` 使用 ghz 直接测试核心 `AreFriends` gRPC。
- `run-baseline.sh` 保存 k6 原始汇总、Prometheus 空闲/负载快照、环境指纹和单请求服务需求。

## 数据准备

复制 `fixture.example.json` 到 `.run/bench-data/accounts.json`，为一次性操作准备独立账号和合法状态。所有 64 位 ID 必须使用十进制字符串。测试期间默认禁止注册账号；只有临时冒烟可显式设置 `ALLOW_RUNTIME_REGISTER=1`。

```bash
DATA_FILE=.run/bench-data/accounts.json \
  k6 run bench/k6/ws_api_baseline.js

SCENARIO=friendList MODE=load TARGET_VUS=20 TARGET_QPS=100 DURATION=1m \
DATA_FILE=.run/bench-data/accounts.json \
  k6 run bench/k6/ws_api_baseline.js

PROTOCOL=ws DATA_FILE=.run/bench-data/accounts.json \
IDLE_BASELINE_SECONDS=60 bench/api/run-baseline.sh syncFarm

PROTOCOL=http USERNAME=bench-user-001 PASSWORD='replace-me' \
  bench/api/run-baseline.sh login

FARM_INTERNAL_TOKEN=dev-internal-token UID_VALUE=1 PEER_UID=2 \
TARGET_QPS=100 bench/ghz/are_friends.sh

# 可重复接口：100 QPS 起步，每档翻倍，失败后补测中点并重复边界档
DATA_FILE=.run/bench-data/accounts.json bench/api/run-ladder.sh syncFarm
```

`MODE=load` 只允许可重复的读操作。状态写接口使用 `shared-iterations`，每个夹具执行一次；阶梯压测前应由数据生成器为每档准备新资源池。跨农场与热点 Actor 场景继续使用 `farmctl`，避免用同一地块反复制造业务错误。

确实准备了可消费的独立状态池后，写接口可显式设置 `ALLOW_STATEFUL_LOAD=1`；否则脚本不会把业务拒绝伪装成容量数据。

## 服务需求

`run-baseline.sh` 先采集空闲窗口，再采集测试窗口。`service-demand.json` 按空闲速率扣除后台开销，分别输出 Gateway、Farm、Social 的 CPU ms/请求、分配 B/请求，以及 MySQL、Redis 操作/请求。缺少对应 exporter 时字段为 `null`，不会伪造为 0。

设置 `CAPTURE_PPROF=1` 会同时采集三个 Go 进程的 CPU/heap profile。每次运行还会生成固定列的 `report.csv` 和可直接填写 Grafana 时间范围的 `grafana-range.json`。
