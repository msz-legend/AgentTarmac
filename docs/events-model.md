# 事件模型（Agent Event Model v0.2）

AgentHub 的事件模型是 Hub 内所有会话、审批、任务、审计留痕的统一载体。事件通过 WebSocket `/ws` 广播到控制台，同时落库到 SQLite。

## 事件类型

| 类型 | 说明 | 字段 |
|------|------|------|
| `agent.message_chunk` | Agent 消息流片段 | session_id, content |
| `agent.thought_chunk` | Agent 思考片段 | session_id, content |
| `tool_call` | 工具调用开始 | toolCallId, title, kind, status, rawInput |
| `tool_call_update` | 工具调用状态更新 | toolCallId, status, content |
| `tool_result` | 工具调用结果 | toolCallId, tool, status |
| `session/request_permission` | 权限请求（触发审批门） | toolCallId, options[{optionId,name,kind}], rawInput |
| `permission_resolved` | 审批判决结果 | requestId, outcome, decision, applied |
| `usage_update` | Token 用量更新 | used, size |
| `plan` | 执行计划 | session_id, steps |
| `task` | 任务派发 / 完成 | action, session_id, mode, request_id |

## 状态机

```
unknown → discovered → active → (running | awaiting_approval | finished | failed)
                                           ↑
                                   permission resolved
```

## 审计留痕

- 所有 `permission_request` 与 `permission_resolved` 入库 `permission_requests` / `approval_history`。
- 会话保留期默认 90 天（决策 G），过期自动清理。
- 数据本地 SQLite，不上云。
