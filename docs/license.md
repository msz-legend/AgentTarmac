# 许可说明

AgentHub 采用 **Open Core** 许可模式（决策 H）。

## 核心代码（MIT）

`internal/`、`cmd/`、`desktop/`、`launcher/` 等核心运行时、API、控制台、审批引擎以 **MIT 许可** 发布，见仓库根目录 `LICENSE`。

## 商业插件 / 企业版

以下部分不属于核心 MIT 代码，另行商业授权：

- 企业级身份鉴权与多用户隔离扩展
- 飞书 / 企微 webhook 的企业级审计与告警扩展
- 私有部署支持与 SLA 服务

如需商业授权，请联系项目维护者。

## 第三方依赖

本项目依赖的第三方库（Go module、xterm.js CDN、Electron 等）遵循其各自的开源许可，详见各模块 `go.mod` / `package.json`。
