# 实时同步与跨农场操作优化设计

## 1. 状态

- 日期：2026-08-02
- 状态：设计已在对话中确认，等待文档审阅
- 范围：访客在好友农场执行浇水、除草、除虫、偷菜，以及相关 FarmDelta、弱网恢复和客户端反馈
- 性能目标：
  - 点击反馈小于 50ms
  - 同地域端到端 P95 小于 150ms
  - 同地域端到端 P99 小于 500ms
  - 消除当前低流量环境中约 2 秒的固定延迟台阶

## 2. 背景与现状问题

当前访客操作采用三段式流程：

1. Gateway 调用访客 Farm 预占资源并同步完整聚合落盘；
2. Gateway 通过 Kafka 发送 CrossAction；
3. 主人 Farm 裁决、同步完整聚合落盘并同步推送 FarmDelta；
4. 主人 Farm 通过 Kafka 发送 CrossResult；
5. Gateway 再调用访客 Farm 结算并同步完整聚合落盘；
6. Gateway 返回 WebSocket 响应，客户端此时才播放动画。

这条链路存在以下结构性问题：

- `kafka.Writer` 未设置 `BatchTimeout`，当前 kafka-go 默认最多等待 1 秒刷新未满批次；CrossAction 与 CrossResult 串行经过两次 Kafka，低流量下形成约 2 秒延迟。
- 客户端把动作动画绑定到最终响应，后端全部 RTT 直接表现为输入延迟。
- 预占、主人裁决、访客结算都调用完整 `SaveFarm`，每次事务重写 player、全部地块、物品和图鉴。
- 主人 Farm 在发 CrossResult 前同步执行 FarmDelta fan-out；Gateway 慢或推送重试会阻塞原始操作者响应。
- 每个 Gateway 使用独立 Kafka consumer group，所有 Gateway 都会收到同一 CrossResult；没有 pending 的 Gateway 也会先结算，导致真正持有 pending 的 Gateway 随后得到 Timeout，出现“业务已成功但客户端显示失败”的竞争。
- WebSocket 指标在异步请求返回空 Envelope 后即结束计时，没有观测完整 Kafka 回环与结算耗时。
- 当前 FarmDelta 有 `farm_seq` 补洞能力，但操作响应与 FarmDelta 分属两套应用逻辑，不能统一处理乱序与重复。

## 3. 设计目标

### 3.1 业务目标

- 玩家点击后立即获得明确但不虚假的操作反馈。
- 服务端仍是金币、经验、果实、地块、偷菜上限和狗拦截的唯一权威。
- 同一个农场内的并发操作严格串行，偷菜 40% 上限和单访客单轮一次不可突破。
- 网络中断、进程重启和 Kafka 重投不得造成重复发奖、重复扣款或重复修改地块。
- 超时表示“结果未知、正在同步”，不能被解释为业务失败。
- FarmDelta 丢失不能阻塞动作完成，客户端可通过 `farm_seq + SyncFarm` 恢复。

### 3.2 工程目标

- 跨农场状态由所属 Farm 管理，Gateway 不再执行跨玩家资源结算。
- 业务变更与待发布消息使用 Transactional Outbox 原子提交。
- Kafka 按目标物理 Farm 定向消费，不再广播给所有 Farm/Gateway 后依赖本地过滤。
- 关键路径使用小粒度事务，不再保存完整聚合。
- 所有异步发布器具备有界并发、退避、积压指标和安全关闭。
- 跨 JSON 的 operation ID、UID、连接 ID、farm_seq、金币和其他 int64/uint64 均编码为十进制字符串；客户端只用 BigInt 处理可能超过安全整数的业务数字。

## 4. 非目标

- 不把整个农场系统改造成事件溯源。
- 不提供跨访客与主人两个聚合的瞬时线性一致或分布式 ACID 事务。
- 不保证网络分区期间立即获得最终结果。
- 不把客户端动画当成业务成功证明。
- 不为旧的 CrossPending blob、旧 Kafka 消息或旧客户端协议保留兼容路径。
- 不在本次改造中实现同一 farm ID 的热备、双活或无缝主备切换。

## 5. 一致性模型

系统采用以下分层：

- 单一玩家聚合内：Actor 串行 + MySQL 事务，强一致。
- 主人农场地块内：Owner Actor 串行，强一致。
- 访客与主人之间：持久 Saga，至少一次消息投递、业务效果恰好一次，最终一致。
- 客户端农场镜像：按 `farm_seq` 单调应用，最终一致。

