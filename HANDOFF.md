# AgentHub Handoff 文档

> 生成时间：2026-11-11（Asia/Shanghai）；上一版 2026-11-10 23:20（已归档 docs/HANDOFF-archive-20261107.md 与本文旧节）
> 版本基线：`v0.2.0-m2`，git HEAD `3bc1d73`（三轮点击回归后的修复批次链：6b5ea28 → 3bc1d73 → R3 批次）
> 项目根目录：`D:\DevTools\AgentHub`
> Go 模块：`github.com/agenttarmac/agenttarmac`（go 1.26，本机 Go 1.27.0，Windows amd64，CGO_ENABLED=0）
> 运行端口：`127.0.0.1:8799`（默认，可通过 `-addr` 覆盖）
> 数据目录：`-data`（默认 `./data`，SQLite WAL + `token` 文件）
> 编译状态：`go build ./...` / `go vet ./...` / `gofmt -l .` 全净；`go test ./...` 全绿（12 个包）
> Go 包清单：`acpclient` / `connector` / `platform` / `pty` / `push` / `relay` / `rules` / `runner` / `server` / `tasks`
> 许可：纯 MIT（决策 H 已废止 Open Core，见 `LICENSE` / `docs/license.md` / `THIRD_PARTY_NOTICES.md`）

---

## 1. 版本基线与环境

### 1.1 git 状态（2026-11-10 23:20）

```
HEAD = a01bb01 docs(license): unify repository under MIT, retire Open Core decision H
M cmd/agenthubd/main.go
M internal/server/web/index.html
M internal/tasks/tasks.go
?? internal/tasks/acp_task.go
?? qa/pm-review-20261110/
```

未提交的四个文件是 **m17006 派发四选项（opencode/hermes）** 的改动，代码已 `go build`/`go vet`/`gofmt`/`go test` 全过，但尚未 commit。接手前建议先 commit 这四个文件，再进入后续修复。

### 1.2 关键旗标与默认值

| 旗标 | 默认 | 说明 |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8799` | 仅回环监听，生产如需局域网需显式改址 |
| `-data` | `data` | SQLite 与 token 目录 |
| `-token` / `$AGENTHUB_TOKEN` | 自动生成 32 字节 hex，写入 `data/token` | Bearer 鉴权 |
| `-retention-days` | `90` | 会话保留天数，`0` 关闭清理 |
| `-task-concurrency` | `2` | 托管派发并发上限 |
| `-hook-path` | `./bin/hub-hook.exe` | CC PreToolUse / Codex PermissionRequest 钩子二进制 |
| `-wecom-aibot-*` | 空 | 企微智能机器人（长连接，推荐） |
| `-wecom-webhook` | 空 | 企微群机器人（旧通道，与 aibot 不要混用） |
| `-feishu-*` | 空 | 飞书自定义机器人 webhook |

### 1.3 平台与已知环境约束

- Windows 首发，macOS/Linux 理论可用未验证。
- 无 C 编译器，纯 Go 依赖（`modernc.org/sqlite`），`go test -race` 不可用。
- Bash `UID` 是只读变量，勿用作变量名。
- Node v25.2.1 `process.exit()` 在 Windows 上会触发 `uv_handle_closing` 断言，QA 脚本请改用 `process.exitCode`。
- 8799 端口若有旧进程占用，先确认并释放（历史曾有 PID 14444 / 25824 / 2436 / 3592 等残留）。

---

## 2. 已验证里程碑与证据

### 2.1 PM 显示层复核（2026-11-10，14 条优化 + 12 张截图）

证据目录：`D:\DevTools\AgentHub\qa\pm-review-20261110\`

- 截图：`01-board.png` / `02-desk.png` / `03-desk-pending.png` / `04-rules-with-data.png` / `05-settings-en.png` / `06-final-restored.png` / `view-{sessions,tasks,stream,rules,terminal,settings}.png`
- 报告：`PM-REVIEW.md`（17 KB，6 节：版本基线 / 17 项真人点击覆盖矩阵 / 已修复明确剔除 / 14 条优化 / 复核残留 / 遗留边界）
- 已剔除的旧账（不再重复计入本轮优化）：移动端导航拥挤 `a20dc39`+`82a3b59`、xterm CDN 本地化 `baef4a0`、`82a3b59`、三语无损还原与原生目录框 `29d3d0a`、派发下拉四选项 m17006。

### 2.2 m17006 派发四选项（opencode / hermes 托管派发）

- 代码：`internal/tasks/tasks.go`（`Enqueue` 放开 4 kind、`resolveAgentBinary` 4 kind、`spawnCommand` 新增 opencode→`opencode acp`、hermes→`hermes-acp`、`streamTask` 路由 `streamACP`、`SetPermissionDecider`）；新建 `internal/tasks/acp_task.go`；`cmd/agenthubd/main.go` 接 `queue.SetPermissionDecider`；`index.html` `#task-kind` 新增 OpenCode/Hermes 选项。
- 验证：`go build`/`go vet`/`gofmt`/`go test -count=1` 全绿；`D:\DevTools\acp-dispatch-e2e.mjs` pass=45 fail=0（codex/opencode/hermes 各 15 断言）；高危 opencode→200 `task_1181417b`。
- 动态复现：隔离 daemon 端口 18899，伪 ACP 伪装 opencode，`POST /api/tasks`→200 `task_94e918b3`，ACP 伪造服务器收到 `initialize` + `session/prompt`（sessionId 回退为 Hub 的 `task-*`，未收 `session/new`），任务落库 `finished` 但会话行保持 `active/turns=0/tokens=0`。**（上述断点已在 2026-11-11 三轮回归中全部修复，见 §3.1；当时"能走到 session/prompt"实为旧 fake 推送的 update 被误消费的侥幸。）**
- 二进制：`bin/agenthubd.exe` 19,440,128 B，已同步至 `install/resources/bin/` 与 `desktop/dist/win-unpacked/resources/bin/`。

