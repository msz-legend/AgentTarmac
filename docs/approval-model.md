# 审批模型

AgentHub 的审批机制是核心安全边界（决策 B / C）。数据全程不出本机，不修改已安装 Agent 的原始配置，不绕过 DSH / CC Safety Net。

## 三级风险行为

| 风险 | 行为示例 | 处理策略 |
|------|----------|----------|
| 低危 | 只读工具（Read/Grep/Glob/LS/TodoRead/NotebookRead） | 被动放行，全量留痕 |
| 中危 | 写文件、非只读命令 | 未命中记忆则进审批队列 |
| 高危 | `rm -rf`、`format`、`reg add/delete/import`、管道执行、force push、`-EncodedCommand` | **一律拦截**，永不自动放行，不提供永久允许 |

## 审批选项

| 选项 | 含义 | 记忆 |
|------|------|------|
| 仅本次允许 (`allow_once`) | 本次工具调用放行 | 不写记忆 |
| 本会话内允许 (`allow_session`) | 本会话内同类请求自动放行 | 写会话作用域记忆 |
| 永久允许 (`allow_always`) | 全局自动放行 | **需二次确认**，写全局记忆 |
| 仅本次拒绝 (`reject_once`) | 本次拒绝 | 不写记忆 |
| 永久拒绝 (`reject_always`) | 全局自动拒绝 | **需二次确认**，写全局记忆 |

## 审批记忆

- 键：`(agent_kind, tool, command_pattern)`，按 agent 类型隔离。
- 永久判决写入 `approval_memory` 表，后续调用自动套用。
- 控制台可随时撤销（`DELETE /api/approval-memory/{id}`）。
- 中危操作未命中记忆前需人工判决；高危操作永远不自动放行。

## API

| 端点 | 说明 |
|------|------|
| `GET /api/permission-requests?outcome=pending` | 待决审批列表 |
| `POST /api/permission-requests/{id}/decision` | 判决（`decision` 字段，`confirm` 二次确认） |
| `GET /api/approval-memory` | 审批记忆查询 |
| `DELETE /api/approval-memory/{id}` | 撤销记忆 |
| `GET /api/approvals` | 判决审计 |
