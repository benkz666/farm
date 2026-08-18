# defense-60-v1 混合压测、优化与基础容量报告

测试日期：2026-08-15  
测试模型：`defense-60-v1`，31个业务接口，峰值模型20万业务QPS  
报告口径：逐接口判定；整体时延只用于观察系统是否全局失稳

## 1. 结论

1. 重构后的首要性能问题已经修复。原7,000 QPS下TaskClaim/MailClaim P95分别为1,096.6/1,061.6 ms；优化后同档降到97.9/118.7 ms，且210,000个请求零失败。
2. 当前1核Farm固定拓扑的30秒混合稳定档为 **10,000 QPS**。最终轮发送300,000个请求，成功299,988个，错误率0.004%；31个接口各自错误率均低于0.1%、P95均低于300 ms、P99均低于500 ms。
3. 按用户要求以1,000 QPS步长上探，11,000 QPS是首个失败档。其中587个1003为真实写入准入拒绝，单独占0.1779%，已超过0.1%；另586个1602是7,500份可领TaskClaim夹具被耗尽，不计入性能瓶颈。因此快速稳定上限确认为10,000 QPS。
4. 最终10,000 QPS的整体Avg/P95/P99为35.5/126.8/167.1 ms；最慢接口P95为MailRead的159.5 ms，最慢接口P99为PetActivate的222.6 ms。11,000 QPS的时延硬SLO仍通过，但错误率先失败，所以上限由写吞吐而不是时延决定。
5. 按3000万DAU、400万PCU、20万业务QPS，且暂不考虑容灾，保守取三轮单位请求成本的较大值后，基础部署建议为：Gateway 411、Farm 358、Social 10、MySQL主容量分片11、Redis主容量分片26。Farm建议1C2Gi，其余规格见第8节。

10,000 QPS是本次30秒峰值容量结论，不冒充生产长稳认证。正式上线前还要补10分钟和30分钟长稳，确认journal积压斜率不会持续增长。

## 2. 行为模型

```text
3000万DAU × 2次会话 / (10小时 × 3600) × 峰值系数2 = 3333.33次会话/s
3333.33次会话/s × 20分钟 × 60秒 = 400万PCU
3333.33次会话/s × 60业务请求/会话 = 20万业务QPS
```

一次20分钟会话按平均每20秒一次业务交互，共60个请求。分为进入同步6、本地农场19、好友与跨农场16、任务7、邮件5、商店/宠物/图鉴7。完整权重见 [`user-behavior.defense-60-v1.json`](../../model/user-behavior.defense-60-v1.json)。Ping/Pong、Handshake和服务器主动推送不计入这20万业务QPS。

## 3. 测试环境与流程

| 组件 | 测试副本 | 单实例资源 | 被测镜像摘要 |
|---|---:|---:|---|
| Gateway | 3 | 1C1Gi | `sha256:1d475482...e2d5f69` |
| Farm | 1 | 1C1Gi，`GOMAXPROCS=1` | `sha256:88f9cdcbc96b...2f707a` |
| Social | 1 | 1C1Gi | `sha256:4e0d218...d7e9f68` |
| MySQL 8.4 | 1 | 2C4Gi | `sha256:bced325...ae5cb` |
| Redis 7 | 1 | 1C1Gi | `sha256:2a51817...6f4b33` |

Farm固定`FARM_ACTOR_MAX_RESIDENT=20000`、8个committer shard、8个journal projector；固定15,000条WebSocket轮询直连3个Gateway。每档均等待旧journal排空、重置15,000个账号、重启Farm清空热Actor、建连并预热，然后开环发压30秒。服务变慢时压测端不自动降速。

## 4. 判定规则：必须逐接口看

| 层级 | 指标 | 通过线 |
|---|---|---:|
| 全局健康 | 整体错误率 | ≤0.1% |
| 全局健康 | 成功完成量/目标量 | ≥99.5% |
| 接口 | 每个接口错误率 | ≤0.1% |
| 接口 | 时延希望线（观察项） | Avg≈50 ms、P95≈100 ms、P99≈200 ms |
| 接口 | 每个接口P95硬SLO | <300 ms |
| 接口 | 每个接口P99硬SLO | <500 ms |
| 排空 | 客户端停止发压后的排空 | ≤1秒 |
| 后台 | journal pending/lag | 最终回到0 |