### 2.3 企微智能机器人通道（aibot）

- 代码：`internal/server/wecom.go` 重写为 `wecomTransport` 接口；新建 `internal/server/wecom_aibot.go`（`WecomAibotConfig`/`Post`/`Tail`/`call`/`truncateUTF8`）；`cmd/agenthubd/main.go` 新增 `-wecom-aibot-*` 四旗标 + env `AGENTHUB_WECOM_AIBOT_*`；`internal/server/wecom_aibot_test.go` 7 个测试（未跑）。
- 协议：`wss://openws.work.weixin.qq.com`，`aibot_subscribe` → `aibot_send_msg`，按 `headers.req_id` 关联，content ≤20480 字节，限频 30 条/分 1000 条/小时，前置：用户需先在目标会话给机器人发过至少一条消息。
- 真机：`aibot_subscribe` 返回 `errcode=0`（新凭据 `aibVFDkQM0bWbgBhS0xwQF3VrheG80IMo-0` / `nL4J7onuO892csF1txsOxNy9DaiwS78DBF3OdYKD5l5` 有效）；`aibot_send_msg` 持续返回 `846607 aibot send msg frequency limit exceeded`。独立 Node+`ws` 参考实现 `D:\DevTools\wecom-aibot-diff.mjs` 同样失败，**属官方账号维度服务端限频，与代码 / chat_type / Go-Node 实现无关**。
- 旧凭据 `aibsGENHq...` / `v3XB1oKqL9LeKtxDTtOOKxw8b4lkwahkcfhFSlYlQOy` 已失效（`853000 invalid bot_id or secret`）。

### 2.4 其他已交付（历史摘录）

- ACP 适配器：ACP v1 stdio JSON-RPC，`handleFrame` 先判 `Method != ""` 分派（server request 也带 id，不能用 id 存在性判断）；`daemon/acp.go` bridge + `server/acp.go` 4 路由；`agent_kind=dsh` / `origin=managed` / `instance_id=acp:<id>`。
- F7 会话续跑：`POST /api/sessions/{id}/continue`，仅 `origin=managed` 且 `instance_id` 前缀 `acp:`；fire-and-forget + 单飞 + 30min 上限；事件 payload 不含 prompt 文本。
- F9 通知：浏览器手势授权 + `isSecureContext` 门控 + `localStorage tarmac.notifyDesktop/notifyTask` + 摘要 + tag 去重；daemon 侧飞书 webhook（高危/failed/dead/error 立即推、finished 仅 task_completion 开启；30s 静默；HMAC 签名；只回显尾号；绝不读 ArgsDigest/ResultDigest/Content）。
- 连接器：`internal/connector/connector.go` stdio↔daemon 桥，`wsEnvelope` 加 `AgentKind` 做多 connector 隔离。
- 安全：`token_acl_windows.go` icacls 锁 token DACL；~~CheckOrigin 收窄~~（**当时该收窄仅覆盖 terminal-ws 且比较对象写错、/ws 实为全放行**——2026-11-11 已重写为统一 `sameOriginOK`，比较浏览器 Origin，见 §3.1 C1/C2）；`-retention-days` 默认 `"90"`。
- 桌面：Electron `desktop/main.js` `SHARED_DATA_DIR → app.getPath("userData")`；xterm.js 资产已 vendor 到 `internal/server/web/vendor/`（`@xterm/xterm@5.5.0` + `@xterm/addon-fit@0.10.0`，MIT）；`/vendor/{name}` + `/favicon.ico` 端点。

