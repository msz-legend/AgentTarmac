# 安全边界

AgentHub 是**本地优先**的 AI Agent 统一托管与审批网关。所有设计决策均围绕“不出本机、不修改已安装 Agent 原始配置、不绕过宿主沙箱”展开。

## 核心原则

| 原则 | 说明 |
|------|------|
| 本地优先 | 数据（SQLite、token、审计日志）全部存储在本机 `data/` 目录，**不上云** |
| 不修改 Agent 配置 | 不覆盖 Claude Code / Codex 的 `settings.json` / `config.toml`；仅通过 `--settings` 临时文件注入 hook |
| 不绕过宿主沙箱 | 不尝试提取或修改 DSH / CC Safety Net 的安全边界；Hook 触发走 Agent 自身机制 |
| fail-closed | Hub 不可达时，hook 显式 deny（Codex：显式拒绝；CC：技能修复路径） |
| 审计留痕 | 所有权限请求与判决落库，支持导出/归档 |

## 接口鉴权

- HTTP API 需 `Authorization: Bearer <token>`。
- WebSocket 支持 `?token=` 查询参数（浏览器 WS 无法设置 header）。
- Token 首次运行自动生成并写入 `data\token`，长度 64 字符。

## 审批门控

- 低危（只读工具）：被动放行，全量留痕。
- 中危（写文件、非只读命令）：未命中记忆则进审批队列。
- 高危（`rm -rf`、`force push`、`reg`、管道执行等）：**一律拦截**，永不自动放行。

## 已知边界

- 代码签名：安装包**未签名**，Windows SmartScreen 可能拦截（M3 计划申请 OV 证书或自签名）。
- Codex 受限沙箱：CreateProcessAsUserW 需 `SeAssignPrimaryTokenPrivilege`，当前主机不具备，Codex 受限沙箱闭环无法在本主机验证（非产品缺陷）。
- macOS/Linux：provider 代码已就绪，但无设备验证。
