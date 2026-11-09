# AgentHub 文档

AgentHub 是**本地优先的 AI Agent 统一托管与审批网关**。把 Claude Code / Codex / OpenCode / Hermes 等 ACP 代理接入一个本地 Hub：统一任务看板、审批门控（含审批记忆）、审计留痕、通知与归档——数据全程不出本机。

## 快速开始

### 1. 启动 daemon

```powershell
go build -o agenthubd.exe ./cmd/agenthubd
.\agenthubd.exe
# 首次运行自动生成 API Token 并写入 data\token
```

### 2. 打开控制台

浏览器访问 `http://127.0.0.1:8799/`，粘贴 `data\token` 中的 Token。

### 3. 桥接真实 Agent

```powershell
go build -o agenthub-connect.exe ./cmd/agenthub-connect
$env:AGENTHUB_TOKEN = (Get-Content data\token)
.\agenthub-connect.exe npx -y @zed-industries/claude-code-acp
```

### 4. 桌面客户端（Electron）

```powershell
cd desktop
npm install
npm start      # 开发模式，自动拉起 daemon 并打开窗口
npm run dist   # 打包 NSIS 安装包（Win 32/64）
```

## 核心能力

| 模块 | 说明 | 决策 |
|------|------|------|
| 统一任务看板 | 创建任务 → 审批放行 → 完成，全程不出 Hub | A |
| 审批门控 | 低危放行 / 中危审批 / 高危永不自动放行 | B/C |
| 审批记忆 | 永久判决按 (agent_kind, tool, command_pattern) 自动套用 | B |
| 事件流 / 审计 | 完整帧留痕，SQLite 本地存储，90 天保留 | G |
| 内嵌终端 | xterm.js + ConPTY + WebSocket 浏览器内嵌 PTY | F8 |
| 企微通知 | 高危审批 / 会话失败推送（-wecom-bot-token） | F |

## 里程碑

| 里程碑 | 内容 | 预计 |
|--------|------|------|
| M1 | ACP 适配器、审批闭环、F7/F9 三件套、全新安装冒烟 | 交付 |
| M1.5 | Claude Code / Codex 运行时适配器 + 审批记忆 | 交付 |
| M2 | Web + Electron 客户端、统一任务看板、通知/自动更新 | 进行中 |
| M3 | 开源运营（文档站 / CI / Releases / 商标检索） | 本页 |

## 许可

MIT 许可，全仓库统一，见 [license.md](license.md) 与根目录 [LICENSE](../LICENSE)。