---

## 3. 三轮真实点击回归（2026-11-11 完成）

**方法**：重度 Vibe Coding 用户角色，每轮「真实浏览器点击发现 → 修复 → 真实点击验收」，直到收敛。证据与细节见 `qa/vibe-trial/REGRESSION-REPORT.md`（主报告）、`qa/vibe-trial/VIBE-TRIAL.md`（试用 38 条发现）、`qa/vibe-trial/fake.log`（agent 侧帧取证）、`accept-r*.png`（UI 截图）。

### 3.1 已修复并点击验收（按数据流主线）

| 断点 | 修复内容 | 验收证据 |
| --- | --- | --- |
| ACP 握手卡死（P0-1/A1） | `acp_task.go` 重写为单 reader 泵：显式 `session/new` + id 关联响应 + 60s 握手超时 + 任务级 `-task-timeout-minutes`（默认 60，0 关）看门狗 | FAKE_LOG 三帧全到位；sessionId=ses_* |
| stdout 混读丢帧（P0-2/A2） | json.Decoder + Scanner 混用废除，全生命周期单一 Scanner | unsolicited update 原文进事件流 |
| session/update 不映射（P0-3/A3） | `{sessionId,update}` → Hub 会话 id + content/plan/status 翻译后 Dispatch；tool_call 类 update 折叠为展示文本（行录制走审批路径避免双卡） | 事件流可见 agent 消息文本 |
| 终态不回写（P0-4/A4） | `settleSession`：finished→completed(+1 turn)、failed→failed、cancelled→closed；防重守卫 | 会话行 completed/turns=1/isTerminal |
| 取消不落库+重跑（P0-5/A5） | Cancel 先落 cancelled 再 cancel()；fail() 尊重 cancelled；错误文案 "cancelled by user" | 取消后无重跑、档案留取消语义 |
| 孤儿不同步（P0-6/A6) | recoverOrphans 同步 settleSession + 清扫 %TEMP% 泄漏的 agenthub-task-*-settings.json | 重启后会话 failed/is_terminal=1 |
| 高危规则死信（A7） | gateToolCallLocked 规则匹配提前到高危拦截之前 | allow_always 后重派 pending=0；删规则 pending 回归 |
| 结果/usage 全丢（A8） | mapClaude 映射 result 行；mapCodex 映射 agent_message/turn.completed/error | claude in=12 out=7；codex in=21 out=9 |
| 迟到判决覆盖（A9） | resolvePendingLocked 终态保护（tool_call 与内存投影均不降级） | completed 不再被改回 approved |
| 血缘不落库（A10/P1-8） | continue 服务端预建子会话行（parent 已设）+ `EnqueueInSession` 预设 session id，消除与 tryStart 的建行竞态 | DB 可查 parent_session_id 链 |
| 事件纯内存（P1-5/B5） | 迁移 0006 `ui_events` 表；recordUIEvent 双写；/api/events 读库（?limit/?session） | 重启后事件可回放（118 行） |
| 通知断链（P1-7/B7） | settleSession 派发终态 session/update 帧 → 复用既有 feishu/wecom/push 管道 | failed 路径端到端连通 |
| 导出不完整（P1-3/P1-4/B3/B4） | 迁移 0007 approval_history.session_id；导出改按 session 直查（系统放行不再丢）；bundle 增 tasks/rules/events | bundle 七键齐全 |
| WS 安全（C1/C2/C3） | 统一 `sameOriginOK`（比较浏览器 Origin 而非 r.Host）；`?token=` 收窄到 /ws 与 terminal-ws | 恶意 Origin 403；普通 API ?token= 401；任意端口终端可达 |
| 通知设置假开关（P1-6/B6） | /api/notify/status 返回真实服务端事实；notify/test 按 channel 报错并脱敏（S2/P1-17） | feishu/wecom 各自诚实文案 |
| 幂等/校验（P1-11/B9） | Enqueue：cwd 存在性、二进制可解析、prompt ≤64KB、queued/running 去重 | 空 prompt 400、误触发暂停被立即发现即为例证 |
| Retry 语义（P1-12/B10） | Retry 接受 failed+cancelled；UI cancelled 行加 ↻ | 点击重试成功 |
| 暂停持久化（P1-13/B11） | `data/queue_state.json` 落盘，重启恢复 | 杀进程重启 paused=true |
| MatchFirst kind（P1-15/B13） | 签名加 agentKind；老规则（空 kind）全 kind 兼容 | 单测覆盖 |
| completed→idle（P1-16/B14） | mapUIState 补 completed→finished | 单测覆盖 |
| 规则↔memory 分叉（P1-14/B12） | allow_always 双写 memory；RemoveRule 按前缀语义联动清理 | 删规则后 memory=[] |
| fork 幽灵（P1-9/B8） | fork 行 origin=fork/awaiting_input；continue 对 fork 409 诚实文案 | UI 上屏拒绝文案 |
| 终端错误命令（S17/D8） | 托管会话 409 诚实解释 + UI 灰提示替代必败按钮 | 文案上屏 |
| i18n 缺词（P2-5/D5 及静态标签） | en/ja 补 33+ 点分 key（h1/顶栏/设置页/连接指示全覆盖） | EN/JA 截图验证 |
| 前端语义（P2-1/P2-2/P2-3/P2-7） | 「取消任务」文案；queued 不显旧错误；暂停/恢复 toast；目录弹窗路径跳转输入框 | 逐项点击验证 |

