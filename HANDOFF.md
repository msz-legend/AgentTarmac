# AgentHub Handoff 文档

> 生成时间：2026-11-10 23:20（Asia/Shanghai）
> 版本基线：`v0.2.0-m2`，git HEAD `a01bb01 docs(license): unify repository under MIT, retire Open Core decision H`
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
- 动态复现：隔离 daemon 端口 18899，伪 ACP 伪装 opencode，`POST /api/tasks`→200 `task_94e918b3`，ACP 伪造服务器收到 `initialize` + `session/prompt`（sessionId 回退为 Hub 的 `task-*`，未收 `session/new`），任务落库 `finished` 但会话行保持 `active/turns=0/tokens=0`。
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
- 安全：`token_acl_windows.go` icacls 锁 token DACL；`CheckOrigin` 收窄为 `127.0.0.1:8799`/`localhost:8799` + env `AGENTHUB_WS_ALLOW_ORIGINS`；`-retention-days` 默认 `"90"`。
- 桌面：Electron `desktop/main.js` `SHARED_DATA_DIR → app.getPath("userData")`；xterm.js 资产已 vendor 到 `internal/server/web/vendor/`（`@xterm/xterm@5.5.0` + `@xterm/addon-fit@0.10.0`，MIT）；`/vendor/{name}` + `/favicon.ico` 端点。

---

## 3. 测试工作（Test Work）

以下从**重度 Vibe Coding 用户视角**拆解，核心关注点是「操作步骤之间的串联性 + 数据在步骤间的流转 + 最终数据处理结果是否正确」。每条用例写明：前置条件、操作步骤、预期结果、校验点（API 响应 + SQLite 行 + 前端 DOM）、优先级。

### 3.1 P0：端到端派发链路（数据流必须闭环）

| ID | 用例 | 前置 | 操作步骤 | 预期结果 | 校验点 |
| --- | --- | --- | --- | --- | --- |
| T-P0-01 | 派发 opencode 任务 | daemon 在线，token 可用 | 页面「任务」视图选 OpenCode，填 prompt，点「派发 →」 | 任务入队，状态 `queued`→`started`→`finished` | `POST /api/tasks` 200；`/api/sessions/{id}` status；SQLite `tasks.state=finished`；前端行状态丸变绿 |
| T-P0-02 | ACP `session/new` 必须发出 | 同上 | 派发 opencode 任务，观察伪 ACP 服务器 stdin 日志 | 伪 ACP 收到 `initialize` **后必须收到 `session/new`** | 伪 ACP `FAKE_LOG` 中 `METHOD=session/new` 出现；若无 → P0-A 断点未修 |
| T-P0-03 | ACP `session/prompt` 用正确 sessionId | 同上 | 派发 opencode 任务 | `session/prompt` 的 `sessionId` 应为 ACP server 生成的 `ses_*`/`local-*`，而非 Hub 的 `task-*` | 伪 ACP 日志 `sessionId` 值；若为 `task-*` → P0-A/P0-C 断点未修 |
| T-P0-04 | `session/update` 不被吞掉 | 同上 | 派发任务，伪 ACP 发 2 条 unsolicited `session/update` | Hub 应接收并派发为 `session/update` 事件，前端出现进度 | `/api/events` 有 `session/update` 条目；若为空 → P0-B 断点未修 |
| T-P0-05 | 任务结束会话必须入终态 | 同上 | 派发任务至 `finished` | 会话行 `status` 变为终态、`turns`/`tokens` 非零、`isTerminal=true` | SQLite `sessions` 行；若仍 `active/0/0/false` → P0-D 断点未修 |
| T-P0-06 | 取消 running 任务状态回写 | 有一条 running 任务 | 点「取消」 | 任务状态 `cancelled`，不再自动重跑；前端行不再显示 `running` | SQLite `tasks.state=cancelled`；`/api/tasks` 返回 cancelled；无自动重跑日志 |
| T-P0-07 | 审批四选项闭环 | 高危 prompt 触发 `session/request_permission` | 放行台点「放行一次/该 session 下放行/放行 Always/拒绝」 | 判决落 `approval_history` + `permission_resolved` 事件；agent 收到对应 optionId | `/api/permission-requests/{uid}/decision` 200；SQLite `approvals` 表；前端卡片消失 |
| T-P0-08 | 放行 Always 二次确认 | 同上 | 第一次点「放行 Always」 | toast 提示需要二次确认；第二次点才落决策 | 前端按钮 `.arm` 状态；SQLite `confirm_count=2` |
| T-P0-09 | 规则生效后自动放行 | 已有 `allow_always` 规则 | 再次派发匹配 pattern 的任务 | 无需人工放行，直接执行 | SQLite `rules` 表命中；任务无阻塞直接 `finished` |
| T-P0-10 | 重启孤儿任务标记失败 | daemon 重启，有 running 任务 | 重启 daemon | 原 running 任务改 `failed`，error 含 `daemon restart during run` | SQLite `tasks.state=failed`；前端行显示失败；会话行不应仍保持 active |