这符合农场游戏的业务特点：跨玩家操作频率低，允许短暂 pending，但资源与地块结果不能重复。

## 6. 状态所有权

### 6.1 Visitor Farm

Visitor Farm 是以下状态的唯一写入者：

- 访客维护次数和奖励资格；
- 偷菜赔付冻结金币；
- 偷菜成功后的果实；
- 互助经验和金币；
- `cross_operation` 状态；
- 最终 PlayerDelta；
- 操作完成回推事件。

### 6.2 Owner Farm

Owner Farm 是以下状态的唯一写入者：

- 主人地块；
- 偷菜额度和访客去重记录；
- 狗拦截和主人赔付收益；
- `cross_owner_receipt`；
- 本次权威 FarmDelta；
- CrossResult Outbox。

### 6.3 Gateway

Gateway 只负责：

- WebSocket 鉴权、房间关系和限流；
- 路由到 Visitor Farm；
- 接收 Farm 完成回推并返回客户端；
- pending 丢失时允许客户端通过 operation query 恢复。

Gateway 不再调用 CrossSettle，也不消费广播式 CrossResult。

## 7. 端到端数据流

### 7.1 客户端发起

1. 客户端使用 `crypto.getRandomValues` 生成 uint64 `operation_id`，转成十进制字符串。
2. 客户端把当前地块标记为 pending，立即播放“尝试动画”。
3. 客户端发送原动作命令，payload 增加 `operation_id`。
4. Gateway 校验当前连接确实处于目标好友房间，不再同步调用 Social 做第一次重复好友查询。
5. Gateway 调用 Visitor Farm 的 `BeginCross`。

数据库唯一键按 `(visitor_uid, operation_id)` 作用。客户端重试同一个操作必须复用相同 `operation_id`。

`BeginCross` 的 WebSocket 契约是：

- 预占失败时，同步返回原命令的业务错误，客户端进入 REJECTED；
- 预占成功时，同步返回 `status=pending`、operation ID 和预占 PlayerDelta；
- 最终结果使用独立的 `CrossCompletion` push，不再让原请求返回空 Envelope，也不使用同一 client_seq 返回第二个响应；
- `CrossCompletion` 携带原命令和原 client_seq，仅用于客户端关联 pending UI。

### 7.2 Visitor Farm BeginCross

Visitor Actor 在聚合副本上执行预占，然后调用专用存储事务：

1. 更新 player 中受影响的访客字段；
2. 插入 `cross_operation(state=RESERVED)`；
3. 以 `action_attempt=0` 插入首次 CrossAction `outbox_event`；
4. 提交成功后替换 Actor 内存聚合；
5. 提交后非阻塞通知 Outbox dispatcher 立即处理新 ID；
6. 提交失败则丢弃副本，Actor 内存保持原状态。

BeginCross 强制执行每个访客最多 16 个 `RESERVED/RECONCILING` operation。Actor 串行化同一 visitor UID；专用事务查询持久状态后再插入，防止重启绕过上限。偷菜冻结金币通过 pending ACK 中的 PlayerDelta 立即反映到客户端 HUD，最终拒绝时由 Completion 发回退款 PlayerDelta。

已存在相同 operation 时：

- `RESERVED/RECONCILING`：返回 pending；
- `SETTLED/ROLLED_BACK`：直接返回已持久化最终结果；
- 请求参数与原 operation 不同：返回 BadRequest，禁止同 ID 换参数。

### 7.3 Action 定向投递

每个写 Outbox 的事务提交后，都把新 Outbox ID 非阻塞写入进程内 wakeup channel。Dispatcher 优先按 ID 立即派发；channel 满、通知丢失或进程崩溃时，由 25ms 周期 safety poll 补偿。正常路径不能等待轮询间隔。

Outbox dispatcher 根据路由表解析 Owner Farm：

- 目标是本进程：投递到本地异步队列，但仍经过相同 handler 和幂等逻辑；
- 目标是远端：发布到 `cross.action.<farm_id>`；
- 只有目标 Farm 订阅自己的 topic；
- Kafka key 使用 `owner_uid`，保证同一主人农场操作有序；
- Writer 设置 `BatchTimeout=2ms`、`RequiredAcks=RequireAll`，保留持久性同时消除 1 秒默认凑批。

