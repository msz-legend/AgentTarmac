# xterm.js + ConPTY 内嵌终端

AgentHub 控制台内置浏览器内嵌终端（PRD F8 / D15），基于 **xterm.js + Windows ConPTY + WebSocket**，支持 `claude_code` / `codex` 会话的交互式操作。

## 架构

```
浏览器 (xterm.js)
    │ WebSocket (binary, token 鉴权)
    ▼
Daemon GET /api/sessions/{id}/terminal-ws
    │ ConPTY (conpty.Start)
    ▼
claude --resume <id> / codex resume <id>  (经 powershell.exe 包装)
```

## 后端实现

- `internal/pty/pty.go`：`Session` 封装 `conpty.ConPty`，提供 `Write/Read/Resize/Close` 及 `Pump` 双向转发。
- `internal/server/console.go`：`handleSessionTerminalWS` — 认证 → 获取 session → `pty.NewSession` → websocket 升级 → `pty.Pump`；会话结束自动注销并关闭 PTY。
- `internal/server/server.go`：路由 `GET /api/sessions/{id}/terminal-ws`，`ptySessions` 注册表（`sync.Mutex` 保护）。

## 前端实现

- `index.html` 引入 xterm.js@5.5.0 CSS/JS（CDN）。
- 新增 `view-terminal` 视图与导航项（`nav.terminal`）。
- 打开会话卡片的「内嵌终端」按钮 → `openTerminal(sessionId)` → 建立 WS → `term.attach()` → 键盘输入 → PTY → agent  stdout → xterm 渲染。
- 支持窗口大小自适应（`term.fit()`）、清屏、聚焦。

## 依赖

| 包 | 版本 | 来源 |
|----|------|------|
| xterm.js | 5.5.0 | CDN (jsdelivr) |
| github.com/UserExistsError/conpty | v0.1.4 | Go module (indirect → direct 经 go mod tidy) |
| golang.org/x/sys | v0.49.0 | Go module (indirect → direct 经 go mod tidy) |
| gorilla/websocket | v1.5.3 | Go module (已 direct) |

## 平台限制

- **Windows**：ConPTY 全功能可用。
- **非 Windows**：`pty.NewSession` 返回 `ErrUnsupported`，构建不报错但运行时不可用（首版仅 Windows，macOS/Linux 未验证）。

## 已知边界

- 终端内运行的是 `claude --resume <id>`（由 `powershell.exe -NoProfile -Command` 包装），与系统终端行为一致。
- 会话若为 F6 托管派发（一次性 headless CLI，进程已退出），内嵌终端不可复用，建议使用「重新派发任务」。