整体Avg/P90/P95/P99仍然记录，但只用于画容量曲线和判断全局失稳，不允许用一个整体P99掩盖低频接口的错误和长尾。低频接口样本不足时应合并同代码、同环境的独立轮次；生产认证要求每个接口至少10,000个样本。

## 5. 优化前后

### 5.1 Claim长尾根因和修复

旧实现的TaskClaim/MailClaim为了保证领取结果持久化，会等待UID的通用投影屏障，并与其他操作争用投影和流工作槽。Claim本身占住工作槽后，后续快请求也发生队头阻塞，所以Fertilize等无关接口同时出现长尾。

修复改为：在Actor串行域内校验领取；用带fence的原子事务直接持久化绝对经济值和`farm_seq`；只投影领取真正依赖的任务记录；成功后直接更新Actor并使快照缓存失效，不再等待整条UID投影追平。

| 7,000 QPS | 优化前P95 | 优化后P95 | 变化 |
|---|---:|---:|---:|
| TaskClaim | 1,096.6 ms | 97.9 ms | -91.1% |
| MailClaim | 1,061.6 ms | 118.7 ms | -88.8% |
| Fertilize | 585.1 ms | 84.7 ms | -85.5% |
| 整体P99 | 709.5 ms | 116.2 ms | -83.6% |

### 5.2 10,000 QPS全局热路径优化

CPU profile显示Claim已经不再是热点，主要成本转移到Delta推送、Presence查询和许多过小的写日志提交。本轮继续做了四项代码优化：

- Delta异步发布增加1 ms微批窗口，把突发Presence查询合成Redis pipeline；
- 绝大多数只投递到一个Gateway的Delta走单Gateway快速路径，去掉每条Delta的临时channel和goroutine；
- Presence读取只用`ZRANGEBYSCORE`返回存活成员，不再每次读都额外执行一次过期成员删除；
- 1核Farm的前台committer窗口从250 μs提高到1 ms，shard从16降到8，增加单次append装箱率。

| 10,000 QPS版本 | 错误率 | Avg | P90 | P99 |
|---|---:|---:|---:|---:|
| Claim优化后、推送优化前 | 0.458% | 41.8 ms | 117.5 ms | 177.5 ms |
| 推送/Presence优化后 | 0.309% | 34.6 ms | 106.0 ms | 164.2 ms |
| committer 8 shard后 | 0.260% | 32.9 ms | 103.0 ms | 167.2 ms |

最后一步将写提交峰值从约2.0请求/批提高到2.64请求/批，减少了小Lua append调用；但1核Farm在10,000 QPS仍触及全局写入边界，因此继续分析完整写链路。

### 5.3 Actor → Journal → Projection → MySQL专项优化

逐接口旧结果中大量写接口同时返回1003，不代表Buy、Water、Plant等业务逻辑同时变慢。服务端证据为Gateway限流和Farm流拒绝均为0，错误全部来自Farm动态写准入；其触发条件是后台journal总积压，而不是某个接口的处理耗时。

专项修复如下：

- 准入只保护真正产生Farm journal的命令；TaskClaim、MailClaim和MailDelete为带fence的MySQL直写，不再被无关journal积压误伤；
- 修复准入等待者单槽通知丢唤醒，改为generation广播，并在订阅后复查可用容量；等待预算从2 ms调整为5 ms；
- projector不再用“某一个分片一次读满1024条”判断积压，改用32分片的实际`pending + lag`总量，并在取得执行槽后非阻塞补满批次；
- outbox ACK从逐条MySQL `UPDATE`改为单条批量更新；在一致性barrier之间把ACK后移成一组，使被ACK穿插的Farm Mutation重新合并，同时保留Farm/Task/Codex原顺序和barrier语义；
- 删除投影首条Mutation的无条件Protobuf clone，仅在同UID确实需要合并时复制；
- A/B验证过6个前台projector和按PlayerMask拆SQL：前者增加1核CPU抢占，后者把MySQL语句数从约2,630/s升到3,751/s，均已撤回。最终保留4个前台projector和单条批量Player UPDATE。

