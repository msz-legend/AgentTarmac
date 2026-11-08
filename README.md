# AgentHub

**本地优先的 AI Agent 统一托管与审批网关。** 把 Claude Code / Codex 等 ACP 代理的会话接入一个本地 Hub：统一任务看板、审批门控（含审批记忆）、审计留痕、通知与归档——数据全程不出本机。

- 核心运行时：Go 1.26+，纯 Go SQLite（WAL），零 C 工具链依赖
- 接口：HTTP + WebSocket（默认 `127.0.0.1:8799`，Bearer 鉴权）
- 内嵌 Web 控制台：会话 / 审批队列 / 审批记忆 / 审计 / 事件流
- 平台：Windows 首发(macOS/Linux 理论可用，未验证)

## 快速开始（开箱即用）

```powershell
# 1. 启动 daemon（首次运行自动生成 API Token 并写入 data\token）
go build -o agenthubd.exe ./cmd/agenthubd
.\agenthubd.exe

# 2. 打开控制台，粘贴 Token
#    http://127.0.0.1:8799/
```

可选参数：

| 参数 | 默认 | 说明 |
|------|------|------|
| `-addr` | `127.0.0.1:8799` | 监听地址（默认仅回环） |
| `-data` | `data` | SQLite 与 token 文件目录 |
| `-token` / `$AGENTHUB_TOKEN` | 自动生成 | API Bearer Token |
| `-retention-days` | `90` | 会话保留天数，`0` 关闭清理（决策 G） |
| `-feishu-webhook` / `$AGENTHUB_FEISHU_WEBHOOK` | 空 | 飞书自定义机器人 webhook（决策 F） |
| `-feishu-secret` / `$AGENTHUB_FEISHU_SECRET` | 空 | 飞书机器人加签密钥 |

### 接入真实 Agent（stdio 连接器）

```powershell
go build -o agenthub-connect.exe ./cmd/agenthub-connect

# 桥接 claude-code-acp（JSON-RPC over stdio → daemon）
$env:AGENTHUB_TOKEN = (Get-Content data\token)
.\agenthub-connect.exe npx -y @zed-industries/claude-code-acp
```

连接器行为：Hub→agent 命令（`session/new` / `session/prompt` / `session/cancel`）经 WS `agent.request` 事件转发到 agent stdin；agent 的通知落库为帧；agent 主动发起的 `session/request_permission` 走 Hub 审批流水线，人工判决自动映射回 agent 自己的选项 id。`fs/*`、`terminal/*` 等客户端能力 v1 明确拒绝（不挂起）。

### 桌面客户端（Electron）

见 [`desktop/README.md`](desktop/README.md)：`npm install && npm start` 自动拉起 daemon 并打开窗口。

## 审批模型（决策 B/C）

| 风险 | 行为 |
|------|------|
| 低危（只读工具/只读命令） | 被动放行，全量留痕 |
| 中危（写文件、非只读命令） | 未命中记忆则进审批队列 |
| 高危（`rm -rf`、`format`、`reg`、`-EncodedCommand`、管道执行、force push…） | **一律拦截**，永不自动放行，不提供永久允许 |

审批选项：`仅本次允许` / `本会话内允许` / `永久允许（需二次确认）` / `仅本次拒绝` / `永久拒绝（需二次确认）`。
永久判决写入 `approval_memory`（键 `(agent_kind, tool, command_pattern)`），后续调用自动套用；控制台可随时撤销。记忆按 agent 类型隔离。

## API 速览

| 端点 | 说明 |
|------|------|
| `GET /healthz` | 存活探针（免鉴权） |
| `GET /` + `/ws` | Web 控制台 / 实时事件流 |
| `POST /acp/frame` | 入站 ACP 帧 `{agentKind, method, params}` |
| `POST /api/agent-requests` → `POST /api/agent-requests/{id}/response` | Hub→agent 命令总线（连接器消费） |
| `GET/POST /api/permission-requests[/{id}/decision]` | 审批队列与判决（`confirm=true` 为二次确认） |
| `GET /api/sessions` / `GET /api/sessions/{id}/export` | 会话列表 / 归档导出（决策 G） |
| `GET/DELETE /api/approval-memory[/{id}]` | 审批记忆查询 / 撤销 |
| `GET /api/approvals` / `GET /api/notifications` | 判决审计 / 通知记录 |

全部鉴权路由需 `Authorization: Bearer <token>`（WS 支持 `?token=`）。

## 架构

```
cmd/agenthubd          daemon：HTTP+WS、鉴权、内嵌控制台、保留期、通知
cmd/agenthub-connect   stdio↔daemon 连接器（桥接真实 ACP agent）
internal/acpclient     BaseAdapter（Claude Code / Codex）+ 审批门控 + SQLite 7 表
internal/server        Bridge（适配器注册/帧分发/WS 广播）+ Web + 飞书
internal/connector     JSON-RPC over stdio 协议层（含进程内假 agent 集成测试）
```

数据模型：`sessions` / `tool_calls` / `permission_requests` / `approval_history` / `approval_memory` / `notifications` / `migrations`。

## 开发

```powershell
go build ./... && go vet ./... && go test ./...
```

里程碑与决策记录见 [HANDOFF.md](HANDOFF.md)。

## 许可

Open Core（决策 H）：核心以 MIT 许可发布（见 [LICENSE](LICENSE)）；商业插件/企业版另行授权。
