# 许可说明

AgentHub 以 **MIT 许可** 发布，见仓库根目录 [LICENSE](../LICENSE)。

整个仓库——daemon、ACP 适配器、connector、PTY、Web 控制台、Electron 桌面壳与文档——均在 MIT 之下；**不存在另行授权的商业插件或企业版**。决策 H（Open Core）已由用户在 2026-11-09 废止，改为纯 MIT。

## 第三方依赖

- **仓库内 vendored 代码**：`@xterm/xterm@5.5.0`（`xterm.js` / `xterm.css`）与 `@xterm/addon-fit@0.10.0`（`addon-fit.js`），二者均为 MIT，完整版权声明见 [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)。本地 vendoring 是为了让内嵌终端在离线/移动端 SSL 受限环境也能工作，不依赖 CDN。
- **Go 模块**：`go.mod` / `go.sum` 声明，构建时拉取，各自遵循其原有许可。
- **Electron 桌面壳 npm 依赖**：`desktop/package.json` 声明；打包产物内另附 `LICENSE.electron.txt` 与 `LICENSES.chromium.html`。

## 商标

「AgentTarmac」/「AgentHub」名称与标识不在 MIT 授权范围内；商标使用另见 [trademark.md](trademark.md)。
