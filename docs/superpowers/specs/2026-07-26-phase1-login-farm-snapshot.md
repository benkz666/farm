# 经典农场 · 期 1 规格：登录 → 进农场 → 快照

> 状态：已评审（头脑风暴 2026-07-26）  
> 工程规范：`2026-07-26-engineering-standards.md`  
> 协议依据：`docs/design/protocol.md`  
> 架构依据：`docs/design/architecture.md`（单进程形态 + 存储模型裁剪）  
> 初始数值：`docs/design/game-design-full.md` 4.2 节

---

## 1. 目标

交付一条可演示、可重启恢复的联通切片：

1. HTTPS 注册 / 登录，获得 token  
2. WebSocket 连接 + `Handshake`  
3. `EnterFarm`（自己的农场）返回全量 `FarmSnapshot`  
4. Redis + MySQL 轻量接入；`docker compose` 一键起依赖  
5. 现有 3D demo 在 **Vite + Vue 3** 工程内可继续本地权威游玩；`src/net/` 提供登录/拉快照联调，**不**反转本地种植权威（期 3 再做）

**成功标准：** `make smoke` 在 compose 依赖就绪后通过；杀掉进程再启动，同一账号仍能登录并看到同一初始农场数据。

---

## 2. 非目标（本期禁止范围蔓延）

- 种植 / 照料 / 收获等玩法动作  
- `FarmDelta` 广播、房间多人同步、seq 补偿（`farm_seq` 字段可返回，但无增量）  
- 好友、偷菜、任务、邮件、商店  
- 完整 ABC 分档 write-behind、Kafka、多实例分片路由  
- 客户端乐观预测与数据流反转  
- 压测  
- 完整 CSV → Go/JS 代码生成管线（可用手写 `gameconf` 最小常量）

---

## 3. 运行时形态

单二进制 `farm-server`：

- HTTP：`POST /api/register`、`POST /api/login`（路径对齐 `protocol.md` 5.1）；静态文件服务挂载 `client/`  
- WebSocket：游戏流量；子协议 `farm.v2.bin`
- 进程内：Gateway + Auth + Actor 运行时 + Farm 聚合 + Store  

```text
Register/Login → Auth → MySQL account/player/plots
        ↓ token
Handshake → Redis session
EnterFarm → Actor(uid) → Farm 聚合
                ↓ miss
         Redis farm:{uid} ← 回填 ← MySQL
```

---

## 4. 协议子集

### 4.1 传输

| 通道 | 用途 |
| --- | --- |
| HTTPS | 注册、登录 |
| WebSocket | Handshake、EnterFarm、Ping/Pong |

编码：JSON Envelope（字段对齐 `protocol.md` 1.3）：

```text
{ "cmd", "client_seq", "err", "payload" }
```

`payload` 为对应消息的 JSON 对象（开发期可读；与日后 binary 共用同一 `.proto` 字段号）。

### 4.2 命令（仅实现这些）

| cmd | 名称 | 说明 |
| ---: | --- | --- |
| 100 | Handshake | `token`；`resume_*` 可忽略；`client_config_ver` 与服务端常量不一致则 `ERR_CONFIG_STALE`（期 1 常量固定为 `1`） |
| 102 | Ping | 心跳 + 校时；应答同 cmd，payload 为 Pong |
| 200 | EnterFarm | `owner_uid`：`0` 或等于会话 uid 视为进自己的农场；**其它 uid 一律 `ERR_NOT_FRIEND`（1401）**，期 3/4 再放宽 |

Push / 种植类 cmd **只在 proto 注释或 reserved 中占位，不实现**。

### 4.3 HTTP Auth

| 接口 | 入参 | 出参 |
| --- | --- | --- |
| `POST /api/register` | `username`, `password` | `uid`, `token`, `ws_url` |
| `POST /api/login` | `username`, `password` | `uid`, `token`, `ws_url` |

密码：bcrypt；用户名唯一（单库 `account.username UNIQUE`）。  
HTTP 错误体使用与协议相同的数字码字段，例如：`ERR_USERNAME_TAKEN`（1103）、`ERR_BAD_CREDENTIAL`（1104）。

