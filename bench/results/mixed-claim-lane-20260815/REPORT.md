# 领取屏障隔离修复与混合压测报告

测试日期：2026-08-15  
行为模型：`normal-v1`  
固定拓扑：Gateway ×3、Farm ×1、Social ×1，应用服务均为 1 CPU / 1 GiB；MySQL 2 CPU / 4 GiB；Redis 1 CPU / 1 GiB。

## 1. 结论

1. 历史 1,000 QPS 雪崩问题已经修复。相同完整模型下，结果从
   `29,871/30,000` 成功、P95 `1.73 s`，改善为 `30,000/30,000` 成功、
   P95 `3.88 ms`。
2. 当前已验证的完整混合安全点是 **2,000 QPS**：60,000/60,000 成功，整体
   P95 `8.03 ms`、P99 `31.00 ms`，TaskClaim/MailClaim P95 分别为
   `102.31/85.19 ms`，排空 `192 ms`，全部满足既定 SLO。
3. 5,000 QPS 是明确越界点，不是系统吞吐：错误率 13.57%，整体 P95
   `2.08 s`，排空超过 5 秒。此时瓶颈已经从“跨 UID 共享 worker 队头阻塞”
   变成“领取屏障/投影的真实服务能力不足”。
4. 按现有无埋点 3,000 万 DAU 假设，Farm 的混合 SLO 容量约束由 198 个实例降为
   50 个；但驻留 Actor 约束仍需要 261 个，因此最终建议值仍为 264 个 Farm。
   这说明本次修复消除了错误的软件瓶颈，但没有改变 300 万 PCU 的内存驻留约束。

## 2. 修复内容

### 2.1 按 UID 串行、跨 UID 异步

旧实现把请求固定分到每条流的 64 个 `RouteUID % 64` worker。领取命令在 Actor
内等待投影时，会占住 worker，并阻塞哈希碰撞的无关 UID。

新实现为每个活跃 UID 创建短生命周期 sequencer：

- 同 UID 命令严格保持到达顺序，领取后的普通命令不会越过领取；
- 不同 UID 独立执行，某个玩家等待投影不会阻塞其他玩家；
- normal lane 每条流 64 并发，barrier lane 每条流 32 并发；
- 两条 lane 各自有有界容量，barrier 饱和不会反压 normal lane；
- 数据库领取事务和 Actor 奖励同步逻辑保持不变，继续依赖原有原子更新保证幂等。

### 2.2 让投影器真正优先处理前台屏障

旧自适应限流在前台写入持续存在时把投影并发压到 1～2，因此即使把配置从 4
改为 8，实际也很难生效。现在只要存在 `WaitUIDProjected` 等待者，就临时恢复到
配置的投影并发；等待者清零后再回到保护前台写路径的自适应限制。

2,000 QPS A/B 结果证明该策略有效：

| 投影上限 | 整体 P95 | 整体 P99 | TaskClaim P95 | MailClaim P95 | 结论 |
|---:|---:|---:|---:|---:|---|
| 4 | 6.33 ms | 135.48 ms | 543.90 ms | 448.70 ms | 领取 SLO 失败 |
| 8 | 8.03 ms | 31.00 ms | 102.31 ms | 85.19 ms | 通过 |

K3s 性能基线因此固定为 8 个投影上限。正常写入期间仍由自适应限流控制，不会
始终以 8 并发冲击 MySQL。

### 2.3 可观测性

新增指标：

- `farm_write_journal_barrier_waiters{reason}`；
- `farm_write_journal_barrier_wait_duration_seconds{reason}`；
- `farm_write_journal_barrier_timeouts_total{reason}`；
- `farm_write_journal_projection_active` 与已有 projection limit / journal lag；
- `farm_grpc_stream_queue_depth{lane}`；
- `farm_grpc_stream_in_flight{lane}`；
- `farm_grpc_stream_queue_wait_seconds{lane}`；
- `farm_grpc_stream_rejected_total{lane}`；
- `farm_grpc_stream_active_sequencers`。