### 3.2 P1：数据持久与清理

| ID | 用例 | 前置 | 操作步骤 | 预期结果 | 校验点 |
| --- | --- | --- | --- | --- | --- |
| T-P1-01 | 保留期只删 sessions，tasks 永不清理 | `-retention-days` 非 0 | 等待清扫 ticker（或手工触发） | 过期 sessions 被删；过期 tasks 表**不应**被级联删除 | SQLite `tasks` 表行数不减；若减 → 违背设计（tasks 应独立保留或显式清理） |
| T-P1-02 | 会话导出完整性 | 有包含系统放行的会话 | `GET /api/sessions/{id}/export` | 导出包含所有 permission_requests（含 `permission_request_id IS NULL` 的系统放行） | 导出 JSON/MD；若系统放行缺失 → 导出不完整 |
| T-P1-03 | 导出包含 tasks / notifications / rules / 事件 | 同上 | 同上 | 导出 bundle 除 Session/ToolCalls/PermissionRequests/Approvals 外，还含 tasks、notifications、rules、事件 | 导出 JSON 字段完整性 |
| T-P1-04 | 通知设置持久化 | 浏览器开关 | 切「桌面通知」/「任务完成通知」开关 | 开关状态跨刷新/重启保持；`/api/notify/status` 反映真实状态 | 前端 localStorage；API 响应；若硬编码 `true` 无法变更 → 设置是假的 |
| T-P1-05 | 暂停派发持久化 | 点「暂停派发」 | 刷新页面 | 暂停状态应保持 | SQLite 或文件持久化；若刷新后恢复 → P1 暂停不落库 |

### 3.3 P2：安全与边界

| ID | 用例 | 前置 | 操作步骤 | 预期结果 | 校验点 |
| --- | --- | --- | --- | --- | --- |
| T-P2-01 | token 不应完整注入 HTML | 打开 `http://127.0.0.1:8799/` | 查看页面源码 | token 应被掩码或不注入完整值 | `boot.token` 字段；若完整 token 明文 → 安全缺陷 |
| T-P2-02 | `/ws`  origin 收窄 | 浏览器开发者工具 | 连接 WS | `CheckOrigin` 应仅允许 `127.0.0.1:8799`/`localhost:8799` | 主 WS upgrader；若 `return true` → 跨站 WebSocket 劫持风险 |
| T-P2-03 | `?token=` 不应进 URL | 任务 | 带 `?token=` 请求 API | 服务器应拒绝或忽略查询参数 token | 日志 / Referer 中不应出现 token |

### 3.4 执行方式