### 4.4 EnterFarmRsp（期 1）

对齐 protocol 2.2 子集：

- `snapshot`：见第 6 节  
- `farm_seq`：新号为 `0`，之后若有脏写可递增（无广播也无妨）  
- `server_time`：毫秒 Unix  
- `relation`：`SELF`（期 1 只允许进自己的农场时）

### 4.5 错误（必须返回的码）

| 场景 | 码 |
| --- | ---: |
| 用户名已占用 | 1103 `ERR_USERNAME_TAKEN` |
| 用户名或密码错误 | 1104 `ERR_BAD_CREDENTIAL` |
| 未带 / 无效 token | 1101 `ERR_UNAUTHORIZED` 或 1102 `ERR_TOKEN_EXPIRED` |
| 配置版本过期 | 1007 `ERR_CONFIG_STALE` |
| Enter 非自己且非 0 | 1401 `ERR_NOT_FRIEND` |
| 限流 | 1003 `ERR_RATE_LIMITED`（令牌桶容量 20、速率 10/s） |
| 参数错误 | 1002 `ERR_BAD_REQUEST` |

禁止返回未在 `protocol.md` 第 4 章登记的码。

---

## 5. 存储

### 5.1 MySQL 表（单库）

```sql
CREATE TABLE account (
  uid            BIGINT UNSIGNED PRIMARY KEY,
  username       VARCHAR(32) NOT NULL UNIQUE,
  password_hash  VARCHAR(255) NOT NULL,
  created_at     BIGINT NOT NULL
);

CREATE TABLE player (
  uid              BIGINT UNSIGNED PRIMARY KEY,
  nickname         VARCHAR(32) NOT NULL,
  level            SMALLINT UNSIGNED NOT NULL,
  exp              INT UNSIGNED NOT NULL,
  coin             BIGINT NOT NULL,
  unlocked_plots   TINYINT UNSIGNED NOT NULL,
  -- 以下为架构对齐的占位，期 1 写默认空值即可
  codex_bitmap     BINARY(8) NOT NULL,
  friend_ids       VARBINARY(1600) NOT NULL,
  daily_blob       VARBINARY(64) NOT NULL,
  pet_blob         VARBINARY(64) NOT NULL,
  farm_seq         BIGINT UNSIGNED NOT NULL,
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL
);

CREATE TABLE farm_plot (
  uid         BIGINT UNSIGNED NOT NULL,
  plot_index  TINYINT UNSIGNED NOT NULL,
  blob        VARBINARY(256) NOT NULL,
  PRIMARY KEY (uid, plot_index)
);
```

**注册事务：** 写 `account` + `player` + 18 行 `farm_plot`（或先写 `unlocked_plots` 行数，但推荐固定 18 行与架构一致，用 `unlocked_plots` 控制可操作数）。

**多库 username 占位流程**（架构 5.8）本期不做；规格注明演进时再引入 `username_index`。

### 5.2 Redis

| Key | 值 | TTL |
| --- | --- | --- |
| `session:{token}` | `uid` 字符串 | TTL **7 天**（可用环境变量覆盖） |
| `farm:{uid}` | 农场聚合 JSON | TTL **10 分钟**（与 Actor 空闲默认一致；加载时回填并续期） |

Redis `farm:{uid}` 与 MySQL `farm_plot.blob`：**聚合缓存用 JSON**；单地块 blob 用与 Go `Plot` 结构体一致的紧凑编码（推荐 encoding/gob 或固定顺序 binary，在 `store` 包内单一函数编解码）。禁止两处各写一套互不兼容的格式。

### 5.3 写路径（轻量，非 ABC 分档）

1. 内存 Actor 为在线权威  
2. 变更标脏  
3. **定时 flush（默认 1s）** 或注册/关键路径同步写入 MySQL  
4. 同时更新或删除后重写 `farm:{uid}`  

进程优雅退出：尽量 flush 再退出（超时兜底可短）。

### 5.4 加载路径