本地快路径只能在 BeginCross 事务提交后执行，不能在一个 Actor 回调内同步调用另一个 Actor。

### 7.4 Owner Farm 裁决

Owner Farm 消费 CrossAction 后：

1. 严格解码消息；
2. 检查目标 Farm、operation ID、双方 UID、动作和 deadline；
3. 在 Actor 外查询 `cross_owner_receipt`，已存在时按当前 action attempt 重放 CrossResult；
4. receipt 不存在且 action 已过 deadline 时，进入 Owner Actor；
5. receipt 不存在且未过期时，在 Actor 外通过 Social 做唯一一次权威好友关系校验；暂时失败直接返回；
6. 进入 Owner Actor 后再次查询 receipt，吸收并发 Action 重投；
7. receipt 已出现时按当前 action attempt 重放 CrossResult；
8. deadline 已过时持久化 Timeout receipt，不修改主人状态；
9. Social 给出最终拒绝时持久化拒绝 receipt；否则在聚合副本上执行主人裁决；
10. 在同一专用事务中更新 player/farm_seq、受影响的单个地块、owner receipt 和 result outbox；
11. 提交成功后替换 Actor 内存聚合并把 Delta 放入内存 ring；
12. 提交后立即唤醒 Result Outbox dispatcher；
13. 异步提交房间 FarmDelta fan-out，不等待推送完成。

Owner receipt 必须保存原始错误码、OwnerOutcome（偷取数量、狗拦截和赔付等主人裁决输入）和完整 OwnerDelta（`farm_seq + PlotChange`）。CrossResult 及其 Outbox payload 也必须显式携带 OwnerOutcome 和 OwnerDelta，Visitor Farm 才能计算 VisitorReward 并构造 Completion。任何重复 CrossAction 都返回第一次裁决结果。

错误分类必须固定：

- 非法 JSON、缺少身份字段、operation ID 为 0：永久格式错误，进入 DLQ，不进入业务状态机；
- 已解析但业务参数非法、非好友、地块不可操作、达到偷菜限制：最终拒绝，Owner 必须原子写 receipt 和 Result Outbox；
- Social 超时/5xx、MySQL 暂时失败、上下文取消：暂时失败，不写 receipt、不发 Result、不提交 Kafka offset；
- 不能把暂时故障转换成 Internal Result，否则重投可能在 Visitor 已回滚后又修改 Owner。

同步 Social RPC 的预算为 P95 20ms、P99 80ms，超时上限 100ms。该预算不达标时不能宣称端到端 SLO 达标，应在后续独立设计中引入带撤销失效的好友授权缓存；本设计不以牺牲删除好友后的即时正确性换取延迟。

### 7.5 Result 定向 Visitor Farm

Result Outbox 根据 Visitor UID 路由到 Visitor Farm：

- 本进程走本地异步队列；
- 远端发布到 `cross.result.<farm_id>`；
- Kafka key 使用 `visitor_uid`，保证同一访客的结算顺序。

Gateway 不订阅该 topic。

### 7.6 Visitor Farm 结算

Visitor Farm 消费 CrossResult 后：

1. 查询 `(visitor_uid, operation_id)`；
2. 已 `SETTLED/ROLLED_BACK` 时返回保存的 completion，不重复修改；
3. `RESERVED/RECONCILING` 时在聚合副本上结算；
4. 在同一事务中更新访客 player/item、operation 最终状态和 completion outbox；
5. 提交成功后替换 Actor 内存聚合；
6. 提交后立即唤醒 Completion Outbox dispatcher；
7. 异步发布 PlayerDelta；
8. Completion Outbox 直接调用 operation 中保存的原 Gateway 内部接口。

Completion 包含：

- operation ID；
- 原命令；
- 最终错误码；
- VisitorReward；
- PlayerDelta；
- 主人侧权威 FarmDelta（如果主人状态发生变化）。

### 7.7 Gateway 完成回推与查询

Gateway 接收 Completion 后：

- 当前连接仍匹配 origin conn：发送独立 `CrossCompletion` push；
- 连接已断开或已重连：安全丢弃本次传输回推并返回 HTTP 204；
- 丢弃回推不影响已经完成的业务状态。

Completion Outbox 在 Gateway 暂时不可达时重试。Gateway 接收并完成当前可达连接的处理后返回 204，dispatcher 才标记已发布。

