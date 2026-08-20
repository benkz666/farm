# 经典农场 · 期 2 规格：种植权威与客户端意图

> 状态：已评审（头脑风暴 2026-07-26）  
> 工程规范：`2026-07-26-engineering-standards.md`  
> 上游：`docs/design/game-design-full.md`、`architecture.md`、`protocol.md`  
> 前置：期 1 联通切片已完成（登录 → Handshake → EnterFarm 快照）

---

## 1. 目标

交付 **服务端权威种植闭环**，并把客户端（登录后）改为 **发意图、等 Rsp 再更新本地镜像**：

1. 惰性 `advance` + validate/commit  
2. 主路径动作：Till / Clear / Plant / Water / RemoveWeed / RemovePest / Harvest  
3. 商店：`Buy` 种子；建议同期落地最小 `Sell`（果实→金币），保证金币可回流  
4. 客户端 online 模式：以服务端快照为底，工具栏/商店走 WS；**等 Rsp 再 patch**  
5. **期末（2c）**：Fertilize、多季跨季  

**成功标准：**

- 服务端单测：状态机主路径 + `advance` 跨成熟/枯萎  
- smoke：注册 → 买种子 → 锄地 → 播种 → 推进时间 → 收获（→ 卖出）  
- 浏览器 online：登录后完成一轮种收；失败动作不改本地地块  
- 未登录仍可使用 **本地模式**（过渡期保留）

---

## 2. 非目标

- FarmDelta 多人房间广播、seq 补偿给访客（期 3）  
- 好友互助 / 偷菜 / 狗（期 4）  
- 完整乐观预测与失败回滚（只做等 Rsp）  
- 完整 CSV → Go/JS 代码生成  
- 压测、ABC 分档 write-behind  
- **移除本地模式**（online 稳定后另开清理任务；见 §8）

---

## 3. 交付节奏（方案 1）

| 子阶段 | 内容 | 验收 |
| --- | --- | --- |
| **2a** | `advance` + 主路径动作 + Buy（+ Sell）+ item 存储 + 服务端 smoke | smoke 绿 |
| **2b** | 客户端 online 意图接线 + 等 Rsp 更新 | 浏览器一轮种收 |
| **2c** | Fertilize + 多季 | 单测 + 可演示施肥/二季 |

---

## 4. 服务端设计

### 4.1 模块

| 包 | 职责 |
| --- | --- |
| `internal/farm` | Plot 完整字段子集、`advance`/`settleTo`、动作 validate/commit、快照含 bag/warehouse |
| `internal/gameconf` | 作物/价格/健康度/时间档最小常量（手写子集） |
| `internal/actor` | 不变；动作在 `Do(uid)` 内串行 |
| `internal/gateway` | 注册 cmd（见下） |
| `internal/store` | `item` 表持久化；SaveFarm/LoadFarm 含物品 |

### 4.2 命令（对齐 `protocol.md`）

| 阶段 | cmd | 名称 |
| --- | ---: | --- |
| 2a | 206 | Till |
| 2a | 208 | Clear |
| 2a | 210 | Plant |
| 2a | 212 | Water |
| 2a | 214 | RemoveWeed |
| 2a | 216 | RemovePest |
| 2a | 220 | Harvest |
| 2a | 302 | Buy |
| 2a | 304 | Sell（最小：卖仓库果实） |
| 2a | 306 | BagSnapshot（可选；Rsp patch 不足时用） |
| 2c | 218 | Fertilize |

地块请求体统一：

```text
PlotActionReq { owner_uid, plot_index, arg }
```

- 播种：`arg = crop_id`  
- 施肥：`arg = fertilizer_id`  
- 其余：`arg = 0`  

期 2 只允许 `owner_uid` 为 0 或自己（与期 1 EnterFarm 相同）；好友浇水留期 4。

### 4.3 执行模型

```text
advance(plot, now)
validate — 纯读，失败无副作用，返回 protocol 错误码
commit  — 写聚合，标脏，flush 沿用期 1 轻量路径
```

Rsp 回带 `client_seq`；成功时携带足够客户端 patch 的字段（变更地块 + 金币/背包/仓库增量或全量小节）。**期 2 不广播 FarmDelta。**

### 4.4 `advance` 深度

**必须：**

