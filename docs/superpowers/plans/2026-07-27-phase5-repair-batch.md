# 期 5 已完成修复批次整合提交计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:verification-before-completion before declaring this historical integration complete. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 `d35a4f0` 之后已经完成的修复，以一条完整、可验证的代码提交落库。

**Architecture:** 本计划是历史修复的整合记录，不重新实现功能。`2026-07-27-phase5-repair-batch.md` 规格逐项定义已存在的行为与验收；代码提交保留当前工作树中所有对应源代码、测试、生成器、生成物、迁移、文档和 `demo/`，以全量验证保证整合状态正确。

**Tech Stack:** Go 1.22+、MySQL、Redis、Prometheus、Three.js、Node.js、Git。

## Global Constraints

- 规格：`docs/superpowers/specs/2026-07-27-phase5-repair-batch.md`。
- 基线固定为 `d35a4f0`；不得改写更早历史，且不得用 reset/checkout 丢弃工作树内容。
- 历史中允许有规格/计划文档提交；所有**代码修复**只创建一条整合提交。
- 整合提交使用中文 Conventional Commit，且不使用 `--no-verify`。
- 不新增或混入 Claude 最终审查列出的 Important 项；只提交当前已经完成的修复。
- `docs/2026-07-27-repair-task.md`、`server/cmd/mig9/main.go` 已移除；`.superpowers/sdd/progress.md` 保持本地忽略、不提交。

---

## File Structure

```text
docs/superpowers/specs/2026-07-27-phase5-repair-batch.md  # 修复范围、边界与验收
docs/superpowers/plans/2026-07-27-phase5-repair-batch.md  # 本整合流程
server/...                                         # P0–P3 服务端修复与测试
client/...                                         # 配置、重连、资源释放与测试
config/crops.csv + tools/gen-config/...            # 作物配置源、生成器与生成物
demo/...                                           # 独立静态演示资源，不被生产构建引用
```

### Task 1: 校验并整合已完成修复

**Files:**
- Modify/Create/Delete: 当前工作树中由 `git status --short` 列出的 P0-1 至 P3-4 修复文件、测试、迁移、配置生成器、生成物、容量文档和 `demo/`。
- Exclude: `.superpowers/sdd/progress.md`、已删除的临时审查工作稿、未在规格范围内的新增功能。

**Interfaces:**
- Consumes: `docs/superpowers/specs/2026-07-27-phase5-repair-batch.md` 的 P0-1 至 P3-4 验收定义。
- Produces: 一条包含完整已修复状态的整合提交。

- [x] **Step 1: 审查完整提交边界**

运行：

```bash
git status --short
git diff --stat
git diff --cached --stat
```

预期：除本规格中列明的修复、测试、生成物、迁移、文档和 `demo/` 外，没有待提交文件。

- [x] **Step 2: 运行服务端验证**

运行：

```bash
cd server && go test ./...
cd server && go test ./internal/farm -run '^$' -bench 'Benchmark(Advance|Settle|Hazard|Yield)' -benchmem -count=1
```

预期：全部测试通过；基准输出包含 `allocs/op`，零分配检查由普通测试覆盖。

- [x] **Step 3: 运行客户端与配置验证**

运行：

```bash
cd tools && go test ./gen-config/ -count=1
cd .. && make gen-check
cd client && node --test src/**/*.test.js
cd client && npm run build
```

预期：生成物确定且与 CSV 同步；客户端测试和生产构建通过。

- [x] **Step 4: 校验暂存内容**

运行：

```bash
git add -A
git diff --cached --check
git diff --cached --stat
```

预期：无空白错误；统计包含 P0–P3、`demo/` 和与修复有关的文档，且不含本地账本。

- [x] **Step 5: 创建唯一代码提交**

提交：

```text
fix: 整合农场可靠性与客户端韧性修复
```

- [x] **Step 6: 提交后确认**

运行：

```bash
git status --short
git log --oneline -3
```

预期：工作树干净；最新代码提交紧随修复规格与计划文档提交之后。

## Final Review

- [x] 使用与实现者不同的审查模型审查整合提交相对 `d35a4f0` 的完整差异。
- [x] 审查只确认本规格范围的已完成修复；未实施的 Important 项已保留为后续工作，未被标记为本批次完成。
