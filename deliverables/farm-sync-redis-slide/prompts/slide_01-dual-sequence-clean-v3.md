# Slide 01 双时序图清爽专业版

- Use case: `precise-object-edit`
- 风格：`codex-ppt/references/清爽专业风.md`
- 编辑参考：`variants/slide_01-sequence-v2.png`
- 目标：Farm 与 Redis 均改为从上到下的双生命线时序图，并显著降低文字密度。

## 页面文案

- 标题：`同一种恢复范式：FarmDelta 与 Redis PSYNC`
- 副标题：`基线 + 增量 + 位点 + 双路径恢复`

### 01 农场状态同步

- 参与方：`前端`、`服务器`
- 服务器组件：`Delta Buffer`，槽位 `101 / 102 / 103`
- 消息：
  - `Snapshot · seq=100`
  - `Δ101`
  - `Δ102`，红色虚线并标记 `丢失`
  - `Δ103`
  - 前端提示：`发现缺口：期待 102`
  - `SyncFarm(101)`
  - 判断：`缓冲命中` / `历史淘汰`
  - 恢复：`补发 Δ102–103` / `Snapshot · seq=103`
  - 结果：`seq=103 · 状态收敛`

### 02 Redis 主从复制

- 参与方：`Replica`、`Primary`
- Primary 组件：`backlog`，槽位 `1001 / … / 1023`
- 消息：
  - `RDB · offset=1000`
  - `命令流 1001…`
  - `连接中断`
  - `PSYNC(replid, 1001)`
  - 判断：`backlog 命中` / `历史失效`
  - 恢复：`Partial Resync` / `Full Resync · RDB`
  - 结果：`offset=1023 · 副本追平`

### 底部映射与结论

- `farm_seq ↔ offset`
- `Delta Buffer ↔ backlog`
- `SyncFarm ↔ PSYNC`
- `Snapshot ↔ RDB`
- `同一恢复范式，不同复制层级：Farm 解决单农场客户端收敛；Redis 解决 Primary→Replica 数据复制。`

## 视觉约束

白色至浅蓝背景、专业蓝与克制橙色、粗线性图标、极轻阴影；两侧镜像、严格对齐；消息直接写在箭头上；禁止阶段编号、长段落、密集卡片、3D、渐变、Logo、水印和乱码。
