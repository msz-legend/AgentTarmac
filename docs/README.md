# AgentHub 文档站点

本目录是 AgentHub 项目的开源文档站点源，采用纯静态 Markdown，可直接部署到 GitHub Pages / Vercel / Netlify。

## 目录结构

```
docs/
├── index.md            # 站点首页（项目定位、快速开始、架构概览）
├── getting-started.md  # 安装与首次运行（daemon / 连接器 / 桌面客户端）
├── approval-model.md   # 审批模型（低危/中危/高危、审批记忆、二次确认）
├── api-reference.md    # HTTP + WebSocket API 完整参考
├── events-model.md     # 事件模型 v0.2（帧、状态机、审计留痕）
├── desktop.md          # Electron 桌面客户端（打包、NSIS、token 复用）
├── ptys-terminal.md    # xterm.js + ConPTY 内嵌终端（PRD F8）
├── security.md         # 安全边界（不修改已安装 Agent 配置、不绕过 DSH/CC Safety Net、本地优先不上云）
├── roadmap.md          # 里程碑路线图（M1 / M1.5 / M2 / M3）
├── contributing.md     # 贡献指南（分支策略、提交规范、CI 触发）
├── code-of-conduct.md  # 社区行为准则
├── license.md          # 许可说明（Open Core：核心 MIT，商业插件/企业版另行授权）
└── trademark.md        # 商标检索记录（M3 决策 H 附件）
```

## 设计原则

- 纯静态，无构建依赖，任意静态托管商即可部署。
- 中英文双语（zh-CN / en），优先中文。
- 每页顶部标注“决策字母（A–H）”与“里程碑（M1/M1.5/M2/M3）”，方便追溯决策来源。