新增 `CrossOperationQuery`：

- 请求可带 operation ID 列表，查询指定操作；
- 列表为空时返回该 visitor 的全部未完成 operation，以及最近 10 分钟完成的 operation；
- 只能查询当前鉴权 UID 自己的 operation；
- 返回 `pending` 或持久化 completion；
- 客户端重连或 sessionStorage 丢失时调用空列表查询，不会留下不可解释的冻结金币。

## 8. 持久化模型

### 8.1 cross_operation

```sql
CREATE TABLE cross_operation (
    visitor_uid       BIGINT UNSIGNED NOT NULL,
    operation_id      BIGINT UNSIGNED NOT NULL,
    owner_uid         BIGINT UNSIGNED NOT NULL,
    kind              VARCHAR(32) NOT NULL,
    plot_index        TINYINT UNSIGNED NOT NULL,
    crop_id           SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    day_id            INT UNSIGNED NOT NULL DEFAULT 0,
    state             VARCHAR(16) NOT NULL,
    rewarded          BOOLEAN NOT NULL DEFAULT FALSE,
    frozen_coin       BIGINT NOT NULL DEFAULT 0,
    origin_gateway_id VARCHAR(64) NOT NULL,
    origin_conn_id    BIGINT UNSIGNED NOT NULL,
    origin_client_seq INT UNSIGNED NOT NULL,
    result_code       INT UNSIGNED NULL,
    result_payload    JSON NULL,
    action_deadline_at BIGINT NOT NULL,
    next_reconcile_at BIGINT NOT NULL,
    reconcile_attempts INT UNSIGNED NOT NULL DEFAULT 0,
    created_at        BIGINT NOT NULL,
    updated_at        BIGINT NOT NULL,
    PRIMARY KEY (visitor_uid, operation_id),
    KEY idx_cross_operation_reconcile (state, next_reconcile_at)
);
```

最终状态的 `result_payload` 保存可重放 completion，避免重试时重新计算奖励或随机结果。

时间语义：

- `action_deadline_at = BeginCross 服务端提交时间 + 10s`；
- 客户端 5s 进入 SYNCING 只影响 UI，不修改服务端状态；
- 服务端首次 `next_reconcile_at = 提交时间 + 1s`，之后按 1s、2s、4s、8s 上限退避重放；
- Owner 只在 receipt 不存在时检查 deadline；允许 500ms 时钟偏差；
- 最终 operation 保留 7 天，未完成 operation 不允许被 GC。

### 8.2 cross_owner_receipt

```sql
CREATE TABLE cross_owner_receipt (
    owner_uid      BIGINT UNSIGNED NOT NULL,
    visitor_uid    BIGINT UNSIGNED NOT NULL,
    operation_id   BIGINT UNSIGNED NOT NULL,
    result_code    INT UNSIGNED NOT NULL,
    result_payload JSON NOT NULL,
    owner_delta    JSON NULL,
    expires_at     BIGINT NOT NULL,
    created_at     BIGINT NOT NULL,
    PRIMARY KEY (owner_uid, visitor_uid, operation_id),
    KEY idx_cross_owner_receipt_expiry (expires_at)
);
```

`result_payload` 是可直接重放的完整 CrossResult，包含 operation ID、原始动作、OwnerOutcome 和 OwnerDelta。VisitorReward 由 Visitor Farm 在结算时计算并保存在 operation completion 中。receipt 保留 7 天，必须晚于 operation 的最大恢复窗口。

### 8.3 outbox_event

```sql
CREATE TABLE outbox_event (
    id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    producer_farm_id VARCHAR(64) NOT NULL,
    event_type       VARCHAR(32) NOT NULL,
    target_id        VARCHAR(64) NOT NULL,
    message_key      VARCHAR(64) NOT NULL,
    dedupe_key       VARCHAR(191) NOT NULL,
    payload          JSON NOT NULL,
    attempts         INT UNSIGNED NOT NULL DEFAULT 0,
    next_attempt_at  BIGINT NOT NULL,
    locked_until     BIGINT NOT NULL DEFAULT 0,
    published_at     BIGINT NULL,
    created_at       BIGINT NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_outbox_dedupe (producer_farm_id, dedupe_key),
    KEY idx_outbox_dispatch (
        producer_farm_id, published_at, next_attempt_at, locked_until, id
    )
);
```

