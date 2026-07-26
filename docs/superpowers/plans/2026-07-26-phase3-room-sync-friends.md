# Phase 3 Room Sync & Friends Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地好友（链接+uid）、仅好友可拜访只读、FarmDelta/SyncFarm 房间同步，以及全屏登录页并删除本地权威。

**Architecture:** Actor 串行写路径成功后向房间订阅者广播 `FarmDelta`；Gateway 维护连接订阅；`friendship` 表双向关系；客户端必须登录，状态仅由 snapshot/Rsp/Delta/Sync 驱动。

**Tech Stack:** 现有 Go farm-server、MySQL/Redis、Vue3 + Vite、JSON Envelope（`farm.v1.json`）

## Global Constraints

- 规格：`docs/superpowers/specs/2026-07-26-phase3-room-sync-friends.md`
- 错误码/命令号照抄 `docs/design/protocol.md`；禁止自造码（社交段 1402–1407；Delta=9000；SyncFarm=204）
- 提交：约定式前缀 + **中文**主题（见 `AGENTS.md`）
- 期 3 **不**做：浇水互助/偷菜/狗、用户名搜索、可偷摘要、完整邮件、压测
- 子阶段：3a → 3b → 3c
- 好友上限测试可用 `gameconf.FriendLimit`（默认 200，单测可注入更小）
- 分支：从当前期 2 HEAD 开 `feat/phase3-room-friends`
- 浏览器验证默认 ego-lite；登录页 UI 遵循 frontend 规则（非紫渐变模板）

---

## File Structure (target)

```text
server/migrations/003_friendship.sql
server/internal/pkgerr/codes.go              # 补 1402–1407
server/internal/social/invite.go             # HMAC 签发/校验
server/internal/social/friends.go            # 关系 API（可放 store）
server/internal/store/friends.go
server/internal/gateway/envelope.go          # cmd 204/400/402/404/406/408/9000
server/internal/gateway/ws_friends.go
server/internal/gateway/ws_room.go           # 订阅、Delta、SyncFarm、LeaveFarm
server/internal/gateway/http_invite.go       # GET /i/...
server/internal/actor/...                    # 可选：FarmActor 上 delta ring
server/cmd/smoke/main.go                     # 扩展双用户好友+拜访
client/src/views/LoginPage.vue               # 全屏登录注册
client/src/views/InviteLanding.vue           # /i/:token
client/src/router.js
client/src/net/client.js                     # 好友/Sync/Leave + onDelta
client/src/net/session.js                    # viewingOwnerUid、lastFarmSeq、relation
client/src/game/applyPatch.js                # 应用 FarmDelta
client/src/game/main.js                      # 删本地权威；访客只读；好友面板
client/src/components/FriendsPanel.vue       # 可选拆分
```

---

## 3a — 好友

### Task 1: 分支 + 社交错误码 + friendship 迁移

**Files:**
- Create: `server/migrations/003_friendship.sql`
- Modify: `server/internal/pkgerr/codes.go`、`Makefile` migrate 目标、`.env.example`（可选 `FARM_INVITE_SECRET`）
- Test: `server/internal/pkgerr/codes_test.go`（扩展）

**Interfaces:**
- Produces: `AlreadyFriend=1402` … `InviteExpired=1407`；DDL `friendship(uid_lo, uid_hi, created_at)`

- [ ] **Step 1:** 检出 `feat/phase3-room-friends`（基于当前期 2 HEAD）

- [ ] **Step 2:** 红测 — 断言 `pkgerr.AlreadyFriend == 1402` 等

```bash
cd server && go test ./internal/pkgerr/ -run Friend -v
```

Expected: FAIL（常量不存在）

- [ ] **Step 3:** 实现常量 + `003_friendship.sql`；`make migrate` 追加执行 003

- [ ] **Step 4:** Commit `feat: 添加好友错误码与 friendship 表`

---

### Task 2: 邀请凭证 HMAC

**Files:**
- Create: `server/internal/social/invite.go`, `invite_test.go`

**Interfaces:**
- Produces: `IssueInvite(inviterUID uint64, now int64, secret []byte) (token string, err error)`；`ParseInvite(token string, secret []byte, now int64) (inviterUID uint64, err pkgerr.Code)`
- Consumes: 密钥字节；`exp = now + 7*24*HourMs`（真实毫秒墙钟即可）

- [ ] **Step 1:** 红测 — 签发后解析得同一 uid；篡改 sig → `InviteInvalid`；过期 → `InviteExpired`

