# 路线图

| 里程碑 | 核心内容 | 状态 |
|--------|--------|------|
| **M1** | ACP 适配器（DSH / 厂商无关契约）、审批闭环（allow/reject/allowSession）、F7/F9 三件套（会话续跑 / 浏览器通知 / 飞书 webhook）、全新安装冒烟 32/32、八包测试全绿 | ✅ 交付 |
| **M1.5** | Claude Code / Codex 运行时适配器 + 审批记忆 SQLite 7 表、go build 验证、换核重启 | ✅ 交付 |
| **M2** | Web + Electron 桌面客户端、统一任务看板、桌面通知 + 企微/飞书 webhook、自动更新、xterm.js 内嵌终端 | 🔄 进行中 |
| **M3** | 开源运营（文档站 / CI / Releases / 商标检索）、代码签名、macOS 验证 | ⏳ 计划中 |

## 非目标（明确排除）

- 云端同步 / SaaS 托管（决策：本地优先，不上云）
- xterm.js 实时终端（F8）归 M2（已实现）
- Service Worker 通知（F9）归 M1.5（已实现，推送改道企微）
- 系统终端跳转（Q-F1 / Q-U9）归 M2（已实现：OS 系统终端）
- F6 统一操作面归 M2（托管派发仅 claude_code / codex）