- 自动化：`D:\DevTools\qa\scripts\` 下 Python 脚本（`qa_cli_integrate.py`、`qa_envelope.py`、`qa_notify_matrix.py`、`qa_perf_gate.py`、`qa_queue_e2e.py`、`qa_recovery.py`、`qa_sec_endpoints.py`、`common.py`）可直接复用或改写。
- 半自动：伪 ACP `D:\DevTools\AgentHub\qa\dataflow-repro\fakeacp.exe`（`FAKE_LOG` 记录每帧、`FAKE_MODE=normal/hold`）用于隔离 daemon 动态复现 P0-A/P0-B/P0-C。
- 人工：页面逐视图点击（见上一轮 `PM-REVIEW.md` 的 17 项覆盖矩阵）。

---

## 4. 后续优化改进（共 38 条，去重合并）

每条带：级别、问题、根因（路径+行号）、修复建议、验证边界。**不记录评分**（用户硬约束 m17580）。

### 4.1 P0（数据流断点，必须先修）

| # | 问题 | 根因 | 修复建议 | 验证边界 |
| --- | --- | --- | --- | --- |
| P0-1 | `session/new` 请求从未发出 | `internal/tasks/acp_task.go:84-99` 只 encode `initialize`，`:101-114` 注释「2. session/new」后零 encode，`:116-127` 直接发 `session/prompt` | 在 `streamACP` 中显式 `enc.Encode({"method":"session/new","params":{"cwd":t.CWD,"mcpServers":[]}})`，读响应取 `sessionId` | 伪 ACP `FAKE_LOG` 出现 `METHOD=session/new`；`session/prompt` 的 `sessionId` 为 `ses_*`/`local-*` |
| P0-2 | 混用 reader 丢数据 | `:65` `json.NewDecoder(stdout)` 预读缓冲与 `:129` `bufio.NewScanner(stdout)` 共读同一 stdout 管道 | 统一用 `bufio.Scanner` 逐行处理，或用单一 `json.Decoder` 不再混用 Scanner | 伪 ACP 发 2 条 `session/update` 后 `/api/events` 有对应条目，不再静默丢帧 |
| P0-3 | progress 全部丢弃 | `:166-169` 原样 `q.Dispatch(..., "session/update", msg.Params)`，ACP params 是 `{sessionId, update:{...}}`，`adapter.go:1227-1235` 只解顶层字段 → 全空；且 `sessionId` 是 agent id，Hub 只有 `task-*` 行 → `unknown session` | `handleACPLine` 解析 `{sessionId, update}`，按 `sessionId` 映射到 Hub task 行，或在 `handleSessionUpdate` 中按 `payload.SessionID` 先按 agent session 索引再回填 task | `/api/events` 有 `session/update` 带 `session_id`；前端进度条/文本有变化 |
| P0-4 | 最终结果不回写会话 | `tasks.go:317-321` 只 `TaskFinished + UpdateTask + emit("finished")`，**从不 UpdateSession** | 任务结束后调用 `UpdateSession` 设 `isTerminal=true`、`turns`、`tokens`、`status` 终态 | SQLite `sessions` 行 `isTerminal=true`、`turns>0`；导出档案会话状态正确 |
| P0-5 | 取消 running 任务不落库 + 自动重试 | `tasks.go:197-203` running 分支只 `cancel()` 不落库；`:303-316` `fresh.State==TaskCancelled` 永不成立；`:327-345` `Retries<MaxRetries` 改回 `queued` 重跑 | Cancel 时 `t.State=TaskCancelled` + `UpdateTask`；重启守护 goroutine 跳过 cancelled 任务；或 `fail()` 不自动重跑 cancelled | 点取消后 SQLite `state=cancelled`；前端行显示 cancelled；无自动重跑 |
| P0-6 | 重启态与会话不一致 | `recoverOrphans()` `tasks.go:92-105` 只把 running→failed，**不动 sessions 行** | 孤儿恢复时同步把关联 session 设 `is_closed=true`/`is_terminal=true` | 重启后 `sessions` 行不再保持 `active` |

### 4.2 P1（持久化与一致性）

| # | 问题 | 根因 | 修复建议 | 验证边界 |
| --- | --- | --- | --- | --- |
| P1-1 | `UpdateSession` 不持久化关键列 | `store.go:108-116` UPDATE 只含 status/turns/tokens/last_error/is_terminal/is_closed/title/updated_at | UPDATE 加入 `cwd/agent_kind/origin/parent_session_id` | 重试后 `existing.CWD` 落库不丢失；`sessions` 行可查询这些列 |
| P1-2 | retention 只删 sessions | `store.go:502-512` 仅 `DELETE FROM sessions WHERE updated_at < ?`；tasks/rules 无 FK | 加 `DELETE FROM tasks WHERE session_id IN (...)` 或独立 tasks 保留策略；rules 加 `session_id` FK 级联 | 清扫后 `tasks`/`rules` 不过期残留 |
| P1-3 | 会话导出排除系统放行 | `store.go:596-645` `JOIN permission_requests` 导致 `permission_request_id IS NULL` 的系统放行全部排除 | 改为 `LEFT JOIN` 或分两段查询，把系统放行也纳入 | 导出包含所有 permission_requests，无遗漏 |
| P1-4 | 导出不含 tasks/notifications/rules/事件 | 同上，bundle 只含 Session/ToolCalls/PermissionRequests/Approvals/ExportedAt | 扩展 `ExportSession` 聚合 tasks、notifications、rules、事件 | 导出 JSON 字段完整 |
| P1-5 | 事件纯内存，重启即失 | `bridge.go:80-83` `uiEventRingCap=500`，`recordUIEvent` 纯内存 | 事件落 SQLite（新表 `ui_events`），`GET /api/events` 优先读库 | 重启后 `/api/events` 仍有历史事件 |
| P1-6 | 通知设置硬编码 | `console.go:150-162` `"desktop":true,"task_completion":true`，不读浏览器权限/localStorage | 读取浏览器授权状态 + localStorage，跨设备同步 | 开关可变更，`/api/notify/status` 反映真实值 |
| P1-7 | 任务完成通知链路未接通 | `Bridge.TaskEvent` `bridge.go:464-478` 只 recordUIEvent，**不触发 notifier** | 任务 `finished/failed` 时触发 notifier（按用户开关） | 任务完成后桌面/飞书/企微收到通知 |
| P1-8 | 继续会话血缘丢失 | `console.go:457-472` `parent_session_id` 不写库；新 session 行异步创建，返回 id 当下不存在 | 重派发时同步写 `parent_session_id` 到 sessions 表 | 查询可追溯会话血缘 |
| P1-9 | fork 是死路 | `console.go:191-215` 只 `CreateSession + RecordFork`，无 agent 进程/connector 接管 | fork 后启动对应 agent 进程或标记为待接入，避免 ghost session | fork 后会话可继续派发，不再永久 ghost |
| P1-10 | 判决事件缺 session 关联 | `bridge.go:271-293` `recordPermissionResolved` UIEvent 未设 `SessionID` | 构造 UIEvent 时填入 `SessionID` | 事件流可按 session 过滤判决记录 |
| P1-11 | 派发无前置校验/幂等 | `tasks.go:140-161` 只校验 kind/cwd/prompt 非空；无 cwd 存在性、二进制存在性、idempotency key、prompt 长度上限 | Enqueue 前校验 `cwd` 是否存在、`resolveAgentBinary` 是否找到二进制；加 `idempotency_key`；prompt 长度上限 | 重复 dispatch 同 idempotency key 只生成一个任务；cwd 不存在直接 400 |
| P1-12 | `Retry` 只接受 failed | `tasks.go:164-180` `cancelled` 任务永远无法重启 | `Retry` 接受 `failed` 和 `cancelled` | cancelled 任务可重新派发 |
| P1-13 | 暂停状态不持久 | `tasks.go:212-231` `paused` 纯内存 bool | Pause/Resume 落 SQLite 或配置文件 | 重启后暂停状态保持 |
| P1-14 | 规则/判决语义不联动 memory | `adapter.go:1045-1067` `allow_session/allow_always` 只写 rules，`else` 分支才写 memory；`RemoveRule` 不联动 `approval_memory` | 判决后同步更新 `approval_memory`；删除规则时联动清理 memory | 规则删除后 memory 不残留；跨 Agent 不串味 |
| P1-15 | `rules.MatchFirst` 不匹配 `agent_kind` | `rules.go:99-140` 注释 `:37` 明示 audit-only | `MatchFirst` 加入 `agent_kind` 匹配 | 同 pattern 不同 Agent 规则隔离 |
| P1-16 | 状态映射漏终态 | `bridge.go:199-215` `status:completed` 但无 `isComplete` 时落 `idle` | 增加 `completed`→`finished` 映射 | 任务 `completed` 后 UI 显示 finished |
| P1-17 | 原始错误直抛 + 通道错配 | `console.go:167-185` 完整错误串入 502；`testNotify()` 硬编码「飞书测试消息已发送」 | 错误脱敏（只保留 errcode，去掉 hint/IP）；测试 toast 按实际 channel 本地化 | 502 不含 hint/出口 IP；toast 显示「企微测试消息已发送」 |
| P1-18 | 会话视图 prompt 截断 U+FFFD | `index.html` 长 prompt 60 字硬截断 | 加展开/复制；不截断或截断后可完整查看 | 长 prompt 可完整阅读和复制 |
| P1-19 | 继续会话无差别开放 | 9 张卡全带「继续此会话」，输入完才吃 409 | 附加态探测（仅 attached claude_code/codex 可续跑），其他显示 409 提示 | opencode/hermes 继续 → 409 `不支持原生续跑` |

### 4.3 P2（体验与安全）

| # | 问题 | 根因 | 修复建议 | 验证边界 |
| --- | --- | --- | --- | --- |
| P2-1 | 「拒绝」名不副实 | `index.html:1062` `data-action="taskCancel"` 文案 `t("拒绝")` | 改文案为「取消任务」或拆分「取消」与「拒绝审批」 | 前端文案与行为一致 |
| P2-2 | 状态/错误文本矛盾 | `tasks.go:332` `q.fail` 先写 error 后 `TaskQueued`；`index.html:1068` 无条件拼 `task.error` | 状态 `queued` 时不显示错误文本；或错误仅在 `failed` 态展示 | 重试态行不显示旧错误 |
| P2-3 | 暂停/恢复无反馈 | `index.html:1778-1783` 只 `S.tasksPaused + renderTasks()`，无 toast | 加 toast「已暂停派发」/「已恢复派发」 | 点击后 toast 出现 |
| P2-4 | 查看会话不定位 | `setView(v)` 只切 view，无滚动/高亮 | 点「查看会话」后滚动到目标卡并高亮 | 目标卡 rectTop 可见，`highlighted=true` |
| P2-5 | 6 个 h1 缺 en/ja 词条 | `DICT.en` 仅 2 个 key，其余 6 个缺 | 补全 `DICT.en` / `DICT.ja` 的 8 个 h1 key | 切英文/日文后标题全部翻译 |
| P2-6 | 运行时数据不本地化 | `taskErrorsEn` 等运行时字符串不进词典 | 把运行时错误/状态文本本地化 | 切英文后错误文本翻译 |
| P2-7 | 目录浏览器无搜索/输入 | `fsModal` 42 个按钮 0 input，深路径只能逐级点 | 加路径输入框 + 搜索过滤 | 输入路径可直接跳转 |
| P2-8 | 终端能力脱节 | 终端页「仅 claude_code / codex 支持」但无提示说明原因 | 加不可用原因提示，或扩展到 opencode/hermes | 用户明白终端不可用原因 |
| P2-9 | token 明文展示 | `index.html:1841` `TOKEN.slice(0,10)+"…"+TOKEN.slice(-6)` | 掩码更严格，或提供「显示/隐藏」开关 | token 首尾不直接明文 |
| P2-10 | 企微 846607 无退避降级 | 官方服务端限频，代码无降级策略 | 加指数退避 + 剩余时间提示 + 降级至日志/本地队列 | 限频期间不反复报 502，用户可见预计恢复时间 |
| P2-11 | 移动端导航重构（历史已修，保留作为参考） | `baef4a0`/`82a3b59` 已修 | —— | ——（已剔除，勿重复计） |

> 已剔除（勿重复计入本轮）：移动端 8 tab 拥挤 `a20dc39`、`82a3b59`；xterm CDN 不可达 `baef4a0`；三语无损还原 `29d3d0a`；原生目录框 `29d3d0a`；派发下拉四选项 m17006。

---

## 5. 测试回归事项（Regression）

### 5.1 回归测试集

| 回归项 | 验证方式 | 自动/人工 | 触发时机 |
| --- | --- | --- | --- |
| R-01 派发→ACP 握手→session/new→session/prompt | 伪 ACP `fakeacp.exe` + 隔离 daemon 18899 | 自动 | 每次 commit |
| R-02 审批四选项闭环 | `three-client-contract.mjs`（pass=45 fail=0 可复用） | 自动 | 每次 commit |
| R-03 取消/重试状态回写 | 页面点击 + SQLite 检查 | 半自动 | 每次 commit |
| R-04 规则生效自动放行 | `review-regression.mjs` 可复用 | 自动 | 每次 commit |
| R-05 重启孤儿任务 | 杀 daemon 重启，检查任务/会话状态 | 半自动 | 发布前 |
| R-06 保留期清理 | 设 `-retention-days` 短值，检查 tables | 自动 | 发布前 |
| R-07 会话导出完整性 | `GET /api/sessions/{id}/export` 字段核对 | 自动 | 发布前 |
| R-08 通知链路（飞书/企微/桌面） | `qa_notify_matrix.py` + `/api/notify/test` | 半自动 | 发布前 |
| R-09 三语切换 | 页面 `[data-lang-set]` en→ja→zh-CN | 半自动 | 每次 commit |
| R-10 安全基线（token/WS origin/?token=） | `qa_sec_endpoints.py` | 自动 | 每次 commit |

### 5.2 回归脚本资产

- `D:\DevTools\qa\scripts\common.py`：公共工具。
- `D:\DevTools\qa\scripts\qa_cli_integrate.py`：CLI 集成。
- `D:\DevTools\qa\scripts\qa_envelope.py`：信封契约。
- `D:\DevTools\qa\scripts\qa_notify_matrix.py`：通知矩阵。
- `D:\DevTools\qa\scripts\qa_perf_gate.py`：性能闸门（审批 p95 ≤1s）。
- `D:\DevTools\qa\scripts\qa_queue_e2e.py`：队列 E2E。
- `D:\DevTools\qa\scripts\qa_recovery.py`：恢复/重启。
- `D:\DevTools\qa\scripts\qa_sec_endpoints.py`：安全端点。
- `D:\DevTools\qa\dataflow-repro\fakeacp.exe`：确定性伪 ACP（`FAKE_LOG` / `FAKE_MODE`）。
- `D:\DevTools\acp-dispatch-e2e.mjs`：opencode/hermes dispatch 契约。
- `D:\DevTools\wecom-aibot-diff.mjs`：企微协议差分参考。

### 5.3 已知残留（不自动清理）

- 4 条 `pm-review-live-*` 会话/权限请求/事件保留未删（「清空全部记录」是 m15556 独立指令，本轮未重新授权；归档先例 `D:\DevTools\_archive\agenttarmac-20261109-0730\`）。
- m17006 四文件未 commit（`M cmd/agenthubd/main.go`、`M internal/server/web/index.html`、`M internal/tasks/tasks.go`、`?? internal/tasks/acp_task.go`）。

---

## 6. 遗留开放目标

| 目标 | 状态 | 备注 |
| --- | --- | --- |
| 企微 846607 服务端限频 | 非我方可控 | 独立参考实现同样失败；窗口过后验证 `{"channel":"wecom","ok":true}` |
| Codex 受限沙箱复跑 | 待用户操作 | 需管理员 PowerShell 运行 `D:\DevTools\codex-elevated-verify.mjs`（脚本已修 4 处） |
| m17006 四文件 commit | 待执行 | build/vet/test 已全绿 |
| M3 按 MIT 整理 | 用户暂缓 | `a01bb01` 已完成纯 MIT 统一，M3 文档站/发布流程/社区运营后续再做 |

---

## 7. 接手动作（建议顺序）

1. **先 commit m17006 四文件**；清理工作树，建立干净基线。
2. **修复 P0-1 ~ P0-6**（数据流断点）：
   - `acp_task.go` 补 `session/new` encode；统一 reader（P0-A/P0-B）。
   - `acp_task.go` 解析 `{sessionId, update}`，映射到 Hub task 行（P0-C）。
   - `tasks.go` 任务结束后 `UpdateSession` 入终态（P0-D）。
   - `tasks.go` Cancel 落库 + 跳过 cancelled 自动重跑（P0-E）。
   - `recoverOrphans()` 同步关闭 sessions（P0-F）。
   - 每条修复后用 `fakeacp.exe` + 隔离 daemon 动态复现，保留 `FAKE_LOG` 作为证据。
3. **修复 P1 持久化与一致性**（P1-1 ~ P1-18），补 SQLite 回写、导出扩展、事件持久化、通知链路。
4. **修复 P2 体验与安全**（P2-1 ~ P2-10），补 i18n 词条、token 掩码、WS origin 收窄、目录搜索、企微降级。
5. **补回归自动化脚本**：把 R-01 ~ R-10 纳入 CI，每次 commit 跑 R-01/R-02/R-09/R-10，发布前跑全集。
6. **写回归报告**：每次回归记录通过/失败项，保留 `qa/` 证据目录。

---

## 8. 交付检查清单（接手前确认）

- [ ] `git status` 干净，m17006 四文件已 commit
- [ ] `go build ./...` / `go vet ./...` / `gofmt -l .` / `go test ./... -count=1` 全绿
- [ ] P0-1 ~ P0-6 各有一条 `FAKE_LOG` 动态复现证据
- [ ] HANDOFF.md 与当前代码基线一致（无已修未记录项）
- [ ] `qa/pm-review-20261110/` 12 张截图 + `PM-REVIEW.md` 完整
- [ ] `qa/dataflow-repro/` `fakeacp.exe` + `inspect_db.py` 可复用
- [ ] 4 条 `pm-review-live-*` 残留已知晓，不误删

---

**接手联系人**：Team Lead `lead`。文档中的路径均为 Windows 绝对路径，如需在其他环境复用请按需替换盘符和分隔符。