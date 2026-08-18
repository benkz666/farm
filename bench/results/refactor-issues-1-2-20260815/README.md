# 重构问题 1/2 性能复测

环境：同一套本机 K3s，Farm 1 副本（1 CPU）、Gateway 3 副本（各 1 CPU），Redis/MySQL、夹具、连接速率和发压 Pod 保持不变。

| 接口 | 旧记录 | 修复后 | 结论 |
|---|---:|---:|---|
| EnterFarm Actor 冷启动 | 10,830 QPS，P99 255.6 ms | 11,840 QPS，P99 101.4 ms | 吞吐 +9.3%，P99 -60.3% |
| SyncFarm（三轮中位数） | 81,286 QPS，P99 226.1 ms | 81,127 QPS，P99 128.6 ms | 吞吐 -0.2%（正常抖动），P99 -43.1% |

所有正式复测均为 100% 成功。

## 问题与修复

1. 原“Actor 冷启动”使用 `water` 夹具。15,000 账号重置期间状态会继续成熟，先重置的账号在测量前已跨过时间窗口，EnterFarm 实际混入了状态推进、Delta 发布和持久化，并非纯 Actor 恢复。现改用时间稳定的 `hot-economy` 夹具，只保留“Redis 快照热、Farm Actor 冷”的变量。
2. Gateway 重构删除了新鲜房间水位的 SyncFarm 快速响应，导致每个已追平 Sync 都跨 Gateway→Farm gRPC 和 Actor 邮箱。现恢复仅限本人农场、序号完全匹配、2 秒内新鲜水位的传输层快路径；好友农场、过期水位、缺失 Delta 及时间推进仍进入 Farm。
3. 保留并优化进入 Farm 后的路径：复用 Actor 调用定时器、复用 Sync 执行状态、已追平响应延迟到 Gateway 组装，并为可信内部流增加扁平 Sync 消息，减少 protobuf 对象和逃逸分配。

## 原始结果

- `enter-cold-cache-hot.json`
- `sync/round-1.json`
- `sync/round-2.json`
- `sync/round-3.json`