1. 查 Redis `farm:{uid}`  
2. miss → 读 MySQL `player` + `farm_plot` → 组装聚合 → SET Redis  
3. 同 uid 并发加载用 singleflight（或 Actor 邮箱排队）合并  

---

## 6. 领域：初始农场与快照

### 6.1 初始状态（策划 4.2）

| 项 | 值 |
| --- | --- |
| 等级 | 0 |
| 经验 | 0 |
| 金币 | 1000 |
| 解锁地块 | 6 |
| 地块总数 | 18（未解锁不可操作） |
| 已解锁地块状态 | 荒地（wasteland） |
| 背包 / 仓库 | 空 |
| nickname | 默认等于 username（可后改） |

### 6.2 Plot blob（期 1 最小字段）

内存与 blob 解码至少支持：

- `state`（荒地）  
- `crop_id` = 0  
- 其余时间字段为 0  

完整 Plot 结构见架构 5.2；期 1 不实现 `advance()`，但字段布局应避免期 2 推翻（推荐按架构 struct 序列化，未用字段置零）。

### 6.3 FarmSnapshot（JSON 形状示意）

```json
{
  "owner_uid": 1,
  "nickname": "alice",
  "level": 0,
  "exp": 0,
  "coin": 1000,
  "unlocked_plots": 6,
  "plots": [
    { "index": 0, "state": 0, "crop_id": 0 }
  ]
}
```

- `plots` **固定 18 项**（与架构固定数组一致）；客户端用 `unlocked_plots` 禁用交互。  
- `state` 为 **数值枚举**（与架构 `Plot.State` uint8 一致）：`0=wasteland`，其余值期 2 再启用；JSON 不用字符串，避免与 Go 枚举分叉。

---

## 7. Actor（期 1 最小行为）

- 每 uid 一个 Actor；mailbox 串行  
- 生命周期：首次消息加载 → 驻留 → 空闲超时疏散（默认 10min，可配置）→ flush  
- 路由表：逻辑上保留「分片 → 实例」概念，期 1 全部指向本进程实例 0  
- 处理消息：`EnterFarm`（组装快照）、后续期再挂动作 handler  

禁止在 Actor 内同步调用另一 Actor（死锁与分布式化前提）。

---

## 8. 客户端

| 项 | 约定 |
| --- | --- |
| 工程 | `client/` 为 Vite + Vue 3 根目录 |
| 3D | three.js 场景模块保留（由现有 `farm3d.js` 迁入 `src/game/` 或等价路径） |
| 权威 | 期 1 种植等仍本地权威可玩 |
| 网络 | `src/net/`：HTTP auth + WebSocket Envelope；提供「登录并拉快照」联调（控制台或简单面板打印 JSON） |
| 开发 | `npm run dev`；生产构建产物可由 `farm-server` 托管或 Vite preview |

---

## 9. 测试与验收

### 9.1 必须通过

1. 注册成功；重复用户名失败  
2. 登录返回 token；错误密码失败  
3. Handshake 成功；坏 token 失败  
4. EnterFarm 快照字段符合第 6 节  
5. 重启 `farm-server`（compose 不关）后登录，金币/地块与注册时一致  
6. Redis 清空后再次 EnterFarm，仍能从 MySQL 回填  

### 9.2 smoke

`make smoke`：HTTP 注册/登录 + WS Handshake + EnterFarm，断言 `err==0` 且 `unlocked_plots==6`、`coin==1000`。

---

## 10. 与后续期的接口预留

| 预留 | 期 1 做法 |
| --- | --- |
| `farm_seq` / delta ring | 字段存在；ring 可空实现 |
| `FarmStore` / `SessionStore` | 接口完整；仅 Redis+MySQL 实现 |
| Plot 完整字段 | 布局对齐架构，逻辑不跑 advance |
| Enter 他人农场 | 非本人返回 1401；期 3/4 再开 |

---

## 11. 修订记录

| 日期 | 说明 |
| --- | --- |
| 2026-07-26 | 初稿：联通切片、轻量 Redis/MySQL、JSON Envelope、客户端不反转 |
| 2026-07-26 | 客户端改为 Vite + Vue 3；net 联调模块路径改为 `src/net/` |
