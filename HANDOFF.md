# AgentHub Handoff 文档

> 生成时间：2026-11-07 15:00（Asia/Shanghai）；**2026-11-07 QA R2 更新**
> 当前阶段：**PRD_v0.4 M1 + M1.5 + M2 主体全量交付**——M1.5（Service Worker 推送 + 静默期）、F7 原生恢复（CC --resume / Codex exec resume）、D6 Go 版 hub-hook、F6 统一操作面（任务队列+看板+全托管派发）、D8 自托管中继、企微 webhook
> 编译状态：`go build ./...` / `go vet ./...` / `gofmt` 全净；`go test ./...` 全绿（7 个包 80+ 用例）；PRD M1 实机验收 19/19 + M2 批次实机验收 10/10 + **QA R2 电池 175 项 ×3 = 525 检查 0 FAIL**

---

## -2. QA R2 首轮（2026-11-07 下午，测试方案落地 + Android 实机）

依据 `docs/TEST_PLAN.md`（v1.0，~160 TC）建成 `qa/` 测试环境并执行 R1+R2+R4：

- **环境**：四件套重建于 `bin/`；`qa/` 含假 ACP agent、claude/codex 托管 shim（argv 记账 + settings 拷贝）、mock webhook（18900）。旧残留 daemon（PID 14444）已清。防火墙规则「AgentHub QA 18799 (LAN only)」已加（UAC 提权，LocalSubnet only）。
- **PC 全电池**（6 脚本，修复后双跑）：sec 95/95、envelope 14/14、queue 14/14、notify 16/16、perf 26/26（**审批 p95=24ms，PRD ≤1s**）、recovery 10/10 —— **175 项 ×2 = 350 检查 0 FAIL**；`go test ./...` 7 包全绿、gofmt 净。
- **发现并修复三缺陷**：
  - **ENV-1**（S3）：事件环缺 `agent.kind` 戳记 → bridge.SetStore 会话反查 + 显式 agentKind（adapter/bridge/tasks/connector 五处同步）。
  - **ENV-2**（P1，手机实测发现）：审批卡/复验框只显示 pattern，违反 D18「强制完整命令」→ `wsToEvent` 改用帧内 `mainArg`（命令原文）。
  - **ENV-3**（P2）：控制台待办只从内存环重建，重启前 pending（DB 存活）放行台不可见 → boot 从 `/api/permission-requests?outcome=pending` 合并。
