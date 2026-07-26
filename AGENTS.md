# AGENTS.md — 经典农场

本仓库的 Agent 工作协议见：

- [`.cursor/rules/agent-protocol.mdc`](.cursor/rules/agent-protocol.mdc)（始终生效）

摘要：

- **浏览器测试**默认用 ego-lite（ego-browser）
- **Git**：约定式前缀 + 中文主题
- **子代理模型**：派发 Task 必须显式指定 `model`。一般实现（含多模态）用 `cursor-grok-4.5-high`；简单实现用 `glm-5.2-high`；多模块、并发、存储或协议网关等复杂任务用 `gpt-5.6-terra-high` 或 `gpt-5.6-sol-high`；特别难或大量 UI 前端先询问用户。
- **审查与裁决模型**：简单非前端审查用 `glm-5.2-high`；简单前端审查用 `gpt-5.6-luna-high`；复杂审查用 `gpt-5.6-sol-xhigh`；非常难的方案判断或最终技术决策用 `claude-opus-5-max`，慎用该模型。审查优先与同一任务的开发模型不同。
- **指派任务用中文**
- 代码/注释/测试风格详见上述 rule；规格见 `docs/superpowers/specs/`
