# Snapshot + FarmDelta + farm_seq + SyncFarm 状态同步机制

状态：内容大纲、主风格和内置图片后端已确认；A 版和手绘白板对照版均已生成，等待用户选择最终风格。

- 当前样稿：`origin_image/slide_01.png`
- 生成提示：`prompts/slide_01.md`
- 白板对照版：`variants/slide_01-whiteboard.png`
- 白板重绘提示：`prompts/slide_01-whiteboard.md`
- 前端—服务器时序版：`variants/slide_01-sequence-v2.png`
- 时序版重绘提示：`prompts/slide_01-sequence-v2.md`
- 双时序图清爽专业版：`variants/slide_01-dual-sequence-clean-v3.png`
- 双时序图生成提示：`prompts/slide_01-dual-sequence-clean-v3.md`

## 视觉风格决策

- 主方案：清爽专业技术风。白底、深蓝 Farm、橙色 Redis、左右镜像流程、粗体无衬线字体、加粗线性图标、中高信息密度。
- 对照方案：手绘白板风。近白背景、深蓝与橙色马克笔线条、轻微手绘不规则感、保留严谨的序号与恢复分支，不使用卡通人物或凌乱涂鸦。
- 对照规则：两版保持标题、流程节点、概念映射和边界声明一致，只改变视觉表达方式。

## 第 1 页：序号驱动的同步与恢复——FarmDelta 与 Redis PSYNC 的共同抽象

- 页面角色：关键机制讲解 / 横向类比
- 核心结论：两者都可抽象为“快照基线 + 有序增量 + 进度标记 + 双路径恢复”，但解决的问题与一致性边界不同。
- 版式：左右镜像双栏；左侧为农场状态同步，右侧为 Redis 主从复制；底部为概念映射和边界声明。

### 左侧：农场状态同步

1. 建立基线：`Snapshot + farm_seq = N`
2. 正常增量：服务端产生连续、有序的 `FarmDelta(N+1...)`
3. 客户端按序应用 Delta，并推进本地 `farm_seq`
4. 发现序号缺口或重连：调用 `SyncFarm(from_seq)`
5. 服务端恢复决策：
   - 环形缓冲仍覆盖缺失区间：补发 FarmDelta
   - 缺口过大、历史淘汰或状态不可信：降级返回新 Snapshot
6. 最终收敛：客户端恢复到服务端权威状态

### 右侧：Redis Primary / Replica 复制

1. 建立基线：RDB Snapshot + replication ID / offset
2. 正常增量：持续复制命令流并推进 offset
3. 连接中断：副本保留旧的 replication ID / offset
4. 发起 `PSYNC(replid, offset)`
5. 主节点恢复决策：
   - backlog 仍覆盖且复制历史匹配：Partial Resync
   - backlog 不足或 replication ID 不匹配：Full Resync（RDB + 后续命令流）
6. 最终收敛：Replica 重新追上 Primary

### 底部概念映射

- `farm_seq` ↔ replication offset
- FarmDelta 环形缓冲 ↔ replication backlog
- `SyncFarm` ↔ `PSYNC`
- Snapshot ↔ RDB / Full Resync

### 边界声明

思想相似，层级不同：Farm 面向单农场的客户端状态收敛；Redis 面向数据集级 Primary→Replica 复制。Farm 机制不负责选主，也不是共识协议。

## 用户素材处理

- 参考图：`/root/.codex/generated_images/019fe4e4-4516-7c02-9c60-d3764d2d94e7/exec-aeb6279d-ec60-46a7-ab30-19f299c78cb5.png`
- 用途：仅作为内容结构和信息密度参考；选定风格后重新绘制，不要求原图直接出现在最终页面中。