Dispatcher 使用小批量 `FOR UPDATE SKIP LOCKED` 认领，认领事务不执行网络 IO。事务提交后通过 wakeup channel 立即派发，25ms safety poll 只负责恢复丢失通知。发送成功后单独标记 `published_at`。进程在“发送成功、标记失败”之间崩溃会造成重复投递，由 operation/receipt 幂等吸收。

dedupe key 固定为：

- `action:<visitor_uid>:<operation_id>:<attempt>`；
- `result:<owner_uid>:<visitor_uid>:<operation_id>:<action_attempt>`；
- `completion:<visitor_uid>:<operation_id>`。

receipt 重放必须为当前 action attempt 插入新的 Result Outbox；不能复用已 published 的旧行，也不能绕过 Outbox 直接发。不同 attempt 允许产生重复 Result，Visitor operation 幂等吸收，确保“首次 Result 已发布但未被结算”时仍能收敛。

已发布 Outbox 保留 24 小时，之后批量删除；未发布 Outbox 永不按年龄直接删除。

### 8.4 消息协议

CrossAction 至少包含：

- schema version；
- operation ID；
- action attempt；首次为 0，每次 Reconciler 重放加 1；
- visitor/owner UID；
- action kind、plot index、crop ID、day ID；
- action deadline；
- origin Farm ID。

CrossResult 至少包含：

- schema version；
- operation ID；
- visitor/owner UID；
- action kind；
- result code；
- OwnerOutcome；
- OwnerDelta（包含 `farm_seq` 和可选 PlotChange）。

永久格式错误写入 `cross.action.<farm_id>.dlq` 或 `cross.result.<farm_id>.dlq`。DLQ envelope 保存原 topic、partition、offset、错误分类、首次失败时间和经过脱敏的原 payload；DLQ topic 保留 14 天。

## 9. Operation 状态机

服务端状态：

```text
不存在
  └─ BeginCross 事务成功 → RESERVED

RESERVED
  ├─ Owner OK Result       → SETTLED
  ├─ Owner Reject Result   → ROLLED_BACK
  └─ 服务端 next_reconcile_at 到期 → RECONCILING

RECONCILING
  ├─ 重放 Action，Owner receipt 存在 → 重放原 Result → SETTLED/ROLLED_BACK
  └─ receipt 不存在且 deadline 已过 → Owner 写 Timeout receipt → ROLLED_BACK
```

禁止 Visitor Farm 仅凭本地时间直接回滚 RESERVED。独立的服务端 Reconciler 扫描 `RESERVED/RECONCILING AND next_reconcile_at <= now`，在一个事务中先递增 `reconcile_attempts`，再把新值作为 `action_attempt` 插入 CrossAction Outbox，随后立即唤醒 dispatcher。该过程不依赖客户端在线。客户端超时只改变本地 UI。

重放的收敛结果：

- Owner 已经提交：receipt 返回原结果；
- Owner 尚未提交且 deadline 已过：Owner 写 Timeout receipt 后返回；
- 因此不存在“主人已扣果实、访客却提前退款”的窗口。

## 10. 客户端交互状态机

```text
IDLE
  └─ 点击 → PENDING

PENDING
  ├─ 权威成功 → CONFIRMED → IDLE
  ├─ 权威拒绝 → REJECTED  → IDLE
  └─ 5 秒未完成 → SYNCING

SYNCING
  └─ OperationStatus + SyncFarm → CONFIRMED/REJECTED → IDLE
```

交互要求：

- 点击后立即播放工具动作、轨迹、基础音效，表达“操作已发出”；
- 不立即发放经验、金币、果实，也不直接提交权威地块状态；
- 只锁定当前地块，其他地块仍可操作；全局 `onlineBusy` 不再覆盖普通地块动作；
- 成功后播放奖励飘字和权威状态变化；
- 失败后播放轻量中断或狗拦截表现；
- 超时显示“网络延迟，正在同步”，不能显示“操作失败”；
- pending operation ID 保存到 sessionStorage，页面恢复后查询最终状态；
- pending ACK 的 PlayerDelta 立即更新冻结后的金币；
- sessionStorage 丢失时调用空列表 CrossOperationQuery 恢复。

偷菜数量、狗拦截结果和奖励不可预测。偷菜点击时只播放“尝试偷取”，最终结果由 completion 确认。

## 11. FarmDelta 统一合并

