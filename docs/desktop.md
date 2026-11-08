# Electron 桌面客户端

AgentHub 桌面客户端基于 Electron（v40.10.2）+ electron-builder（v26），Windows 首发（NSIS 安装包 + 便携版）。

## 打包产物

| 产物 | 大小 | 说明 |
|------|------|------|
| `AgentTarmac-Setup-0.2.0.exe` | 116.8 MB | NSIS 安装包（oneClick, perMachine: false, 可选安装目录，创建桌面 + 开始菜单快捷方式） |
| `AgentTarmac-Portable-0.2.0.exe` | 105.59 MB | 免安装便携版 |
| `dist/win-unpacked/` | 204.04 MB | 解包目录（含 `resources\bin\{agenthubd,agenthub-connect,hub-hook}.exe`） |

## 构建命令

```powershell
# 开发
cd desktop
npm install
npm start        # electron . （自动拉起 daemon 并打开窗口）

# 打包
npm run pack     # --dir 解包目录
npm run dist     # electron-builder --win nsis 打包安装包 + 便携版
```

## 运行时行为

1. **复用优先**：若 `127.0.0.1:8799` 已有 daemon 监听，直接复用，不重复拉起。
2. **冷启动**：若无 daemon，壳自行拉起 `resources\bin\agenthubd.exe`，数据目录复用 `D:\DevTools\AgentHub\data`（与 CC/Codex hook 共享同一 Token，避免 401）。
3. **常驻连接器**：壳启动后自动为 `opencode` / `hermes` 启动常驻 `agenthub-connect`（ACP stdio → daemon WS）。
4. **单实例**：`app.requestSingleInstanceLock()`，重复启动聚焦现有窗口。
5. **清理**：关闭窗口时杀掉壳本身 spawn 的 connector 与 daemon 进程。

## 环境变量

| 变量 | 说明 |
|------|------|
| `AGENTHUB_DAEMON_URL` | daemon 地址（默认 `http://127.0.0.1:8799`） |
| `AGENTHUB_DATA` | 数据目录（默认 `D:\DevTools\AgentHub\data`） |
| `AGENTHUBD_PATH` | daemon 二进制路径 |
| `AGETHUB_CONNECT_PATH` | 连接器二进制路径 |
| `HUB_HOOK_PATH` | hub-hook 二进制路径 |
| `AGENTHUB_OPENCODE_PATH` | OpenCode 可执行文件 |
| `AGENTHUB_HERMES_PATH` | Hermes 可执行文件 |

## 已知边界

- 未签名（SmartScreen 可能拦截，需用户“更多信息 → 仍要运行”）。
- 自动更新归 M3，当前版本需手动下载新安装包。
- 首版仅 Windows；macOS/Linux 理论可用，未验证（darwin provider 代码已就绪）。