- **Android 实机**（华为 Android 10，无 GMS，USB adb；AP 隔离 → `adb reverse` USB 隧道）：控制台渲染/WS 实时/**TC-UI-010 手机高危审批全流程**（复验框完整命令→键入 note→确认拒绝→审计落库）/TC-UI-006 三语/F4 假活标记/F8 三入口 全部通过。**SW 推送按产品决策改道企微 webhook**（华为浏览器对 localhost 不给 isSecureContext 且无 Push 服务——前提不成立，详见 qa/README §Android）；GMS 设备可恢复 SW 能力。
- 测试装置噪声：Windows 回环阵发 RST → 脚本退避重试；空列表序列化 null → 脚本防御。
- **待人工项**：真实企微 webhook URL（接入后移动通知即达真群）；真实 claude CLI 未装（shim+单测已覆盖构造路径）。


---

## -1. PRD 交付对照（2026-11-07，PRD_v0.4 @ E:\找工作\...\AgentHub\）

| PRD 条目 | 状态 | 实现 |
|------|------|------|
| **F3 审批四选项**（D16） | ✅ | 放行一次/该 session 下放行/放行 Always/拒绝（复验框）；`allow_session/always → 规则表`，`reject_always → 审批记忆`；永久判决二次确认（决策B，强于 PRD） |
| **F3 规则引擎 v1**（Q-F4，M1 即需） | ✅ | `internal/rules`：`<tool>(<arg_pattern>)` 字面 glob、最具体优先、session 作用域随会话失效；主参数=shell 命令/文件路径；决策派生 pattern=精确主参数（E: 参考实现同语义） |
| **F3 hook 通道**（CC PreToolUse / Codex PermissionRequest） | ✅ | `POST /api/hook/intake`（阻塞判决，2min fail-closed 超时 unavailable/timeout）+ `hooks/hub-hook.mjs`、`hooks/codex-hook.mjs`（AGENTHUB_URL/TOKEN/TIMEOUT_MS，低危本地短路）；会话自动注册 `origin=attached` |
| **F3 验收**（≤1s 全链路） | ✅ 实测 | policy 命中 <1s（本机毫秒级）；四选项端到端可审计（rule_scope/note 落 approval_history + permission_resolved 事件） |
| **F4 状态告警** | ✅ | 7 态映射（UI 稿 §1.4）+ 假活检测（running/waiting >120s 无活动 → ⚠疑似失联，不擅自 kill）；失败→飞书推送 |
| **F5 多端一致** | ✅（M1 部分） | 响应式（≥1180/760 断点）+ 局域网 `-addr`；公网中继 = M2（D8） |
| **F7 多会话并行+复用**（D14） | ✅ | sessions 表 origin(`attached\|managed`)/parent_session_id/title（首条 prompt）；「继续此会话」经命令总线；「分叉继续」显式按钮 → `/api/sessions/{id}/fork`（Hub 级 lineage；CC `--resume`/Codex resume 与 E: 同步推迟 M2） |
| **F8 会话视图+终端跳转**（D15） | ✅ | Hub 内只读会话详情（头部 kind/cwd/origin/tokens + 按 session 过滤事件流）；`PlatformProvider.OpenSessionTerminal`（Windows wt.exe→cmd start 回退，darwin osascript，linux 多终端回退）+ `POST /api/sessions/{id}/terminal`（仅 claude_code/codex 有原生 resume，ACP 系 409 提示 Hub 内查看）；放行条「查看会话」跳转入口 |
| **F9 通知**（D17） | ✅（M1+M1.5 范围） | 桌面 Web Notification（手势授权、isSecureContext 门控、仅摘要）+ 飞书 webhook（daemon 侧、加签、**仅高危待放行**+**会话失败**立即推、24h tail_hash 去重）；企微 = M2 |
| **§5.6 数据保留**（D9） | ✅ | **默认永久保留**（-retention-days 默认 0），可选滚动窗口；purge 留痕（system 通知行）；导出 JSON/**Markdown** |
| **§6 事件模型 v0.2**（冻结） | ✅ | 信封 v=2/event_id(uuid)/ts RFC3339(ms UTC)/agent{kind,version,instance_id}/risk/rule_supported；status 携带 cwd/origin/parent（C5）；tool_result ok；task 事件（命令总线）；outcome 词表不变（allowed_once/rejected/cancelled/unavailable）+ rule_scope/decided_by(policy) |
| **M2 项** | ⏳ 按 PRD 排队 | 自托管中继、F6 统一操作面（任务队列看板）、xterm.js 内嵌终端、企微 webhook、macOS 正式支持、CC `--resume`/Codex CLI resume |
| **M3 项** | ⏳ | 文档站/发布流程/社区运营；商标检索（PRD §14.5 遗留） |

## -0.5 M1.5 + M2 交付（2026-11-07 第二轮）

| PRD 条目 | 状态 | 实现 |
|------|------|------|
| **M1.5 Service Worker 通知**（F9 验收：关标签页仍可达） | ✅ | `internal/push`（VAPID 密钥持久化 + 订阅注册表 + Web Push 投递 + 410 自动退订）+ `/sw.js`（push/notificationclick）+ `POST /api/push/subscribe` + 设置页推送行 + 启用通知即注册订阅；高危审批/会话失败触达 |
| **M1.5 通知静默期** | ✅ | 30s 同类合并（D17：perm/failed 按 session 抑制），飞书/企微共用 |
| **F7 原生恢复** | ✅ | `internal/runner`：`claude --resume <id> --print --output-format stream-json --verbose` / `codex exec resume <id> --json`，cwd 强制携带；stream-json/JSONL → tool_call/tool_result/status 帧；单飞保护 + fire-and-forget；`POST /api/sessions/{id}/continue` 统一 managed（命令总线）/attached（原生）两条路 |
| **D6 Go 版 hub-hook** | ✅ | `cmd/hub-hook`：stdin JSON → intake → allow(exit 0)/deny(exit 2, fail-closed)；`install/uninstall [--codex]` 幂等合并 CC settings.json / Codex hooks.json（只动自己的条目）；argv 顺序兼容 |
| **F6 统一操作面主体**（D13） | ✅ | `internal/tasks`：tasks 表（迁移 0005）+ 队列（并发可配 `-task-concurrency`）+ 托管 spawn（CC `-p --settings` 注入 hub-hook → **审批进同一收件箱**；Codex exec --json）+ 失败自动重试 + 手动 retry/cancel + **暂停/恢复派发**（暂停续跑）+ 重启孤儿任务标记失败；task 生命周期事件（queued/started/finished/failed/cancelled）进环与 WS |
| **F6 看板 UI** | ✅ | 控制台「任务」视图：派发表单（Agent/工作目录/描述）+ 任务台账（状态丸/错误/会话跳转）+ 暂停/恢复 + 重试/取消；任务 WS 事件实时刷新 |
| **D8 自托管中继** | ✅ | `cmd/agenthub-relay`（TLS，`-insecure` 仅开发）+ `internal/relay`：Hub **出站** WS 连接（无需公网 IP）、浏览器请求管道转发、**不落地**（零存储）、双向 token（relay token + Hub Bearer 透传）、指数退避重连；`agenthubd -relay wss://… -relay-hub-id …` 启用（默认关） |
| **M2 企微 webhook** | ✅ | `internal/server/wecom.go`（企业微信群机器人 text 消息）+ `-wecom-webhook`；与飞书同策略（高危/失败、静默期、去重） |
| **M2 xterm.js 内嵌终端** | ⛔ 环境阻塞 | ConPTY 依赖（conpty v0.1.4）已就位；**xterm.js 资产在本环境无法下载**（CDN SSL 连接被拒）——非设计缺口；F8 系统终端跳转已交付替代路径。有网环境补齐前端资源后即可接上（后端 PTY 端点待写） |
| **M2 macOS 正式支持** | ⚠️ 代码就绪 | darwin platform provider（osascript 终端跳转）在库；真机验证无法在本 Windows 环境执行 |

**M2 批次实机验收（2026-11-07，python 10 项脚本）**：sw.js 服务 / VAPID 密钥 / push 订阅注册 / 任务派发→重试→终态 / managed session 链接 / task 生命周期事件 / **暂停阻塞派发** / continue 端点契约 — **10/10 PASS**；中继 e2e（Go 测试：浏览器→中继→Hub→回，含 401 透传与离线 502）✓；任务队列 e2e（假 agent 全生命周期 + 暂停阻塞）✓。

### Go 包清单（7 个）

`acpclient` / `connector` / `relay` / `rules` / `runner` / `server` / `tasks`

---

**PRD 实机验收（2026-11-07，python 19 项脚本）**：healthz / intake 阻塞判决 allowed_once·always / **policy auto-allow <1s** / 信封 v2 全字段 / command 原文 / hook deny+note 落审计 / fork lineage / title=首条 prompt / rules 列表 / **Markdown 导出** / retention 默认 0 / **真实 wt.exe 终端拉起** — **19/19 PASS**。浏览器复验：会话卡三按钮（继续/分叉/终端）+ 详情过滤事件流 + fork 行显示。

---

## 0. UI 稿适配（2026-11-07，本轮）

**依据**：`E:\找工作\毛世泽+后端开发工程师\04_技术学习\AgentHub\design\UI_UX_v0.1.md`（内容版本 v0.4，§11/§12 为实现口径；其「唯一实现依据」为同项目 `internal/web/index.html` 参考实现，已通过 70/70 CDP）。

**落地方式**：以参考实现为蓝本 1:1 移植布局/视觉/交互，数据层适配本仓库 API。旧三件套（index.html/app.js/style.css）删除，现为**单文件** `internal/server/web/index.html`（~1300 行，零依赖零构建）。

| 采纳项 | 实现 |
|------|------|
| 暖色终端色板（Q-U1） | `--bg:#1c1714 / --surface:#262019 / --amber:#e0a458 / --green:#8fb573 / --red:#d4736a / --blue / --violet` 全套 token |
| 默认落地=全局态势大屏（Q-U2） | 4 tile（在线 Agent/活跃会话/待放行/事件总数）+ Agent roster（全部/仅活跃）+ 待放行请求 + 最近事件 |
| 三栏结构 | 顶栏(品牌+daemon/Agent/待放行/会话 状态) + rail 62px 六导航(态势/放行台/会话/事件流/规则/设置，含琥珀徽标) + statusline（kinds · 待放行 N · 会话 M · 连接态 · 版本）；<760px rail 变底部 tab |
| 放行台四选项 | `1 放行一次 / 2 该 session 下放行 / 3 放行 Always / 4 拒绝(复验)`，各带 decision 词汇说明；选中卡琥珀描边+左风险色条，其余 hairline |
| 拒绝复验框（§1.5） | 只读命令块 + 非必填说明（写入审计 note）+ 取消/确认；打开不改队列 |
| 二次确认（决策B × 四选项3） | 首次点击 → 服务端 `requiresConfirmation` → 按钮变紫「再次点击确认」(.arm)；二次 `confirm:true` 落决策 |
| 会话视图（§2.1） | 卡=会话（kind·cwd·回合+状态丸+继续此会话）；继续走 `/api/agent-requests` 命令总线 |
| 规则视图 | 审批记忆呈现为 `tool(pattern) · kind · allow/deny · always` + 删除（`/api/rules` 别名 → approval_memory） |
| 键盘 | `1/2/3/4` 决策、`j/k` 选择、`Esc` 关浮层（输入框聚焦让位）；输入框内 Esc 仅 blur |
| 通知（决策F） | 设置页三行（桌面通知/任务完成通知/飞书 webhook 掩码+发送测试）+ `tarmac.notifyDesktop/notifyTask` localStorage；浏览器通知仅摘要不含命令正文 |
| 三语（§9） | ROWS 词典 + TreeWalker localize，中/EN/JA，`tarmac.lang` 持久化 + 浏览器语言优先；机器值不翻译 |
| token 注入（§11.2） | `window.__TARMAC_BOOT__` 由 daemon serve 时注入（token/version/WS addr），操作员免粘贴 |
| WS 重连 | 指数退避 500ms→8s；WS 消息在前端转换为规格事件形状（与事件环 1:1） |

**配套后端**（本轮新增）：
- 迁移 `0002_session_agent_kind`（sessions.agent_kind，适配器写入）→ 会话卡显示 Agent 类型
- 事件环形缓冲（内存 500 条，规格事件形状：permission_request/permission_resolved/status/tool_call/tool_result/task/error）+ `GET /api/events`（放行台重载后从环重建待办）
- `GET /api/agents`、`GET|DELETE /api/rules`（记忆别名）、`GET /api/notify/status`（feishu/feishu_tail/retention_days/version）、`POST /api/notify/test`（501 未配置）
- `PermissionResolution.Note` 回填（permission_resolved 事件携带复验说明）

**浏览器实测（IAB 1440×900，截图证据）**：态势大屏四 tile 真实数据 ✓；放行台四选项+风险徽标（high 红/medium 蓝）✓；按 4 → 复验框打开且队列不变 → 确认拒绝 → 队列 2→1 + `permission_resolved{outcome:rejected, rule_scope:none}` ✓；allowAlways 武装态 → 二次点击 → 记忆落库（confirm_count=2）→ **规则视图即时出现** `Bash(cargo build) allow·always` ✓；会话卡 ✓；设置页（token 掩码/90 days/版本/飞书未配置/桌面通知 Blocked 降级）✓；中→EN 全 chrome 翻译 ✓；BOOT 注入 ✓；WS 已连接 ✓。

**与参考实现的有意差异**：`decide()` 成功且为 session/always 判决时**即时刷新规则视图**（参考实现需整页重载）；事件环为 daemon 生命周期内存态（规格允许：持久审计在 SQLite）。

---

## 1. 当前开发路径（全路径）

**项目根目录**：`D:\DevTools\AgentHub`

**Go 模块**：`github.com/agenttarmac/agenttarmac`（go 1.26；本机 Go 1.27.0）

| 模块 | 路径 | 职责 |
|------|------|------|
| **适配器** | `internal/acpclient/adapter.go` | `BaseAdapter` 通用底座（~990 行）：5 个 handle* + 入站 `session/request_permission` + 审批流水线 + 状态机 + 重启恢复 |
| | `internal/acpclient/claudecode_adapter.go` / `codex_adapter.go` | 薄封装：`agent_kind=claude_code` / `codex`（决策A 首批双适配，记忆按 kind 隔离） |
| | `internal/acpclient/store.go` `db_init.go` `migrate_init.sql` `risk.go` `approval.go` `types.go` | 数据层（7 张表）/ 迁移（幂等+embed）/ 风险分级 / 决策词汇 / 领域模型 |
| **daemon** | `internal/server/server.go` | HTTP+WS：Bearer 鉴权、`/acp/frame`、`/api/*`（sessions/export/tool-calls/permission-requests/decision/approvals/approval-memory+DELETE/notifications/agent-requests）、内嵌 Web 控制台 |
| | `internal/server/bridge.go` | ACP 桥：适配器注册表 + 帧分发 + WS 事件（frame.in/out、agent.request/response、permission.resolved）|
| | `internal/server/feishu.go` | 飞书自定义机器人通知（加签、tail_hash 去重落库、Test/WebhookTail） |
| | `internal/server/console.go` | 控制台端点：`/`（BOOT 注入）、`/api/events`、`/api/agents`、`/api/rules`、`/api/notify/status|test` |
| | `internal/server/web/index.html` | **Web 控制台**（单文件，UI 稿 v0.4 布局，见 §0，go:embed） |
| **连接器** | `internal/connector/connector.go` + `cmd/agenthub-connect/main.go` | stdio↔daemon 桥：spawn 真实 ACP agent；agent 通知→`/acp/frame`；agent 权限请求→Hub 门控→判决映射回 agent 选项 id；Hub 命令→agent stdin；prompt 响应→回合完成回填；`fs/*`、`terminal/*` 明确报错 |
| **入口** | `cmd/agenthubd/main.go` | daemon：默认 8799；双适配器注册；`-retention-days`（默认 90，启动+每 24h 清扫）；`-feishu-webhook/-feishu-secret`（或 env）；token 三级来源 |

---

## 2. M2 完成内容（2026-11-07）

| 序号 | 内容 | 证据 |
|------|------|------|
| 1 | **BaseAdapter 提取**：Claude Code/Codex 共用同一审批门控、状态机、审批记忆（键含 agent_kind，互相隔离） | codex_adapter_test.go：kind 注册、apply_patch/shell 分类、记忆不跨 kind |
| 2 | **入站 session/request_permission**：真实 ACP agent 主动请求权限 → 同一审批管线 → 结果带 Hub requestId + agent 选项回显 | codex_adapter_test.go TestInboundRequestPermission |
| 3 | **stdio 连接器**：spawn 模式 + passthrough 模式；`Ready()` 就绪信号；unsupported 能力（fs/*、terminal/*）立即 JSON-RPC 报错不悬挂 | connector_test.go TestConnectorEndToEnd（进程内假 agent 全链路） |
| 4 | **Hub→agent 请求总线**：`POST /api/agent-requests` → WS agent.request → 连接器转发 → `POST /api/agent-requests/{id}/response` → WS agent.response；连接器在 agent 建会话后自动向 Hub 注册；prompt 响应自动回填 `session/update isComplete`（失败则 failed） | 实机冒烟：node 假 agent 经 agenthub-connect 建会话/跑回合/完成 |
| 5 | **Web 控制台**：5 tab 看板 + WS 实时刷新 + 审批 pill 计数 + 浏览器桌面通知 + 手动发帧调试表单 + 会话导出下载 | 浏览器实测（见 §3） |
| 6 | **审批两步确认（UI+服务端对齐）**：第一次点击 `confirm:false` → 服务端 `requiresConfirmation` → 按钮武装**保持到再次点击**（修复了 WS 重渲染清除武装态、8s 自动解除与服务端无过期不对称两个缺陷）；永久允许后记忆 `confirmCount=2` | 修复于 app.js `bindApprovalCards`/`decide`；服务端语义由 adapter_test 覆盖 |
| 7 | **飞书通知**（决策F）：`session/request_permission` 帧触发；HMAC 加签；tail_hash 24h 去重（API 层与落库层双保险）；notifications 表审计 | feishu_test（httptest webhook 收包验证）+ TestNotifierHookOnPermissionFrame |
| 8 | **保留期+导出**（决策G）：`-retention-days 90` 启动+每日清扫（FK 级联删 tool_calls/permission_requests/notifications，approval_memory 全局保留）；`GET /api/sessions/{id}/export` JSON 档案（session+toolCalls+permissionRequests+approvals） | db_extra_test（purge 级联/导出 bundle/记忆撤销/通知去重） |
| 9 | 记忆撤销：`DELETE /api/approval-memory/{id}` + 记忆页撤销按钮（confirm 弹窗） | m2_test.go TestMemoryRevokeEndpoint |

### 实机冒烟（2026-11-07，端口 18799）

**开箱链路**：`agenthubd` 启动（迁移+双适配器注册+保留期策略+控制台服务）→ `agenthub-connect -token … node /tmp/fake-agent.mjs`（spawn 模式）→ Hub `POST /api/agent-requests {session/new}` → 假 agent 建会话 → 连接器自动注册 Hub（/api/sessions 可见）→ `{session/prompt}` → agent 发 `session/request_permission`（cargo build，medium）→ Hub API `allow_once` 判决 → **agent 收到 `{outcome:selected, optionId:proceed_once}`（选项映射正确）** → 回合完成、会话 `completed`/turns=1、工具调用留痕 → `GET /api/sessions/{id}/export` 下载档案。

---

## 3. Web 控制台浏览器验证（2026-11-07，IAB 实测）

**通过（截图证据）**：
1. 控制台加载、深色主题渲染质量良好（顶栏/五 tab/表格/发帧表单）
2. Token 保存 → WS 绿点"已连接" + toast
3. 会话列表实时显示（状态徽章 awaiting_approval/completed、回合、tokens）
4. 会话详情：工具调用时间线（shell/approved/medium）+ 权限请求记录 + 导出按钮
5. 审批空态 ✓；API 触发请求后 **WS 实时推卡片**（不点刷新）+ tab pill 计数
6. "仅本次允许" → toast「已允许：pr_…」+ 卡片消失（瞬态 toast 已截图捕捉）
7. 永久允许两步确认 → 审批记忆页出现 allow / confirmCount=2 / 撤销按钮
8. 事件流 WS 事件（hello 等）；新版 app.js（含修复）已确认由二进制 serve

**受阻项（IAB 运行时输入通道故障，非页面缺陷）**：修复后的两步确认武装态点击复测、审计/事件流 tab 的手动交互、手动发帧表单点击。前期十余次点击成功证明页面 handler 正常；故障表现为 playwright/cua/dom_cua/键盘全部失效而只读正常（无对话框、有焦点、无遮挡、无循环重载）。**服务端两步确认语义已由 Go 测试覆盖**（TestAdapterPermanentVerdicts 等）；待后续换环境（或人工打开 http://127.0.0.1:8799/）复测武装态保持。

**测试中发现并修复的缺陷**：
- WS 重渲染清除永久判决武装态（`bindApprovalCards` 按 armed 状态恢复）
- UI 8s 自动解除与服务端 AwaitingConfirm 无过期不对称（移除自动解除，武装保持到点击）
- 重放 tool_call 缺 requestId（M1.5 后补）
- notifications 外键约束（测试需先建会话）

---

## 4. 决策落实状态（A–H）

| 字母 | 决策 | 状态 |
|------|------|------|
| A | 首批 Claude Code + Codex | **双适配器已注册**（claude_code + codex） |
| B | 一次+会话审批；永久判决二次确认；记忆自动套用 | **已实现**（once/session/always×allow+reject；两步确认服务端强制+UI 对齐） |
| C | 被动监测 + 高危拦截 | **已实现**（高危永不自动放行；只读白名单被动放行） |
| D | Web + Electron | **Web 控制台已实现**；Electron 壳未启动 |
| E | 登录鉴权 + 多用户 + 远程开关 | 单 token 鉴权已实现；多用户/远程开关未做（默认仅 127.0.0.1） |
| F | 桌面通知 + 飞书 webhook + 自动更新 | 浏览器通知 + 飞书已实现；自动更新未做 |
| G | SQLite 90 天保留 + 导出 | **已实现**（清扫 job + 会话导出） |
| H | Open Core + 商业插件 | M3 |

---

## 5. 运行指南（开箱即用）

```
# 启动 daemon（数据目录 ./data，token 自动生成到 data/token）
go run ./cmd/agenthubd                      # http://127.0.0.1:8799
# 常用 flag：-addr 127.0.0.1:8799 -data data -retention-days 90
#            -feishu-webhook <url> -feishu-secret <sign>（或 env AGENTHUB_FEISHU_WEBHOOK/SECRET）

# 接入真实 ACP agent（如 Claude Code）
go run ./cmd/agenthub-connect -token <token> npx -y @zed-industries/claude-code-acp

# 打开控制台
# 浏览器访问 http://127.0.0.1:8799/ ，粘贴 data/token 中的 API Token
```

API 速查：`GET /healthz`（免鉴权）；`POST /acp/frame`；`POST /api/agent-requests`(+`/{id}/response`)；`GET /api/sessions`、`/api/sessions/{id}/export`、`/api/tool-calls?sessionId=`；`GET /api/permission-requests[?outcome=pending]`、`POST /api/permission-requests/{uid}/decision`；`GET/DELETE /api/approval-memory[/{id}]`；`GET /api/approvals`、`/api/notifications`；`GET /ws?token=`。

帧词汇（Hub 内部 ACP v2 draft）：入站 `initialize/session/new/session/prompt/tool_call/tool_result/session/update/session/request_permission`；出站 `session/update`、`session/request_permission`、`tool_call/approved|rejected`；WS 事件 `hello/frame.in/frame.out/frame.error/agent.request/agent.response/permission.resolved`。

---

## 6. 下一步建议（M2 收尾 → M3）

1. **Electron 桌面壳**（决策D 余项）：包裹 Web 控制台（BrowserWindow 加载 127.0.0.1:8799），托盘、开机自启；自动更新（决策F 余项）。
2. **多用户与远程开关**（决策E）：多 token/用户隔离、`-remote` 开关默认关。
3. **真实 Claude Code 联调**：本机安装 `@zed-industries/claude-code-acp` 后走连接器实机验证（连接器已按其协议实现：session/new 分配 id、prompt 生命周期、request_permission 映射、fs/* 能力缺失报错——如需读写文件回调，需在连接器实现 fs/read_file、fs/write_file）。
4. **Codex 真实接入**：Codex CLI 非 ACP 协议，需 codex 专用桥接器（Hub 侧适配器已就绪）。
5. **UI 复测**：换常规浏览器完成武装态保持复测 + 审计/事件流 tab 手动验证。
6. **M3**：审计导出/归档增强 + Open Core 许可 + 商业插件框架。

---

## 7. 待选项（用户可随时指示，原样保留）

- 提升权限终端跑 `node D:\DevTools\codex-elevated-verify.mjs`（Codex 受限沙箱闭环）
- zcode CLI 派发（headless `-p` 卡在 "Select a model before sending a message"）
- M2 项：系统终端跳转 / 内嵌 xterm.js / F6 统一操作面
- 真实 DSH `session/request_permission` 触发
- Codex 真实触发闭环（danger-full-access + 非提升令牌）
- Gemini CLI 实机适配验证

## 8. 环境备注

- 8799 端口编写时有旧进程占用冒烟用 18799；新会话如需 8799 先确认旧进程。
- 本机无 C 编译器（CGO_ENABLED=0）：用纯 Go 依赖（modernc.org/sqlite）；`go test -race` 不可用。
- Bash `UID` 是只读变量，勿作变量名。
- 冒烟临时产物已全部清理（/tmp/agenthubd*、fake-agent.mjs、wsclient 等），工作区仅含源码树。

---

**当前状态**：M2 核心五项（双适配器、连接器、Web 控制台、通知、保留/导出）完成并有测试与实机证据；41 个测试用例全绿；等待指示进入 Electron 壳/M3 或真实 agent 联调。
