# 经典农场 · 期 3 规格：房间同步、好友与登录页

> 状态：已评审（头脑风暴 2026-07-26）  
> 工程规范：`2026-07-26-engineering-standards.md`  
> 上游：`docs/design/game-design-full.md`（11 / 13 章）、`architecture.md`、`protocol.md`（2 / 3.3 / 5.5）  
> 前置：期 2 种植权威 + online「发意图等 Rsp」已完成

---

## 1. 目标

交付 **好友关系 + 仅好友可拜访只读**，以及 **FarmDelta 房间同步**；客户端改为 **必须登录**，删除本地权威。

1. **全屏登录/注册页** → Handshake → EnterFarm(自己) → 进入农场场景  
2. **删除本地权威**：无 localStorage 种收、无未登录可玩路径  
3. **好友**：分享链接（HMAC、7 天、可多人复用）+ 按 **uid** 添加；好友列表；双向解除  
4. **EnterFarm**：仅 `SELF` / `FRIEND`；访客只读；非好友 `1401`  
5. **房间**：Actor 订阅表；写成功后广播 `FarmDelta`（9000）+ `farm_seq`；缺口 `SyncFarm`（小环形缓冲，过大回全量快照）

**成功标准：**

- 两账号互加好友（链接或 uid）后，A 拜访 B 只读；B 在自家种植/照料，A 经 Delta 或 Sync 跟上  
- 非好友 EnterFarm → `ERR_NOT_FRIEND`（1401）；访客写操作被拒且本地不变  
- 未登录只能进入登录页，无法进入可玩农场  
- 服务端单测覆盖：好友幂等四种情形、订阅进出、seq 缺口 Sync；smoke 或双客户端脚本覆盖主路径  

---

## 2. 非目标

- 好友浇水 / 除草 / 除虫 / 偷菜 / 狗（期 4+）  
- 公开按**用户名**搜索陌生人加好友（后期；见 §8）  
- 跨进程房间迁移、完整 200 条缓冲压测、乐观预测回滚  
- FriendList「有无可偷作物」摘要（期 3 可省略或恒 false）  
- 加好友成功的完整系统邮件权威（可用 toast；邮件链路后期）  
- 压测与容量分档  

---

## 3. 交付节奏

| 子阶段 | 内容 | 验收 |
| --- | --- | --- |
| **3a** | `friendship` 表 + GenShareLink / AcceptInvite / AddFriendByUID / FriendList / RemoveFriend；HTTP `/i/...` 落地 | 单测 + smoke 加好友 |
| **3b** | 房间订阅 + FarmDelta + SyncFarm；EnterFarm 校验好友；写路径广播 | 双连接同农场 seq 连续 |
| **3c** | 全屏登录页、删本地权威、好友/拜访 UI、访客只读、Delta 应用 | 浏览器两账号演示 |

建议分支：`feat/phase3-room-friends`（自期 2 完成点拉出）。

---

## 4. 服务端设计

### 4.1 好友存储