CrossCompletion 中的权威 FarmDelta 与房间推送的 FarmDelta 必须进入同一个 FarmMirror 方法：

```text
seq <= lastFarmSeq      → 已应用，幂等忽略
seq == lastFarmSeq + 1  → 应用并推进 lastFarmSeq
seq > lastFarmSeq + 1   → 缓存，调用 SyncFarm 补洞，随后按序应用
```

Completion 和 Push 携带相同 `farm_seq` 时：

- 先到者更新镜像；
- 后到者因 seq 已应用而被忽略；
- 不允许 Completion 绕过序列检查直接 applyPatch；
- 跨农场响应禁止再调用当前 `applyResponsePatch` 修改好友地块，所有好友地块变化只能进入 `FarmMirror.applyAuthoritativeDelta`。

PlayerDelta 独立按 operation 最终状态应用。相同 operation 的重复 completion 必须幂等忽略。

FarmDelta fan-out 是可恢复的 best effort：

- 不进入 CrossResult 关键路径；
- 使用有界 worker 并行推送到 Gateway；
- 推送失败只记录指标和日志；
- 客户端靠 SyncFarm 恢复。

## 12. Redis 农场缓存调整

专用小事务提交 MySQL 后若进程崩溃，旧的 `farm:{uid}` Redis 快照可能未及时更新。继续把该缓存作为 Actor 权威加载来源会产生旧状态回退。

本设计移除 Farm Actor 对 `farm:{uid}` Redis 聚合快照的权威读取：

- Actor 冷加载直接读取 MySQL；
- Actor 内存仍是在线热状态；
- Redis 继续承载 session、连接注册、房间订阅和可偷提示；
- SyncFarm 从 Actor/MySQL 权威状态构造快照；
- 不采用“MySQL 提交后再删 Redis”作为正确性保证，因为该方案仍有崩溃窗口。

同时修订 `docs/design/architecture.md` 中“Redis 是 Farm Actor backing store”的旧描述；MySQL 是持久权威，Actor 内存是在线热状态。

### 12.1 与 Actor 持久化契约的衔接

- BeginCross、OwnerApply、VisitorSettle 在 Actor mailbox 内串行执行“clone → 专用事务 → swap”；
- 这些路径不调用 `RequireFlush`，也不设置 runtime 的 full-save dirty 标记；
- 专用事务只更新明确字段或 upsert 单个 item，禁止把局部 Items map 传给 `replaceItemsTx`；
- 其他旧命令仍可使用完整 `SaveFarm`；同一 UID 的 mailbox 串行保证 full save 不会与专用事务并发；
- 专用事务提交失败时不得 swap；后续完整 SaveFarm 保存的是最后一次已提交的完整聚合；
- 测试必须覆盖专用事务之后执行 full save 不丢失其他 items/codex/plots。

## 13. Kafka 与本地异步传输

### 13.1 Kafka

- `BatchTimeout=2ms`
- `RequiredAcks=RequireAll`
- key：
  - CrossAction 使用 owner UID 十进制字符串；
  - CrossResult 使用 visitor UID 十进制字符串。
- topic：
  - `cross.action.<target_farm_id>`
  - `cross.result.<target_farm_id>`
- 每个物理 Farm ID 只消费自己的 topic；
- handler 成功后提交 offset；
- 暂时性存储错误不提交 offset；
- 永久格式错误写 DLQ 并提交原消息，避免毒消息无限阻塞分区。

生产环境关闭 `AllowAutoTopicCreation`，由部署脚本预建 topic、分区和保留策略。

一个物理 `farm_id` 同时只允许一个进程：

- 不同 Farm 实例必须使用不同 farm ID 和不重叠 UID 路由；
- 启动时若相同 farm ID 已注册则失败，不能形成 active-active；
- 部署替换必须先让旧实例停止接收、停止消费并释放注册，再启动同 ID 新实例；
- 同 farm ID 热备、无缝切换与数据库 fencing 不在本设计范围内；
- 这避免同一 UID Actor 分散到两个 Farm 进程。

### 13.2 本地快路径

目标 Farm 与当前 Farm 相同时：

- 事务提交后的 wakeup 使 Outbox dispatcher 立即投递本地有界队列；
- 队列调用与 Kafka 消费完全相同的 handler；
- handler 返回成功后标记 outbox published；
- 队列满时不阻塞 Actor，保留 outbox 由 25ms safety poll 重试；
- 禁止在当前 Actor 回调中直接同步调用目标 Actor。

