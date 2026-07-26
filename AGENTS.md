# AGENTS.md — 经典农场

本仓库的 Agent 工作协议见：

- [`.cursor/rules/agent-protocol.mdc`](.cursor/rules/agent-protocol.mdc)（始终生效）

摘要：

- **浏览器测试**默认用 ego-lite（ego-browser）
- **Git**：约定式前缀 + 中文主题
- **子代理模型**：一般 `cursor-grok-4.5-high`；较复杂 `gpt-5.6-terra-high`；简单 review `glm-5.2-high`；不复杂前端 review `gpt-5.6-luna-high`；很难用 `gpt-5.6-sol-xhigh`；特别难或大量 UI 前端先问用户
- **指派任务用中文**
- 代码/注释/测试风格详见上述 rule；规格见 `docs/superpowers/specs/`
