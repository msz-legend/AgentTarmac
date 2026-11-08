# AgentTarmac 桌面客户端（决策 D）

Electron 原生壳：确保本地 daemon 运行 → 拉起 opencode/hermes 常驻 connector → 打开统一任务看板窗口。
Token 由服务端 BOOT 注入页面（`window.__TARMAC_BOOT__`），localStorage 仅作兜底。

## 开发运行

```powershell
# 1) 构建 daemon 与 connector（壳会自动拉起它们）
cd D:\DevTools\AgentHub
go build -o bin\agenthubd.exe .\cmd\agenthubd
go build -o bin\agenthub-connect.exe .\cmd\agenthub-connect

# 2) 安装依赖（首次会下载 Electron 运行时）
cd desktop
npm install

# 3) 启动
npm start
```

## 行为说明

- 优先连接 `AGENTHUB_DAEMON_URL`（默认 `http://127.0.0.1:8799`）上已运行的 daemon；
- 不可达时按顺序查找 `AGENTHUBD_PATH` → 仓库 `bin\agenthubd.exe` → 安装包内 `resources\bin\agenthubd.exe`，找到即拉起；
- **数据目录策略（token 一致性关键）**：`AGENTHUB_DATA` 环境变量 → 已存在 `token` 的本机共享目录 `D:\DevTools\AgentHub\data` → 回退 `%APPDATA%\AgentTarmac\data`。
  复用共享目录是为了让 daemon token 与 CC/Codex hook 中已配置的 token 保持一致，避免 hook 401 → fail-closed 全 deny；
- 仅当**本壳自己拉起** daemon 时才拉起 opencode/hermes connector（daemon 已在运行说明 `launcher\start.ps1` 已起好，避免重复进程）：
  - opencode：`AGENTHUB_OPENCODE_PATH` 或 `D:\DevTools\npm-global\node_modules\opencode-ai\bin\opencode.exe`，参数 `acp`；
  - hermes：`AGENTHUB_HERMES_PATH` 或 `%LOCALAPPDATA%\hermes\bin\hermes-acp.exe`，**无位置参数**（它本身就是 ACP stdio server），需设 `MSYS_NO_PATHCONV=1`；
- 外链走系统浏览器；关闭窗口即退出并回收本壳拉起的 daemon/connector 子进程；
- 单实例：重复启动只会聚焦已打开窗口（`app.requestSingleInstanceLock()`）。

## 打包安装程序

```powershell
# 目录级打包（不生成安装器，最快验证产物）
npm run pack

# NSIS 安装包 + 便携版（输出到 desktop\dist\）
npm run dist
```

产物：

| 文件 | 用途 |
|---|---|
| `desktop\dist\AgentTarmac-Setup-0.2.0.exe` | NSIS 安装包（创建桌面 + 开始菜单快捷方式，卸载器 `Uninstall AgentTarmac.exe`） |
| `desktop\dist\AgentTarmac-Portable-0.2.0.exe` | 免安装便携版，双击即用 |
| `desktop\dist\win-unpacked\AgentTarmac.exe` | 解包目录，等价于安装后内容 |

打包配置要点（`package.json` 的 `build` 字段）：

- `extraResources` 内嵌 `bin\agenthubd.exe` / `bin\agenthub-connect.exe` / `bin\hub-hook.exe` 与 `icon.ico`，因此安装后无需预装 daemon；
- `win.icon` = `icon.ico`（256×256，暖色圆角板 + Agent 节点连线）；
- `nsis.oneClick: false` + `allowToChangeInstallationDirectory: true`，用户可选安装目录；
- 未配置代码签名证书 → 安装包不签名（electron-builder 会提示 `signing is skipped`），安装时可能有 SmartScreen 提示。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `AGENTHUB_DAEMON_URL` | `http://127.0.0.1:8799` | 控制台地址 |
| `AGENTHUB_DATA` | 见上 | daemon 数据目录（决定 token） |
| `AGENTHUBD_PATH` | 自动查找 | daemon 可执行文件路径 |
| `AGETHUB_CONNECT_PATH` | 自动查找 | connector 可执行文件路径 |
| `HUB_HOOK_PATH` | 自动查找 | hub-hook 路径（传给 daemon 的 `-hook-path`） |
| `AGENTHUB_OPENCODE_PATH` / `AGENTHUB_HERMES_PATH` | 本机默认 | connector 目标 CLI |

## 已知边界

- **opencode/hermes 会话依赖本机已安装的 CLI**：安装包不含这两个第三方 CLI，缺失时壳会跳过对应 connector 并在日志中说明；
- **首次安装后需对齐 hook token**：daemon 在全新数据目录下会生成新 token，而 Claude Code / Codex 的 hook 配置指向本机共享数据目录的 token。本机部署沿用 `D:\DevTools\AgentHub\data` 即可保持一致；
- 从某些终端（如 DeepSeek Harness 会话）启动时，环境可能带 `ELECTRON_RUN_AS_NODE=1`，会使 Electron 二进制退化为纯 Node 并静默退出（exit 0）。桌面双击由 explorer.exe 启动不受影响；防御性入口见 `launcher\launch-native.vbs`。
- 自动更新（决策 F）依赖发布渠道与签名，规划于 M3（见 `HANDOFF.md`）；本地 v1 通过重打包更新。