## 14. 错误与恢复策略

### 14.1 消息格式错误

- 写入 DLQ，记录 event type、目标 Farm、消息 ID 和解码错误；
- 不记录原始认证令牌等秘密；
- 提交原消息 offset，防止分区永久卡死。

### 14.2 数据库暂时失败

- 当前业务事务整体回滚；
- Actor 副本不替换内存状态；
- Kafka handler 返回 error，不提交 offset；
- Outbox dispatcher 使用带 jitter 的指数退避。

### 14.3 业务拒绝

- Owner 对已解析的最终业务拒绝持久化 receipt 和 Result Outbox；
- 重复请求返回同一个错误；
- Visitor 收到后回滚预占并保存最终 operation。

Social/MySQL 超时等暂时故障不是业务拒绝，禁止落 Internal receipt 或发送 Result。

### 14.4 Gateway 不可达

- Completion Outbox 重试；
- 业务状态不回滚；
- 客户端重连后通过 OperationStatus 查询；
- Gateway pending 只负责传输，丢失不影响结算。

### 14.5 Delta 丢失

- 不重试到阻塞 operation；
- FarmMirror 发现 seq 缺口时 SyncFarm；
- ring 无法补齐时返回全量快照。

## 15. 并发与背压

- 同一 UID Actor 严格串行；
- 不同 UID Actor 并行；
- 每个 visitor 最多 16 个未完成 operation，超过返回 RateLimited；
- Kafka 对同 owner UID 保序；
- Outbox dispatcher 采用固定批大小和 worker 上限；
- 本地消息队列有容量上限；
- Gateway completion push 和 FarmDelta fan-out 有独立 worker 池；
- 队列满时保留持久 Outbox 或丢弃可恢复 Delta，不能无限创建 goroutine；
- 暴露 backlog 数量与 oldest age，超过阈值告警；
- Actor mailbox 持续大于阈值时告警，不通过并行写同一 Aggregate 来扩容。

关键路径延迟预算：

- Gateway 入站和 Farm RPC：P95 10ms；
- 三个小粒度 MySQL 事务合计：P95 45ms；
- 两次 Outbox commit-to-dispatch 合计：P95 10ms；
- 两次本地队列/Kafka 传输合计：P95 30ms；
- Social AreFriends：P95 20ms；
- Completion HTTP 与 WebSocket push：P95 15ms；
- 其余调度余量：20ms。

性能测试必须分别输出这些阶段分位数。若某一同步依赖超预算，先处理该依赖，不通过放宽总 SLO 掩盖问题。

## 16. 可观测性

所有阶段使用 operation ID 关联，新增：

- `cross_operation_duration_seconds`
- `cross_begin_duration_seconds`
- `cross_action_queue_seconds`
- `cross_owner_commit_duration_seconds`
- `cross_result_queue_seconds`
- `cross_settle_duration_seconds`
- `cross_completion_push_duration_seconds`
- `cross_operations_total{kind,result}`
- `cross_reconcile_total{reason}`
- `cross_outbox_backlog{event_type}`
- `cross_outbox_oldest_age_seconds{event_type}`
- `cross_outbox_retry_total{event_type}`
- `cross_outbox_wakeup_lag_seconds{event_type}`
- `cross_dlq_total{event_type,reason}`
- `cross_local_queue_depth{event_type}`
- `farm_delta_recovery_total{mode=ring|snapshot}`

客户端记录：

- click-to-attempt-feedback；
- click-to-authoritative-result；
- pending 超过 5 秒比例；
- SyncFarm 补洞与全量恢复次数。

旧 `farm_ws_request_duration_seconds` 继续观测同步请求；跨农场操作的完整耗时必须由 operation 指标覆盖，不能在返回空 Envelope 时提前结束。

## 17. 测试与验收

### 17.1 单元测试

