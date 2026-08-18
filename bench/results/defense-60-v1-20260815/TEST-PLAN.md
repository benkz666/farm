# defense-60-v1 混合容量压测计划

## 1. 目标

在固定单机K3s资源拓扑下，使用新的“20分钟会话、每分钟3次、每会话60次业务请求”模型：

1. 找到满足统一SLO的最高稳定混合QPS和第一个失败档；
2. 采集Gateway、Farm、Social、MySQL、Redis的实际CPU、内存和调用放大；
3. 采集连接、驻留Actor、write journal pending/lag及测试停止后的排空时间；
4. 在不考虑容灾的前提下，计算3000万DAU、400万PCU、20万业务QPS所需的基础实例数和单实例资源建议；
5. 用实测Social调用比例和CPU解释Social实例数为何显著少于Gateway/Farm。

## 2. 行为模型

- 文件：`bench/model/user-behavior.defense-60-v1.json`
- SHA-256：`f35cab03e7c07f51a07498e054c7ea764a9b2b838a432148c87cef74d4dcc51f`
- 全站峰值业务QPS：200,000；不含Ping/Pong、Handshake和服务器主动推送。
- 六类会话次数：进入同步6、本地农场19、好友跨农场16、任务7、邮件5、商店宠物图鉴7，合计60。
- 六类内部沿用旧31接口模型的相对比例，保证每个接口仍参与混合测试。
- 一次性加好友、删好友等破坏关系夹具的接口不混入主容量测试。

## 3. 固定测试环境

| 服务 | 副本 | 单实例资源 | 镜像摘要 |
|---|---:|---:|---|
| Gateway | 3 | 1 CPU / 1 GiB | `sha256:1d475482...d5f69` |
| Farm | 1 | 1 CPU / 1 GiB | `sha256:88f9cdcbc96b...2f707a` |
| Social | 1 | 1 CPU / 1 GiB | `sha256:4e0d218...e9f68` |
| MySQL 8.4 | 1 | 2 CPU / 4 GiB | `sha256:bced325...ae5cb` |
| Redis 7 | 1 | 1 CPU / 1 GiB | `sha256:2a51817...f4b33` |
| 压测Pod | 1 | 20 CPU / 10 GiB请求，22 CPU / 12 GiB上限 | `sha256:fcb687c...a1096` |

- 代码基准提交：`6ed9f89e22aca181192b8704ba6abb8f99ff6a96`；工作区存在未提交的本轮优化代码，因此以镜像摘要作为实际被测版本标识。
- 固定15,000条WebSocket，3个Gateway直连地址轮询分配。
- Farm使用`GOMAXPROCS=1`、`GOMEMLIMIT=800MiB`、`FARM_ACTOR_MAX_RESIDENT=20000`、`FARM_COMMITTER_SHARDS=8`、`FARM_WRITE_JOURNAL_PROJECTORS=8`。
- 时间配置使用`authentic`，夹具为`ratio-15000x18.json`。
- 每档开始前等待journal归零、重置15,000账号、重启Farm清空热Actor，再建立并预热连接与Actor。

## 4. 阶梯与重复规则

实际执行阶梯：

```text
5,000 → 6,500 → 6,800 → 7,000
→ 修复Claim长尾
→ 7,000 → 8,000 → 9,000 → 9,500 → 10,000 → 11,000 QPS
```

- 每档开环发压30秒；服务变慢时不自动降压，压测端队列满计失败。
- 每档均重新准备相同合法数据，不复用上一档被消费的任务、邮件和地块状态。
- 找到最高通过档和第一个失败档后，最高通过档换独立重置重复；本次快速容量验证完成2轮，生产认证应补1轮10分钟和1轮30分钟长稳。
- 如果7,500仍明显通过，则继续提高；如果5,000已失败，则向下补档定位。

## 5. 稳定判定

混合测试不能只看整体时延。整体指标只判断系统是否失稳，接口是否通过必须看每个接口自己的样本。以下条件必须同时满足：

| 指标 | 标准 |
|---|---:|
| 整体系统/业务错误率 | ≤0.1% |
| 31个接口各自错误率 | ≤0.1% |
| 测量窗口实际完成QPS | ≥目标的99.5% |
| 接口时延期望线（观察项） | Avg≈50 ms、P95≈100 ms、P99≈200 ms |
| 31个接口各自P95硬SLO | <300 ms |
| 31个接口各自P99硬SLO | <500 ms |
| 停止发压后的排空时间 | ≤1秒 |
| journal pending/lag | 排空后回到0 |

整体P90/P95/P99仍记录，用于画容量曲线和发现全局失稳，但不能掩盖低频接口的错误或长尾。候选稳定档还要保证每个低频接口有足够样本；本次10,000 QPS是单轮30秒快速验证，正式生产认证要求独立复测并使每接口累计至少10,000个样本。

旧代码在9,500 QPS档曾出现MailClaim和TaskClaim单接口错误率超线，证明必须逐接口判断。写链路修复后，最终10,000 QPS的31个接口错误率均低于0.1%，P95/P99也均通过硬SLO。

11,000 QPS为首个失败档：写入准入错误1003占0.1779%，已独立超过0.1%。该档TaskClaim还有586个1602，原因是7,500个本地账号每个只有1份可领奖励，第7,501次以后的TaskClaim耗尽了夹具；这部分与系统容量分开统计。

## 6. 每档必须记录的数据

### 压测端

- 目标QPS、发送数、成功数、失败数、错误码；
- 实际完成QPS、测量时长、总墙钟时长、排空时间；
- 整体Avg/P50/P90/P95/P99/Max；
- 31个接口各自的实际QPS、成功率、Avg/P90/P95/P99/Max。

### 服务端与依赖

- Gateway/Farm/Social/MySQL/Redis：CPU平均值与峰值、内存峰值、CPU throttling；
- Gateway：WebSocket连接数；
- Farm：驻留Actor、journal pending/lag、投影活跃数、Claim等待；
- MySQL：questions/s、threads running/connected、行锁等待；
- Redis：commands/s、连接数、内存使用；
- 测量窗口前后累计计数器差值，用于计算每个前端业务请求的Gateway/Farm/Social CPU毫秒、MySQL statements和Redis commands。

## 7. 基础容量计算所需参数

压测完成后统一使用：

```text
规划利用率 U = 70%
长连接规划利用率 Uc = 65%
业务峰值 B = 200,000 QPS
峰值在线 P = 4,000,000
会话到达 A = 3,333.33/s
```

各服务基础实例数：

```text
Gateway = max(连接约束, 业务CPU约束, Handshake约束)
Farm = max(混合QPS约束, CPU约束, 驻留Actor约束)
Social = max(Social调用QPS约束, Social CPU约束)
MySQL主分片 = ceil(峰值statement/s / 单分片规划statement/s)
Redis主分片 = max(命令吞吐约束, 数据内存约束)
```

本轮不增加N+1、多可用区或主从容灾余量；MySQL/Redis如写“副本”，只代表容量分片，不代表容灾副本。
