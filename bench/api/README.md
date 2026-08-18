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

`run-baseline.sh` 先采集空闲窗口，再采集测试窗口。`service-demand.json` 按空闲速率扣除后台开销，分别输出 Gateway、Farm、Social、MySQL、Redis 的 CPU ms/请求，三个Go服务的分配 B/请求，以及 MySQL、Redis 操作/请求。cAdvisor容器CPU使用测量窗口两端的原始counter差值，避免精确边界内只有一个scrape时`increase`返回空。缺少对应 exporter 时不会伪造为0。

设置 `CAPTURE_PPROF=1` 会同时采集三个 Go 进程的 CPU/heap profile。每次运行还会生成固定列的 `report.csv` 和可直接填写 Grafana 时间范围的 `grafana-range.json`。

## 回归对比口径门禁

“冷/热”必须拆成连接、Actor、本地读缓存和 Redis 缓存四个维度；Redis 单实例与
事件 Redis 拆分也不能混在同一张回归表中。正式压测前用
`benchmark_context.py capture` 为结果目录生成 `context.json`，其中资源配置、
拓扑和发压参数使用 JSON，并自动记录发压机；对比前执行 `compare`，返回码 2
表示两次结果不可同比。
部署 ID 和源码指纹会同时展示以证明比较的是两版代码，但不会被误判为环境变量不一致。

```bash
python3 bench/api/benchmark_context.py capture \
  --output .run/results/new/context.json \
  --environment k3s-benkz --deployment-id image-sha256-xxx \
  --service-topology '{"gateway":3,"farm":1,"social":1}' \
  --resource-profile '{"gateway":"1C1G","farm":"1C1G","social":"1C1G","redis":"1C1G"}' \
  --redis-topology single \
  --connections hot --actors cold --local-read-cache hot --redis-cache hot \
  --fixture .run/bench-data/accounts.json --load-tool .run/service-bench/bin/servicebench \
  --settings '{"mode":"gateway-ws","operation":"enter","qps":12000,"duration_seconds":10,"concurrency":15000}'

python3 bench/api/benchmark_context.py compare \
  .run/results/baseline/context.json .run/results/new/context.json
```

## capacity-100-v1 当前混合容量实验

当前正式模型是 `capacity-100-v1`：20分钟会话、每分钟5次业务交互、每会话
100个请求，峰值业务负载约33.3万QPS。实验入口和完整口径见
[`CAPACITY-ESTIMATION-MODEL.md`](../model/CAPACITY-ESTIMATION-MODEL.md)。

固定实验拓扑和资源后，用 `run_capacity_experiment.sh` 分别执行3,000、5,000、
7,000 QPS成本档、15,000 QPS候选档和写链路长稳；每档输出客户端、A/B/C/D
窗口、Kubernetes快照和Prometheus指标。候选档只要求一轮，每接口至少500样本：

```bash
python3 bench/api/capacity_slo.py \
  --model bench/model/user-behavior.capacity-100-v1.json \
  --result bench/results/capacity-100-v1-20260815/candidate-15000-final-r1/client.json \
  --output bench/results/capacity-100-v1-20260815/candidate-15000-final-r1/candidate-slo-final.json
```

资源、写链路、拓扑和数据量分别汇总，最后再代入生产工作负载。最终计算不把
CPU和内存绑定成“固定容量单元”，而是独立计算CPU、内存、连接、Actor、数据
和已验证业务密度约束并取最大值：

```bash
python3 bench/api/summarize_capacity_costs.py \
  --baseline bench/model/capacity-experiment-baseline-v1.json \
  --run-dir bench/results/capacity-100-v1-20260815/cost-03000-final-r1 \
  --run-dir bench/results/capacity-100-v1-20260815/cost-05000-final-r1 \
  --run-dir bench/results/capacity-100-v1-20260815/cost-07000-final-r1 \
  --output bench/results/capacity-100-v1-20260815/capacity-cost-summary-final.json

python3 bench/api/calculate_production_capacity.py \
  --plan bench/model/capacity-production-plan-v1.json \
  --cost-summary bench/results/capacity-100-v1-20260815/capacity-cost-summary-final.json \
  --topology-comparison bench/results/capacity-100-v1-20260815/topology-comparison-final.json \
  --dataset-summary bench/results/capacity-100-v1-20260815/static-dataset-summary-final.json \
  --output bench/results/capacity-100-v1-20260815/production-capacity-final.json
```

可用 `render_capacity_tables.py` 从机器可读结果重建31接口、成本和容量表，避免
手工抄数产生口径漂移。

## defense-60-v1 历史混合容量计算

混合压测必须把31个接口分别判定，整体时延只用于判断系统是否全局失稳。最终稳定档的两轮资源成本可直接生成不含容灾的基础容量：

```bash
python3 bench/api/defense_capacity.py \
  --model bench/model/user-behavior.defense-60-v1.json \
  --demand bench/results/defense-60-v1-20260815/final-9000-r1-demand-v3.json \
  --demand bench/results/defense-60-v1-20260815/final-9000-r2-demand-v3.json \
  --output bench/results/defense-60-v1-20260815/capacity-result.json
```