| 10,000 QPS版本 | 错误率 | Avg | P90 | P95 | P99 | 排空 |
|---|---:|---:|---:|---:|---:|---:|
| 专项优化前 | 0.2600% | 32.9 ms | 103.0 ms | 123.0 ms | 167.2 ms | 235 ms |
| 准入与积压判断修复 | 0.0317% | 36.4 ms | 109.1 ms | 129.5 ms | 174.4 ms | 178 ms |
| ACK批量落库 | 0.0103% | 35.0 ms | 106.0 ms | 123.5 ms | 160.5 ms | 36 ms |
| 最终平衡版 | **0.0040%** | 35.5 ms | 107.2 ms | 126.8 ms | 167.1 ms | 233 ms |

最终版journal峰值lag从157,963降到114,597（-27.5%），投影记录速率由831/s提高到1,465/s（Prometheus窗口平均口径）。原始结果为[`final-chain-balanced-10000-r4.json`](final-chain-balanced-10000-r4.json)，资源窗口为[`final-chain-balanced-10000-r4-window.json`](final-chain-balanced-10000-r4-window.json)。

最终10,000 QPS下所有接口错误率均小于0.1%，并通过300/500 ms硬SLO。MailRead、PetFeed和部分Mail接口只是略高于期望线，不应判为失败；PetActivate只有174个样本，P99 222.6 ms暂列长稳复测项。

## 6. 最终10,000 QPS逐接口结果

| 接口 | 实际QPS | 请求数 | 错误率 | Avg | P95 | P99 |
|---|---:|---:|---:|---:|---:|---:|
| buy | 263.0 | 7,890 | 0.0127% | 44.4 ms | 130.8 ms | 173.7 ms |
| clear | 351.6 | 10,548 | 0.0095% | 44.4 ms | 131.2 ms | 164.2 ms |
| codex-list | 128.8 | 3,863 | 0 | 30.7 ms | 103.8 ms | 140.5 ms |
| enter-friend | 495.0 | 14,850 | 0 | 34.9 ms | 114.1 ms | 141.9 ms |
| enter-self | 398.7 | 11,961 | 0 | 30.1 ms | 104.3 ms | 130.7 ms |
| fertilize | 194.1 | 5,822 | 0 | 44.4 ms | 136.1 ms | 182.0 ms |
| friend-list | 609.8 | 18,294 | 0 | 3.0 ms | 9.4 ms | 31.0 ms |
| gen-share | 38.0 | 1,140 | 0 | 1.7 ms | 2.3 ms | 16.4 ms |
| harvest | 472.6 | 14,179 | 0.0071% | 44.4 ms | 134.8 ms | 180.7 ms |
| list-friend-requests | 720.6 | 21,618 | 0 | 2.1 ms | 2.9 ms | 14.8 ms |
| mail-claim | 40.6 | 1,217 | 0 | 45.8 ms | 153.1 ms | 214.0 ms |
| mail-delete | 9.9 | 297 | 0 | 46.8 ms | 150.1 ms | 188.3 ms |
| mail-list | 666.7 | 20,000 | 0 | 45.4 ms | 151.5 ms | 197.8 ms |
| mail-read | 117.0 | 3,509 | 0 | 52.7 ms | 159.5 ms | 196.3 ms |
| pest-cross | 162.7 | 4,880 | 0 | 44.8 ms | 138.4 ms | 168.4 ms |
| pest-local | 202.3 | 6,068 | 0 | 43.4 ms | 130.8 ms | 164.5 ms |
| pet-activate | 5.8 | 174 | 0 | 47.4 ms | 135.4 ms | 222.6 ms |
| pet-feed | 54.1 | 1,622 | 0 | 52.2 ms | 147.5 ms | 192.4 ms |
| pet-status | 586.2 | 17,585 | 0 | 29.7 ms | 104.3 ms | 131.8 ms |
| plant | 424.8 | 12,746 | 0.0157% | 43.7 ms | 129.8 ms | 165.3 ms |
| search-user | 86.1 | 2,583 | 0 | 2.1 ms | 2.9 ms | 16.5 ms |
| sell | 130.0 | 3,901 | 0.0256% | 44.0 ms | 135.3 ms | 190.2 ms |
| steal | 438.2 | 13,145 | 0 | 45.4 ms | 137.4 ms | 169.7 ms |
| sync | 97.7 | 2,932 | 0 | 33.0 ms | 115.6 ms | 159.1 ms |
| task-claim | 244.2 | 7,326 | 0 | 36.6 ms | 121.1 ms | 169.8 ms |
| task-list | 920.0 | 27,600 | 0 | 37.2 ms | 130.4 ms | 176.9 ms |
| till | 287.6 | 8,629 | 0.0116% | 43.9 ms | 129.5 ms | 166.9 ms |
| water-cross | 380.6 | 11,419 | 0 | 44.1 ms | 132.0 ms | 160.4 ms |
| water-local | 989.4 | 29,686 | 0.0135% | 44.4 ms | 133.6 ms | 166.7 ms |
| weed-cross | 228.1 | 6,843 | 0 | 45.1 ms | 131.0 ms | 161.3 ms |
| weed-local | 255.7 | 7,673 | 0.0130% | 44.6 ms | 132.4 ms | 171.5 ms |