- [ ] **Step 2:** 实现 base64url JSON payload + HMAC-SHA256 截断 16 字节

- [ ] **Step 3:** 测试绿；Commit `feat: 实现好友分享链接 HMAC 凭证`

---

### Task 3: FriendStore + 建/删/列

**Files:**
- Create: `server/internal/store/friends.go`, `friends_test.go`（可用 integration tag 或 sqlmock；优先 integration）
- Modify: `store.go` 接口扩展

**Interfaces:**
- Produces:
```go
AreFriends(ctx, a, b uint64) (bool, error)
AddFriends(ctx, a, b uint64) error // 已存在返回 ErrAlreadyFriend 哨兵
RemoveFriends(ctx, a, b uint64) error
ListFriends(ctx, uid uint64) ([]FriendRow, error) // uid, nickname join player
CountFriends(ctx, uid uint64) (int, error)
```
- FriendLimit：`gameconf.FriendLimit = 200`

- [ ] **Step 1:** 红测 — Add A-B 成功；再 Add → AlreadyFriend；Add self → 调用方校验；满员（测时把 Limit 设 1）

- [ ] **Step 2:** 实现 `uid_lo/uid_hi` INSERT；List join `player.nickname`

- [ ] **Step 3:** Commit `feat: 实现好友关系存储`

---

### Task 4: Gateway 好友命令 + HTTP 落地

**Files:**
- Modify: `envelope.go`（CommandFriendList=400, GenShareLink=402, AcceptInvite=404, RemoveFriend=406, AddFriendByUID=408）
- Create: `ws_friends.go`, `http_invite.go`
- Modify: `http_auth.go` Handler 注册 `GET /i/`
- Test: `gateway` 表驱动（session stub + friend store stub）

**Interfaces:**
- AcceptInvite payload: `{ "token": "<payload>.<sig>" }`
- AddFriendByUID: `{ "peer_uid": N }`
- GenShareLink Rsp: `{ "path": "/i/..." }`
- FriendList Rsp: `{ "friends": [ { "uid", "nickname" } ] }`

- [ ] **Step 1:** 红测 — 非好友无关；Accept 自己 → 1403；重复 → 1402

- [ ] **Step 2:** 接线 Store + social.ParseInvite；密钥 `FARM_INVITE_SECRET` 或回落 `FARM_TOKEN_SECRET`

- [ ] **Step 3:** Commit `feat: 网关接入好友命令与邀请落地页`

---

### Task 5: smoke 加好友路径

**Files:**
- Modify: `server/cmd/smoke/main.go`

- [ ] **Step 1:** 注册用户 A、B；A GenShareLink；B AcceptInvite；A FriendList 含 B；B AddFriendByUID(A) → 1402

- [ ] **Step 2:** `make smoke` 绿（或 `FARM_SMOKE_MODE=friends` 子命令）；Commit `feat: 扩展 smoke 覆盖加好友`

---

## 3b — 房间与 Delta

### Task 6: Delta ring + 房间订阅

**Files:**
- Create: `server/internal/farm/delta.go`, `delta_test.go`
- Create: `server/internal/gateway/room.go`（或 actor 内 Room）
- Modify: `FarmActor` / Runtime 视现有结构二选一：**推荐 gateway RoomHub 按 ownerUID 订阅 conn**，写成功后 hub.Broadcast

**Interfaces:**
```go
type PlotChange struct { /* 与 PlotSnapshot 对齐的变更子集 */ }
type FarmDelta struct {
  OwnerUID uint64 `json:"owner_uid"`
  FarmSeq  uint64 `json:"farm_seq"`
  Plots    []PlotChange `json:"plots"`
  ActorUID uint64 `json:"actor_uid"`
  Action   uint32 `json:"action"`
}
type DeltaRing struct { /* cap 64; Append; Since(fromSeq) ([]FarmDelta, ok) */ }
```

- [ ] **Step 1:** 红测 — Append 1..65；Since(1) 在溢出后 ok=false

- [ ] **Step 2:** 实现 ring；RoomHub Subscribe/Unsubscribe/Broadcast

- [ ] **Step 3:** Commit `feat: 实现 FarmDelta 环形缓冲与房间订阅`

---

### Task 7: EnterFarm 好友校验 + LeaveFarm + SyncFarm + 写后广播

**Files:**
- Modify: `ws.go` EnterFarm；`ws_actions.go` 成功路径广播
- Create/Modify: SyncFarm handler；LeaveFarm=202
- Test: gateway 双 runtimeStub 或单 hub 测 seq