```sql
CREATE TABLE IF NOT EXISTS friendship (
  uid_lo BIGINT UNSIGNED NOT NULL,
  uid_hi BIGINT UNSIGNED NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (uid_lo, uid_hi)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

约定 `uid_lo = min(a,b)`、`uid_hi = max(a,b)`。插入冲突 → 已是好友（幂等）。

内存：`player.friend_ids` 或独立加载；期 3 以表为准，Actor 可缓存好友集合供 EnterFarm 快路径校验。

### 4.2 好友命令（对齐 protocol）

| cmd | 名称 | 说明 |
| ---: | --- | --- |
| 400 | FriendList | uid、nickname；在线粗状态可选；可偷摘要省略 |
| 402 | GenShareLink | 返回相对或绝对 `/i/<payload>.<sig>` |
| 404 | AcceptInvite | body 含凭证；建立双向关系 |
| 406 | RemoveFriend | 双向解除 |
| （新增或 404 变体） | AddFriendByUID | `{ peer_uid }`；直接建关系 |

分享凭证（不落库）：

```text
payload = base64url({ inviter_uid, nonce, exp })
sig     = base64url(HMAC-SHA256(server_key, payload)[:16])
link    = /i/<payload>.<sig>
```

`exp` = 签发时刻 + 7 天。密钥可用 `FARM_TOKEN_SECRET` 或独立 `FARM_INVITE_SECRET`。

错误码照抄 protocol 4.5：`1402`–`1407`；已有 `1401`。

HTTP：`GET /i/<payload>.<sig>` — 未登录引导登录页并带上 invite 参数；已登录则触发 AcceptInvite。

### 4.3 EnterFarm 与权限

| 关系 | Enter | 写地块 / BuySell | 收 Delta |
| --- | --- | --- | --- |
| SELF | ✓ | ✓ | ✓（在房时） |
| FRIEND | ✓ 只读 | ✗ | ✓ |
| 其他 | ✗ 1401 | — | — |

写路径：非主人一律拒绝（`NotOwner` 或既有码）。访客 UI 不发写 cmd。

`EnterFarmRsp.relation`：`SELF` | `FRIEND`。

### 4.4 房间与 FarmDelta

- 房间 ID = 农场主 uid  
- Actor（或 gateway 协作）维护订阅：`map[connID]viewerUID`  
- Enter 成功加入；LeaveFarm / WS 断开移除  
- 任意改变农场可见状态的成功写：`farm_seq++`，构造 `FarmDelta`，推送给订阅者  

`FarmDelta`（cmd **9000**）字段最小集：

```text
owner_uid, farm_seq, plots[], meta?, actor_uid, action
```

`SyncFarm`（cmd **204**）：

- 请求：`owner_uid`, `from_seq`  
- 响应：环形缓冲内的 deltas **或** 全量 snapshot（二选一）  
- 缓冲容量期 3：**64**（小于协议建议的 200，可配置；超出直接全量）

断线重连：Handshake 已有 `resume_farm_uid` / `resume_farm_seq`；期 3 尽量接上：校验好友后补 Sync 或重 Enter。

### 4.5 与期 2 写路径的衔接

- 现有 `ApplyPlotAction` / Buy / Sell 成功后：除 Rsp patch 外，若该农场有订阅者则广播 Delta  
- EnterFarm 前继续 `AdvanceAll`  
- `farm_seq` 必须在**持久化可见变更**时递增（含误铲 1204 带副作用的扣血，若仍保留该语义）

---

## 5. 客户端设计

### 5.1 路由

| 路径 | 说明 |
| --- | --- |
| `/login`（或 `/`） | 全屏登录/注册 |
| `/farm` | 3D 农场；需已登录 |
| `/i/:invite` | 分享落地；登录后 AcceptInvite |

视觉：农场题材品牌向；遵守工程 frontend 规则（非默认紫渐变模板）。本期不做营销长落地页。

### 5.2 删除本地权威

- 移除 / 停用：`localStorage` 权威存档、未登录 `doTill` 等、NPC 假好友本地模拟  
- DevNetPanel：删除或 DEV-only 诊断，不作入口  
- 所有玩法写路径仅 online 意图  

### 5.3 镜像与房间

- 维护 `viewingOwnerUid`、`lastFarmSeq`、`relation`  
- 应用顺序：Enter 快照 → 自己的 Rsp patch → FarmDelta → SyncFarm 结果  
- Delta：`seq == last+1` 则 apply；否则 SyncFarm  
- `FRIEND`：隐藏写工具/商店写；显示「回自己的农场」  
- 好友面板：展示自己的 uid、复制分享链接、粘贴链接或输入 uid、列表进入拜访  

### 5.4 等 Rsp（沿用期 2）

- 写操作仍等 Rsp；`err≠0` 默认不改本地（期 2 已定的 1204+patch 例外可保留在自家）  
- 访客不发起写 cmd  

---

## 6. 验收与测试

1. 好友：四种链接情形 + uid 自加/重复/满员（可用较小上限测满员或单测 mock）  
2. Enter：好友可进、非好友 1401、访客写拒绝  
3. 双连接：主人动作 → 访客 Delta seq 连续；人为丢包后 Sync 恢复  
4. 未登录不可进 `/farm`  
5. 浏览器：两账号互加 → 拜访 → 实时看到种植变化  

---

## 7. 与相邻期的边界

| 能力 | 期 |
| --- | ---: |
| 登录 / 种植权威 / online 意图 | 1–2（已完成） |
| 登录页、删本地、好友、只读拜访、FarmDelta | **3（本文）** |
| 好友浇水 / 偷菜 / 狗 | 4 |
| 按用户名搜好友 | 后期（§8） |
| 压测 | 5 |

---

## 8. 技术债登记

| 项 | 触发条件 |
| --- | --- |
| 公开按用户名搜索加好友 | 需要更顺畅的获客/加好友 UX，且可接受隐私与防刷方案时 |
| FriendList「可偷」摘要 | 期 4 偷菜上线时 |
| Delta 环形缓冲扩至 200 | 长观看 / 弱网压测出现频繁全量降级时 |
| 加好友系统邮件 | 邮件权威完善时 |
| 独立 `FARM_INVITE_SECRET` 与密钥轮换 | 生产部署前 |

---

## 9. 修订记录

| 日期 | 说明 |
| --- | --- |
| 2026-07-26 | 初稿：方案 A；登录页；删本地；好友链接+uid；只读拜访；最小 FarmDelta+Sync；用户名搜索后置 |