所有31个接口都通过错误率和时延硬SLO。MailRead的Avg 52.7 ms和PetActivate的P99 222.6 ms是最高值，仅略高于期望线；其中PetActivate只有174个样本，应在长稳中增加样本后再判断。

## 7. 10,000通过、11,000失败与当前瓶颈

| 指标 | 10,000 QPS | 11,000 QPS | 判断 |
|---|---:|---:|---|
| 成功/发送 | 299,988/300,000 | 328,827/330,000 | 11,000包含夹具耗尽错误 |
| 真实系统错误 | 12个1003，0.0040% | 587个1003，0.1779% | 11,000超过0.1% |
| 夹具错误 | 0 | 586个1602 | TaskClaim发送8,086次，但只有7,500个本地账号可领 |
| 整体Avg/P95/P99 | 35.5/126.8/167.1 ms | 60.1/180.3/238.7 ms | 两档都通过时延硬SLO |
| 最慢接口P95/P99 | 159.5/222.6 ms | 211.7/363.3 ms | 11,000仍低于300/500 ms |
| Farm 30秒窗口平均CPU | 97.5% / 1C | 98.2% / 1C | 10,000已接近1核上限，11,000无法继续线性增长 |
| Farm CPU节流峰值 | 11.9% | 21.8% | cgroup CPU争用显著增加 |
| journal lag峰值 | 114,597 | 154,739 | 写入与投影差距扩大 |
| 写准入拒绝峰值 | 0.64/s | 33.48/s | 1003的直接来源 |

11,000档中Plant、Weed、Pest、Buy、Harvest等无关写接口同时出现1003，再次证明瓶颈不是单接口业务逻辑，而是1核Farm下的Actor → Journal → Projection公共写链路。停压后pending/lag最终能回到0，说明数据没有卡死；但测量窗口内生产速度已超过可稳定消化速度，因此11,000不能用作容量点。

用测量窗口CPU累计时间除以30秒，得到不受Prometheus滑动窗口对齐影响的平均CPU：

| 服务 | 资源上限 | 10,000 QPS | 11,000 QPS |
|---|---:|---:|---:|
| Gateway（3实例合计） | 3C | 1.253C，利用率41.8% | 1.322C，利用率44.1% |
| Farm | 1C | 0.975C，利用率97.5% | 0.982C，利用率98.2% |
| Social | 1C | 0.294C，利用率29.4% | 0.310C，利用率31.0% |
| MySQL | 2C | 0.701C，利用率35.0% | 0.527C，利用率26.4% |
| Redis | 1C | 0.533C，利用率53.3% | 0.455C，利用率45.5% |

11,000档MySQL和Redis CPU不升反降，是因为Farm准入在写日志之前拒绝了1003，TaskClaim夹具耗尽的1602也没有产生后续持久化工作。这说明存储层未饱和，当前上限由Farm 1C与公共写链路决定。

### 7.1 1:1:1:1:1拓扑10,000 QPS对照

保持同一镜像、1核Farm、15,000条WebSocket、同一夹具与行为模型，仅将Gateway从3实例缩减为1实例。正式测量前已等待journal归零、重置15,000账号并重启Farm清除热Actor。