**Interfaces:**
- EnterFarm：owner≠self 时 `AreFriends`；失败 1401；成功 Subscribe + relation
- 写成功：`farm_seq` 已由聚合递增则用之；构造 Delta；Broadcast（含操作者连接可选）
- SyncFarm：`from_seq`；ring 命中回 deltas，否则回 snapshot
- 访客写：gateway 在 owner≠connection.uid 时直接 NotOwner（1202）

- [ ] **Step 1:** 红测 — 非好友 Enter→1401；好友 Enter relation=FRIEND；Till 后订阅者收到 cmd 9000 且 seq+1

- [ ] **Step 2:** 实现；注意 WS 推送为**服务端主动** Envelope（无 client_seq 或 client_seq=0）

- [ ] **Step 3:** Commit `feat: 接入 EnterFarm 好友校验与 FarmDelta 广播`

---

### Task 8: smoke / 集成双连接同步

**Files:**
- Modify: smoke 或 `server/cmd/smoke_room/main.go`

- [ ] **Step 1:** A、B 好友；B Enter A；A Till；B 收到 Delta 或 Sync 后状态一致

- [ ] **Step 2:** Commit `feat: 扩展 smoke 覆盖房间 Delta`

---

## 3c — 客户端

### Task 9: 路由 + 全屏登录页

**Files:**
- Create: `client/src/router.js`, `client/src/views/LoginPage.vue`, `InviteLanding.vue`
- Modify: `client/src/App.vue`、`main.js`（入口）
- 视觉：frontend-design 约束；品牌名「经典农场」

**Interfaces:**
- Login 成功 → session + connect + handshake + enterFarm(0) → `/farm`
- `/i/:token` → 登录后 `acceptInvite`

- [ ] **Step 1:** 未登录访问 `/farm` 重定向 `/login`

- [ ] **Step 2:** 实现登录/注册表单（用户名密码）；错误码 toast

- [ ] **Step 3:** Commit `feat: 添加全屏登录注册与邀请落地路由`

> 注：若 UI 工作量很大，实现子代理按 `AGENTS.md` 先询问用户模型选择。

---

### Task 10: 删除本地权威

**Files:**
- Modify: `client/src/game/main.js`, `state.js`（停用 save/load 权威）、移除 NPC 驱动写
- Remove/Disable: DevNetPanel 入口（或 import.meta.env.DEV 诊断 only）

- [ ] **Step 1:** 确认无 localStorage 写档驱动种收；未登录无 3D 可玩

- [ ] **Step 2:** Commit `refactor: 移除客户端本地种植权威`

---

### Task 11: 好友面板 + 拜访只读 + Delta 应用

**Files:**
- Modify: `client.js`（Friend* API、`onPush`/`onDelta`）、`session.js`、`applyPatch.js`、`main.js`/UI
- Create: `FriendsPanel.vue`（可选）

**Interfaces:**
- `lastFarmSeq`、`viewingOwnerUid`、`relation`
- 收 9000：连续则 apply；否则 `syncFarm`
- FRIEND：隐藏写工具；「回自己农场」→ EnterFarm(0)

- [ ] **Step 1:** 单元/node 测：乱序 seq 触发 sync 标志

- [ ] **Step 2:** 实现面板（展示 uid、复制链接、加 uid、列表拜访）

- [ ] **Step 3:** Commit `feat: 客户端接入好友拜访与 FarmDelta 镜像`

---

### Task 12: 浏览器双账号验收

**Files:** 无强制代码；更新 `README.md` 期 3 演示步骤

- [ ] **Step 1:** ego-lite 或双浏览器：注册两号 → 加好友 → 拜访 → 主人种植 → 访客看到变化

- [ ] **Step 2:** 记录结果到报告；Commit `docs: 更新 README 期 3 双客户端演示步骤`

---

## Self-Review (author)

1. **Spec coverage:** 3a 好友/邀请/落地；3b 房间/Delta/Sync/Enter 校验；3c 登录页/删本地/镜像/拜访 — 均有 Task。用户名搜索在非目标。  
2. **Placeholders:** 无 TBD 步骤。  
3. **Types:** FarmDelta/cmd 与规格一致；AddFriendByUID=408（规格允许新增变体）。  

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-26-phase3-room-sync-friends.md`.

**Two execution options:**

**1. Subagent-Driven (recommended)** — 每 Task 派实现子代理 + 审查，连续跑完 3a→3c  

**2. Inline Execution** — 本会话按 executing-plans 批次推进  

Which approach?