- 每个 operation 状态转移；
- 相同 operation ID 同参数幂等、不同参数拒绝；
- operation ID 为 0 或缺少身份字段进入 DLQ；
- Owner receipt 重放；
- receipt 重放按新 action attempt 生成新 Result Outbox，不被旧 dedupe key 阻断；
- NotFriend/BadRequest 落拒绝 receipt，Social 超时不落 receipt；
- action 超时且无 receipt 时安全回滚；
- action 超时但 receipt 已存在时重放原结果；
- 重复 Result 不重复发奖或扣款；
- 最多 16 个未完成 operation，第 17 个返回 RateLimited；
- Actor 事务失败不替换内存状态；
- FarmDelta/Completion 任意顺序与重复；
- Completion OwnerDelta 与 push 乱序时禁止走 `applyResponsePatch`；
- seq 缺口触发 SyncFarm；
- 客户端点击立即触发尝试动画；
- BeginCross 同步错误进入 REJECTED，pending ACK 等待 Completion push；
- pending 只锁当前地块；
- pending ACK 的 PlayerDelta 立即反映冻结金币；
- 1411 狗拦截应用副作用但展示失败表现；
- 大整数 operation ID、farm_seq、UID 和金币在 JSON 中保持十进制字符串，覆盖 9007199254740992 与 9007199254740993，客户端禁止 Number/parseInt 合并二者。

### 17.2 MySQL 集成测试

- 预占变更、operation、action outbox 原子提交；
- 主人地块、receipt、result outbox 原子提交；
- 访客奖励、operation 完成、completion outbox 原子提交；
- dispatcher 认领并发不重复占有同一任务；
- 提交后 wakeup 可立即派发，通知丢失时 safety poll 能恢复；
- 发送成功但标记前崩溃造成重复投递时，业务效果仍恰好一次；
- Actor 重启直接从 MySQL 恢复最新状态；
- 专用事务后执行完整 SaveFarm 不丢失未修改的 item/codex/plot。

### 17.3 Kafka 集成测试

- 目标 Farm 定向消费；
- 同 owner UID 操作保序；
- handler 失败不提交 offset；
- 重复 action/result 幂等；
- 格式错误进入 DLQ，不阻塞后续消息；
- `BatchTimeout=2ms` 配置回归测试；
- 生产配置禁止自动创建 topic。

### 17.4 多实例测试

- 双 Gateway、双 Farm 下只有 Visitor Farm 结算；
- 重复 farm ID 启动失败，不形成双活；
- 原 Gateway 崩溃后 operation 仍完成；
- 客户端重连到另一 Gateway 后查询最终状态；
- sessionStorage 清空后空列表查询仍能找回未完成 operation；
- 进入好友房间后删除好友，Owner 写 NotFriend receipt 并回滚预占；
- Owner Farm 在提交后、发布 Result 前崩溃，重启后由 Outbox 续投；
- Visitor Farm 在结算后、回推前崩溃，重启后由 Completion Outbox 续投；
- FarmDelta 推送失败后 SyncFarm 恢复。

### 17.5 性能验收

- 低流量下不再出现约 1 秒或 2 秒台阶；
- 正常 commit-to-dispatch 不等待 25ms safety poll；
- 点击到尝试动画小于 50ms；
- 同地域成功操作 P95 小于 150ms；
- 同地域成功操作 P99 小于 500ms；
- 200 个访客并发偷同一农场时不突破 40% 上限；
- 高峰期 Outbox backlog 可回落，worker/goroutine 数量有界；
- 注入 100ms 网络延迟时操作进入同步中但不产生错误回滚。

## 18. 清理项

实现完成后删除：

- 聚合内旧 `CrossPending` blob 与旧惰性过期逻辑；
- 聚合内旧 `CrossReceipts` blob；
- Gateway `settleCrossVisitor`、`timeoutCrossAction`、`WithCrossEventBus` Result 订阅与广播式 CrossResult consumer；
- Farm RPC 的 `OperationCrossSettle`；
- 旧 `ExpireCrossPending` 调用点；
- 跨农场关键路径上的同步 FarmDelta fan-out；
- 客户端地块动作全局 `onlineBusy`；
- Farm Actor 对 Redis 聚合快照的权威读取；
- 旧的未定向 `cross.action` / `cross.result` topic 使用路径。
- `docs/design/architecture.md` 中 Redis 作为 Farm Actor backing store 的旧描述。

## 19. 已确认决策

- 采用按状态所有权重构，而不是局部修补或全面事件溯源。
- 允许新增数据库表和迁移，不保留旧实现兼容。
- 客户端使用“即时尝试动画 + 权威成功效果 + 超时自动同步”。
- 性能目标采用点击反馈小于 50ms、P95 小于 150ms、P99 小于 500ms。
- Transactional Outbox、稳定 operation ID、定向消费、回推恢复和有界背压是硬性要求。