Grafana `Farm Overview` 已新增“领取屏障与 gRPC 流隔离”一行面板。

## 3. 测试口径

- 15,000 个固定 WebSocket 和专用账号；
- 50% 本地农场、20% 访客、30% 社交池；
- 完整 31 项可执行操作，不排除 TaskClaim/MailClaim；
- 开环总 QPS，30 秒测量，512 并发预热，预热后静置 2 秒；
- 每档重启 Farm 清空热 Actor，并重新生成合法 mixed 状态；
- 夹具和 Farm 均使用 `authentic` 时间配置；
- 直连 3 个 Gateway Pod，避免 Service 目的地址影响连接分布。

既定 SLO：错误率 ≤0.1%，整体 P95 ≤50 ms，P99 ≤250 ms，领取 P95 ≤500 ms，
排空 ≤1 秒。

## 4. 阶梯结果

| 目标 QPS | 成功/发送 | 错误率 | P95 | P99 | TaskClaim P95 | MailClaim P95 | 排空 | 结论 |
|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 500 | 15,000/15,000 | 0 | 3.61 ms | 9.41 ms | 17.07 ms | 14.76 ms | 36 ms | 通过 |
| 1,000 | 30,000/30,000 | 0 | 3.88 ms | 22.12 ms | 65.77 ms | 69.92 ms | 214 ms | 通过 |
| 2,000（projectors=4） | 60,000/60,000 | 0 | 6.33 ms | 135.48 ms | 543.90 ms | 448.70 ms | 262 ms | 领取 SLO 失败 |
| 2,000（projectors=8） | 60,000/60,000 | 0 | 8.03 ms | 31.00 ms | 102.31 ms | 85.19 ms | 192 ms | 通过 |
| 5,000 | 129,647/150,000 | 13.57% | 2,084.58 ms | 5,000.32 ms | 5,001.01 ms | 5,000.10 ms | 5,001 ms | 越界 |

历史对比：

| 档位 | 修复前 | 修复后 |
|---:|---|---|
| 500 QPS TaskClaim P95 | 241.31 ms | 17.07 ms |
| 500 QPS MailClaim P95 | 231.15 ms | 14.76 ms |
| 1,000 QPS 成功数 | 29,871/30,000 | 30,000/30,000 |
| 1,000 QPS 整体 P95 | 1,726.92 ms | 3.88 ms |
| 1,000 QPS TaskClaim P95 | 3,736.68 ms | 65.77 ms |

## 5. 5,000 QPS 为什么失败

这次不再是旧的跨 UID worker 哈希碰撞：FriendList、SearchUser 等 Social 路径在
5,000 QPS 越界档仍保持约 2～3 ms P95。

指标显示 barrier lane 共处理 4,132 个领取请求，其中 3,559 个等待 lane permit
超过 5 秒；没有出现服务端 lane 容量拒绝。投影/领取速度低于约 140 次/s 的领取
到达率后，barrier lane 被持续占满。同 UID 后续命令按正确顺序等待领取，因此
normal lane 也有 3,318 个请求等待超过 5 秒。测试结束后 journal lag 回到 0，
说明这是测量窗口内的瞬时服务能力越界，而不是无法恢复的数据积压。

因此当前结论只能表述为：**2,000 QPS 已验证通过，5,000 QPS 已验证失败**。
没有执行 2,000～5,000 之间的二分搜索，不能宣称精确极限为 2,000 QPS。

## 6. 验证与后续

代码验证已通过：

- `go vet ./farmsvr/farmrpc ./shared/store ./shared/telemetry`；
- `go test ./...`；
- `go test -race ./farmsvr/farmrpc ./shared/store`；
- 新增测试覆盖“旧 `%64` 碰撞 UID 不再互相阻塞”和“同 UID 跨 lane 不乱序”。

本轮尚未执行 30 分钟稳定性测试。若要作为最终转正材料，建议下一轮固定
2,000 QPS 跑 30 分钟，并补测 3,000 QPS 快速档；生产容量结论仍应等待真实行为
埋点替换 `normal-v1` 中的频率假设。
