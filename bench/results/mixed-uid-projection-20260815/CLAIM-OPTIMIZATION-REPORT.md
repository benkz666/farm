# 1 万 QPS TaskClaim / MailClaim 长尾优化报告

## 结论

在资源拓扑保持 **Farm 1 副本 2 CPU / MySQL 1 副本 2 CPU / Redis 1 副本 1 CPU / Gateway 3 副本各 1 CPU** 不变的条件下，最终 30 秒混合压测发送 300,000 个请求并全部成功，实际吞吐 10,000 QPS。

| 指标 | 最初同拓扑 | 任务批量投影后 | 原子领取后 | 最终无日志 cutoff | 相对最初 |
|---|---:|---:|---:|---:|---:|
| 成功请求 | 299,339 | 299,469 | 299,964 | 300,000 | 错误清零 |
| 整体 P95 | 75.58 ms | 60.64 ms | 59.45 ms | 51.67 ms | -31.6% |
| 整体 P99 | 1,812 ms | 180.47 ms | 137.69 ms | 107.36 ms | -94.1% |
| TaskClaim P95 | 2,725 ms | 419.69 ms | 298.78 ms | 278.29 ms | -89.8% |
| MailClaim P95 | 2,857 ms | 399.23 ms | 305.60 ms | 280.81 ms | -90.2% |
| 测量后排空 | 2,325 ms | 437 ms | 267 ms | 61 ms | -97.4% |

最终原始结果：`full-10000-cutoff-claim-2cpu.json`；最终 Farm 指标：`farm-metrics-10000-cutoff-claim-2cpu.prom`。

## 根因与修复

1. Claim 必须等待同 UID 的异步写日志投影到 MySQL。旧实现的定向投影许可不足，短时 Claim 流量形成秒级许可队列；改为有界的前台定向投影容量，并让 barrier 存在时恢复普通投影宽度。
2. 任务投影存在 N+1 SQL：每个任务逐条 `SELECT ... FOR UPDATE` 后逐条写回。在定向投影突发时放大为大量 MySQL 往返。现在按主键排序后一次批量锁定、一次多行幂等 upsert，同时保留 Stream high-water 重放语义。
3. 成功 TaskClaim/MailClaim 原来需要 `BEGIN + SELECT FOR UPDATE + 两次 UPDATE + COMMIT`。现在 TaskClaim 使用一条原子 JOIN UPDATE；MailClaim 使用一次响应元数据读取和一条原子 JOIN UPDATE。并发领取仍由 `claimed_at IS NULL` 保证只入账一次。
4. MailClaim 提交后的邮箱版本广播与农场缓存删除原来是两次 Redis 往返，现在合并为一次事务 pipeline。
5. 每个非快速 Claim 原来都会额外写入一条无业务含义的 barrier Stream 记录，再执行空物化与 ACK。现在原子读取 UID pending 列表尾部作为投影 cutoff；定向投影成功直接返回，仅失败恢复路径才写 durable barrier。最终每轮日志记录减少 6,460 条，投影批次从 56,956 降到 49,589（-12.9%）。

## 一致性验证

- TaskClaim 8 路并发：只有一个请求成功，其余均为 already-claimed，金币只增加一次。
- MailClaim 8 路并发：只有一个请求成功，其余均为 already-claimed，附件只入账一次。
- Stream 记录完整重放、部分重叠重放、COMMIT 后 ACK 前重放均保持幂等。
- 最终压测结束后 Redis journal entries、Farm journal lag、pending 均为 0。
- `go test ./...`、`go vet ./...`、相关包 `go test -race` 和 Task/Mail/WriteJournal 的真实 MySQL/Redis 集成测试通过。

## 否定实验

曾尝试用 shard 级互斥消除普通投影与定向投影的少量重复执行。一个 shard 同时承载约 469 个活跃账户，该方案把不同 UID 错误串行化，并在 Actor 内形成 5 秒队头阻塞。结果仅成功 244,857/300,000，已完整撤销。反例保存在 `full-10000-shard-lock-negative.json`，避免以后重复采用该设计。

## 未纳入本轮的既有测试问题

完整 integration 套件中的两个 Outbox 用例仍按“创建后 1 秒可领取”断言，但当前实现 `outboxInitialDelay` 为 6 秒；数据库实际行也显示 `next_attempt_at = created_at + 6000ms`。这与本轮 Claim/投影修改无关，且涉及重试业务口径，因此未在本轮擅自调整。
