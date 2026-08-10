# Slide 01 生成提示词

## 用途与风格

- Use case: `infographic-diagram`
- Asset type: 16:9 中文技术转正答辩 PPT 样稿
- 风格：清爽专业技术风，白色至极浅蓝背景，现代企业技术汇报，扁平矢量信息图，少量柔和阴影。
- 配色：Farm 使用深蓝 `#082B76 / #1555C0`，Redis 使用橙色 `#F04B13`，成功绿色 `#159447`，告警红色 `#E33333`。
- 字体：类似思源黑体 / Noto Sans SC 的粗体无衬线字体；标题、分区标题和图标明显加大、加粗。

## 页面结构

精确 16:9 横版。顶部标题与副标题；中部左右等宽镜像流程面板；左侧蓝色、右侧橙色；底部四张概念映射卡片和一条边界声明。保持严格栅格对齐、清晰阅读顺序和中高信息密度。

### 标题

“序号驱动的同步与恢复：FarmDelta 与 Redis PSYNC 的共同抽象”

“共同思想：快照基线 + 有序增量 + 进度标记 + 双路径恢复”

### 左侧：01 农场状态同步

1. 建立基线：`Snapshot + farm_seq = 100`
2. 正常增量：`Δ101 → Δ102 → Δ103`；`FarmDelta 按序应用`
3. 发现缺口：`last_seq = 101，却收到 103`
4. 发起恢复请求：`SyncFarm(from_seq = 101)`
5. 服务端恢复决策：
   - `环形缓冲仍覆盖` → `补发 Delta 102、103`
   - `缺口过大 / 历史淘汰` → `返回 Snapshot(seq = 103)`
6. 最终收敛：`客户端重新收敛到服务端权威状态`

### 右侧：02 Redis 主从复制（Primary / Replica）

1. 建立基线：`RDB Snapshot + replication ID`
2. 正常增量：`offset 1000 → … → 1023`；`持续复制命令流`
3. 连接中断：`副本保存旧 offset`
4. 发起恢复请求：`PSYNC(replid, offset)`
5. 主节点恢复决策：
   - `backlog 仍覆盖且历史匹配` → `Partial Resync：仅补缺失命令`
   - `backlog 不足 / replication ID 不匹配` → `Full Resync：RDB + 后续命令流`
6. 最终收敛：`副本重新追上 Primary`

### 底部映射

- `farm_seq ↔ replication offset`
- `Delta 环形缓冲 ↔ replication backlog`
- `SyncFarm ↔ PSYNC`
- `Snapshot ↔ RDB / Full Resync`

### 边界声明

“思想相似，层级不同：Farm 面向单农场的客户端状态收敛；Redis 面向数据集级 Primary→Replica 复制。Farm 不负责选主，也不是共识协议。”

## 图形与约束

使用粗重、统一的线性图标：文档/快照、增量序列、告警、请求、循环同步、盾牌、数据库和 backlog 环。禁止假 Logo、水印、页码、无关装饰、3D、重渐变、过小文字、乱码和额外标签。