- 跨成熟 → 固化产量相关字段、进入 Mature  
- 跨枯萎 → Withered  
- Growing 上水分/草/虫对健康度的惰性结算（使 Water/Weed/Pest 有意义）  

**可简化：**

- 风险窗口伪随机：固定 `HazardSalt` + 可测；不强制一次做满架构属性测试全集  

**不变式：** 任何读写地块入口先 `advance`。

### 4.5 时间

- 服务端时间档可配置，默认 `demo`（或与 `.env` 一致）  
- 播种时将 `SeasonDuration` 按档折算写入地块（策划 3.3）  
- 单测 / smoke **注入 `now`** 或切 `bench`，禁止依赖真实长等待  

### 4.6 经济与物品

- 引入 MySQL `item` 表（`uid, kind, item_id, count`），与架构 5.8 一致  
- `Buy`：校验等级解锁与金币 → 扣币 → 加种子  
- `Sell`：扣仓库果实 → 加金币（不做道具回卖）  
- 错误码：`ERR_NOT_ENOUGH_COIN`、`ERR_CROP_LOCKED`、`ERR_SEED_NOT_OWNED`、`ERR_ITEM_NOT_SELLABLE` 等，照抄 protocol  

### 4.7 配置

- 期 2 允许 `gameconf`（Go）与 `client/src/game/config.js` 两份手写  
- smoke 断言关键价与白萝卜等成熟时长一致；完整 CSV 管线后置  

---

## 5. 客户端设计

### 5.1 双模式（过渡）

| | 本地模式 | online 模式 |
| --- | --- | --- |
| 进入条件 | 未登录 / 联调失败可选回退 | 登录 + Handshake + EnterFarm 成功 |
| 权威 | `state.js` + localStorage | 服务端；本地为镜像 |
| 写路径 | 现有 `doTill` 等 | WS 意图 → **等 Rsp** → `applyPatch` |
| NPC 好友 | 可玩假数据 | 隐藏或只读本地假数据（不做真偷菜） |

**后期：** online 稳定后移除本地权威路径（§8）。

### 5.2 模块

| 路径 | 职责 |
| --- | --- |
| `src/net/client.js` | 扩展动作与 Buy/Sell |
| `src/net/session.js` | token、online 标志、uid |
| `src/game/applyPatch.js` | Rsp/snapshot → 现有 state 形状 |
| `src/game/main.js` | online 分支改写点击/商店；尽量不改 `farm3d.js` |

### 5.3 等 Rsp 交互

1. 发出请求后可短暂禁用连点  
2. `err≠0`：toast 协议文案，不改地块  
3. `err==0`：patch；不足则 `BagSnapshot` 或再 EnterFarm  
4. 倒计时：本地时钟 + Ping 校时 offset  

---

## 6. 验收与测试

1. 表驱动：合法转移成功；非法转移返回正确 err 且无副作用  
2. `advance` 注入 `now` 的成熟/枯萎用例  
3. smoke：买种子→种植闭环（时间注入）  
4. 浏览器 online 一轮种收  
5. 未登录本地模式仍可打开 3D demo  
6. 2c：施肥缩短成熟；多季收获进入下一季 Growing  

---

## 7. 与期 1 / 期 3 的边界

| 能力 | 期 |
| --- | ---: |
| 登录 / 快照 / Redis+MySQL | 1（已完成） |
| 种植权威 + online 意图 | **2（本文）** |
| FarmDelta 多人同步 + 客户端全面镜像 | 3 |
| 好友 / 偷菜 | 4 |
| 压测 | 5 |
| 删除本地模式 | online 稳定后（建议 2b 验收通过后的清理 PR，或期 3 前） |

---

## 8. 技术债登记

| 项 | 触发条件 |
| --- | --- |
| 移除本地模式与 localStorage 权威 | online 种收稳定、答辩演示默认走登录 |
| CSV 双端生成 | 配置分叉开始疼时 |
| FarmDelta | 期 3 |
| `advance` 全量属性测试 | 期 2 后补强 |

---

## 9. 修订记录

| 日期 | 说明 |
| --- | --- |
| 2026-07-26 | 初稿：方案 1；主路径 A；商店 Buy；等 Rsp；本地模式过渡保留；化肥/多季 2c |