| 指标 | 3 Gateway基线 | 1 Gateway对照 |
|---|---:|---:|
| 成功/发送 | 299,988/300,000 | 299,479/300,000 |
| 整体错误率 | 0.0040% | 0.1737% |
| Avg/P95/P99 | 35.5/126.8/167.1 ms | 310.2/768.1/1,060.7 ms |
| Gateway 30秒平均CPU | 1.253C/3C，41.8% | 0.795C/1C，79.5% |
| Farm 30秒平均CPU | 0.975C/1C，97.5% | 0.936C/1C，93.6% |
| Farm流队列峰值 | 543 | 2,207 |
| Farm流内并发峰值 | 203 | 60 |
| Farm流调度拒绝 | 0 | 平均18.65/s，峰值21.67/s |
| Farm动态写准入拒绝峰值 | 0.64/s | 0 |
| journal lag峰值 | 114,597 | 112,561 |

1 Gateway产生443个1003，单独占0.1477%，已经超过0.1%；另有26个1002和52个1202为业务/夹具错误，剔除后也不改变失败结论。

根因是Gateway到Farm的批处理gRPC调度器按流创建，每条流的普通lane并发上限为64。3 Gateway提供三条独立流，实测流内并发可达203；1 Gateway只有一条流，并发停在60左右，队列和长尾迅速放大。因此当前代码下`1:1:1:1:1`不能承载10,000 QPS；若坚持单Gateway，需把Gateway→Farm改为多流或全局调度，不能只调大队列上限。测试后Gateway已恢复为3副本。

## 8. 基础容量与部署建议（不含容灾）

两轮9,000 QPS与最终10,000 QPS精确测量窗口扣除空闲基线后，对每项成本取三轮较大值：Gateway/Farm/Social/MySQL/Redis CPU分别为0.13830/0.10541/0.03156/0.06570/0.05903 ms/业务请求；MySQL放大0.33374 statement/请求，Redis放大4.21192 command/请求。

| 服务 | 吞吐/CPU约束 | 连接/状态约束 | 基础量 | 单实例建议 |
|---|---:|---:|---:|---:|
| Gateway | CPU 40、Handshake 1 | 连接411 | **411** | 1C1Gi |
| Farm | 混合QPS 29、CPU 31 | Actor 358 | **358** | 1C2Gi |
| Social | 调用QPS 5 | CPU 10 | **10** | 1C1Gi |
| MySQL | statement 6 | 混合读写CPU 11 | **11个主容量分片** | 2C4Gi |
| Redis | command吞吐5 | 非pipeline真实CPU 26 | **26个主容量分片** | 1C4Gi起步 |

Social比Gateway/Farm少是正常的：它不维护WebSocket，也不驻留Actor；模型中63,395/s的社交与跨农场鉴权很多走缓存，实测CPU仅0.03156 ms/业务请求。Social最终由CPU算出10个，而Gateway由400万连接算出411个，Farm由约500万Actor算出358个。

MySQL和Redis的数字是主分片容量下限，不含任何从库、哨兵、N+1或多可用区。真正部署前必须实现UID分片路由并做双分片线性度测试。Redis内存无法从15,000账号夹具外推到3000万DAU；4 GiB只是建议起步规格，最终还要按key基数、value大小、TTL和碎片率单独核算。

机器可读结果见 [`capacity-result.json`](capacity-result.json)，计算脚本见 [`defense_capacity.py`](../../api/defense_capacity.py)。

## 9. 复现与证据文件

- 最终10,000 QPS稳定轮：[`final-chain-balanced-10000-r4.json`](final-chain-balanced-10000-r4.json)
- 首个失败11,000 QPS档：[`upper-bound-11000-r1.json`](upper-bound-11000-r1.json)
- 1:1:1:1:1拓扑10,000 QPS对照：[`topology-1to1-10000-r1.json`](topology-1to1-10000-r1.json)
- 优化前对照：[`final-9500-r1.json`](final-9500-r1.json)、[`final-8shard-10000-r1.json`](final-8shard-10000-r1.json)
- 资源时间窗：`final-*-window.json`
- 单请求资源成本：`final-*-demand-v3.json`
- CPU profile：`optimized-10000-profile.cpu.pprof`

容量计算命令：

```bash
python3 bench/api/defense_capacity.py \
  --model bench/model/user-behavior.defense-60-v1.json \
  --demand bench/results/defense-60-v1-20260815/final-9000-r1-demand-v3.json \
  --demand bench/results/defense-60-v1-20260815/final-9000-r2-demand-v3.json \
  --demand bench/results/defense-60-v1-20260815/final-chain-balanced-10000-r4-demand-v3.json \
  --output bench/results/defense-60-v1-20260815/capacity-result.json
```
