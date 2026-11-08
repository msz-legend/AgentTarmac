# API 参考

基 URL：`http://127.0.0.1:8799`（默认）。全部鉴权路由需 `Authorization: Bearer <token>`；WS 支持 `?token=` 查询参数。

## 免鉴权

| 端点 | 说明 |
|------|------|
| `GET /healthz` | 存活探针，返回 `{"agentKinds":[...],"status":"ok"}` |

## 控制台

| 端点 | 说明 |
|------|------|
| `GET /` + `GET /ws` | Web 控制台页面 + 实时事件流 WS |

## 会话

| 端点 | 说明 |
|------|------|
| `GET /api/sessions` | 会话列表 |
| `GET /api/sessions/{id}/export` | 会话归档导出 |
| `POST /api/sessions/{id}/continue` | 继续会话（F6：F6 任务重新派发为 `task-redispatch`） |
| `POST /api/sessions/{id}/fork` | 分叉继续 |
| `GET /api/sessions/{id}/terminal` | 在系统终端打开（PRD F8） |
| `GET /api/sessions/{id}/terminal-ws` | 内嵌终端 WebSocket（xterm.js + ConPTY） |

## 任务

| 端点 | 说明 |
|------|------|
| `POST /api/tasks` | 派发任务（F6 托管派发，仅 claude_code / codex） |
| `GET /api/tasks` | 任务列表 |
| `POST /api/tasks/{id}/retry` | 重试 |
| `POST /api/tasks/{id}/cancel` | 取消 |
| `POST /api/tasks/queue/pause\|resume` | 暂停 / 恢复队列 |

## 审批

| 端点 | 说明 |
|------|------|
| `GET /api/permission-requests?outcome=pending` | 待决列表（返回 `requestId`，判决必须使用 `requestId`，不是数字 `id`） |
| `POST /api/permission-requests/{id}/decision` | 判决 `{"decision":"allow_once\|allow_session\|allow_always\|reject_once\|reject_always","confirm":bool,"note":"..."}` |
| `GET /api/approval-memory` | 审批记忆 |
| `DELETE /api/approval-memory/{id}` | 撤销记忆 |

## ACP 桥接

| 端点 | 说明 |
|------|------|
| `POST /acp/frame` | 入站 ACP 帧 `{agentKind, method, params}` |
| `POST /api/agent-requests` | Hub→agent 命令总线 |
| `POST /api/agent-requests/{id}/response` | agent 响应回填 |

## 通知

| 端点 | 说明 |
|------|------|
| `GET /api/notify/status` | 桌面通知 / 飞书 / 企微状态 |
| `POST /api/notify/test` | 发送测试通知 |
| `POST /api/push/subscribe` | Service Worker Push 订阅 |