### 3.2 修复过程中引入并被测试抓住的问题

- continue 测试语义更新（会话校验前置）；导出测试补 session_id；fakeTaskQueue 补 EnqueueInSession。
- `data-i18n` 点分 key 与 PAIRS 双轨机制的缺口（33 个静态标签不翻译）——已全量补齐。

### 3.3 残留清单（明知未修）

| 项 | 性质 | 说明 |
| --- | --- | --- |
| 企微 846607 | 外部限制 | 官方服务端限频；窗口过后复测 `{"channel":"wecom","ok":true}` |
| 页面注入完整 token | 架构后续 | WS 鉴权必需；真正解法 = 短时会话 token（待立项） |
| CSP 头 | 硬化后续 | 需先审计内联脚本 |
| 托管任务原生 resume | 功能后续 | 需捕获 agent 侧真实 session id（UI 已诚实禁用） |
| 间歇白屏 | 未复现 | 两次受控尝试均未复现（agent-browser 截图瞬态疑似）；留观 |

## 4. 工程约束（沿用，勿违）

- 无 C 编译器：纯 Go 依赖（modernc.org/sqlite），`go test -race` 不可用。
- Bash `UID` 只读；Node 25 `process.exit()` 在 Windows 触发断言（用 `process.exitCode`）。
- **daemon 启动一律绝对路径**（本轮曾因 cwd 漂移把 11-08 的旧 `agenthubd.exe` 连同生产 data 目录误起在仓库根——已核实零写入；仓库根那个旧 exe 勿删勿用）。
- `agent-browser` 点击后 a11y 树偶发瞬态空白；用 `eval` 驱动页面函数更稳（按钮精确匹配 `^派发 →$`，`includes('派发')` 会误中「暂停派发」——本轮踩过两次）。

## 5. 回归资产

- `qa/vibe-trial/shims-src/`：fakeacp（`[perm]`/`[hold]` prompt 标记 + initialize 后主动推 update）、fake claude（`[highrisk]`/`[hang]`/`[fail]`）、fake codex。
- `qa/vibe-trial/tools/dbg.py`：API/SQLite/事件流三层巡检（注意改用绝对路径——相对路径曾只读触到生产库）。
- `qa/vibe-trial/REGRESSION-REPORT.md` / `VIBE-TRIAL.md`：报告与 38 条原始发现。
- `D:\DevToolscp-dispatch-e2e.mjs`（pass=45）等历史脚本仍可复用。

## 6. 接手动作

1. 工作树保持干净（当前全部已提交）。
2. 发布前跑一轮：`go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`（当前 8/8 包全绿）。
3. 残留清单四项按需立项；企微限频窗口过后先测通知。
4. 生产 data 目录（`D:/DevTools/AgentHub/data`，13 个会话）与本轮试验数据（`qa/vibe-trial/data`）**互不相干**，清理时勿混。
