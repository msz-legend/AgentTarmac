# AgentTarmac 测试方案与测试用例 v1.0

> 生成时间：2026-11-07 ｜ 作者：QA（依据 PRD_v0.4 + HANDOFF §-1/§-0.5 交付现状）
> 测试对象：`D:\DevTools\AgentHub`（module `github.com/agenttarmac/agenttarmac`）
> 依据文档：`PRD_v0.4.md`（F1–F9 / D1–D19 / §5.6 / §6 / §9）、`design/EVENT_MODEL_v0.2.md`（冻结契约）、`design/UI_UX_v0.1.md`（内容版本 v0.4）、`HANDOFF.md`
> 当前基线：`go build/vet/gofmt` 全净；7 包 80+ Go 用例全绿；M1 实机验收 19/19 + M2 批次实机验收 10/10

---

## 1. 测试对象与范围

### 1.1 被测系统组成

| 组件 | 路径 | 说明 |
|---|---|---|
| daemon（Hub） | `cmd/agenthubd` | HTTP+WS 服务、审批管线、任务队列、通知、内嵌 Web 控制台 |
| hub-hook（Go 版） | `cmd/hub-hook` | CC PreToolUse / Codex PermissionRequest 钩子；`install/uninstall` 子命令 |
| stdio 连接器 | `cmd/agenthub-connect` | spawn 真实 ACP agent，stdio↔daemon 桥 |
| 中继服务端 | `cmd/agenthub-relay` | D8 自托管公网中继（TLS） |
| 适配器/规则/运行器/任务/推送 | `internal/acpclient`、`internal/rules`、`internal/runner`、`internal/tasks`、`internal/push`、`internal/relay` | 核心库 |
| Web 控制台 | `internal/server/web/index.html` + `sw.js` | 单文件零构建，go:embed |

### 1.2 范围（In）

PRD §3 In 项全部纳入：F1–F9、§5.6 数据保留、§6 事件模型 v0.2 契约、§9 安全红线（含 D8 中继五条红线）、D16 审批四选项、D17 通知策略、D13 双模（附加+托管）。

### 1.3 范围外（Out / 已知边界）

- 多用户/多机聚合、云服务、DAG 编排（PRD Out）。
- xterm.js 内嵌终端（M2 环境阻塞：CDN 资产不可达；见 §10）。
- macOS 真机验证（darwin provider 代码就绪，本环境无 Mac；见 §10）。
- 个人微信通知（D17 明确不做）。
- `go test -race`（本机无 CGO，环境限制；以 `-count=2` + 并发用例设计补偿）。

---

## 2. 测试策略（分层）

```
L0 单元/组件    Go test ./...（80+ 用例）—— 回归基线，每轮必跑
L1 集成         包间 e2e（httptest + 假 agent + 假 spawner + 假 webhook）
L2 实机验收     真实 daemon + 真实 CLI（wt.exe / hub-hook / node 假 agent）python 验收脚本
L3 UI 验收      浏览器手工/CDP（对照 UI 稿 v0.4 布局与 D18 决策）
L4 用户旅程     E2E 场景（§8）：附加全链路 / 托管全链路 / 手机+中继 / 关标签页 SW / 重启恢复
专项            安全 SEC / 数据 DATA / 可靠性 REL / 性能 PERF / 兼容性 COMP
```

**测试类型与执行方式**：

| 类型 | 标记 | 执行方式 | 触发时机 |
|---|---|---|---|
| 自动-已覆盖 | `自动` | `go test ./...` | 每次提交 |
| 自动-待建 | `脚本` | python 验收脚本（附录 A 清单） | 每轮发布前 |
| 手工-UI | `手工` | 浏览器（PC 1440×900 + 移动 390×844） | 每轮发布前 |
| 手工-实机 | `实机` | 真实 CC/Codex CLI + 手机设备 | 里程碑验收 |

### 优先级定义

| 级别 | 定义 | 准出要求 |
|---|---|---|
| **P0** | 冒烟 + 核心链路 + 安全红线（审批门控 fail-closed、鉴权、数据审计） | 100% 通过，阻塞发布 |
| **P1** | 重要功能路径（四选项全部、任务队列、通知送达、中继转发） | 100% 通过，阻塞发布 |
| **P2** | 边缘/体验（i18n、键盘、断点、掩码展示） | ≥95% 通过或带已知问题发布 |

### 缺陷严重级定义

| 级别 | 定义 | 示例 |
|---|---|---|
| S1 致命 | 审批被绕过/误放行、数据丢失、鉴权失效、fail-closed 失效 | 高危工具未拦截即执行；token 校验缺失；审计缺 rule_scope |
| S2 严重 | 功能不可用、错误结果、崩溃 | 任务队列卡死；通知不送达；resume 失败 |
| S3 一般 | 次要功能缺陷、体验问题 | 掩码显示错误；i18n 漏翻 |
| S4 轻微 | 文案/样式 | 间距、配色偏差 |

---

## 3. 测试环境与数据准备

### 3.1 环境矩阵

| 环境 | 用途 | 配置 |
|---|---|---|
| ENV-WIN（主） | 全量功能 + 实机 | Windows 11 x64、Go 1.27、`CGO_ENABLED=0`、Node（假 agent）、Claude Code CLI + Codex CLI 已安装、wt.exe 可用 |
| ENV-WIN-无网 | fail-closed 专项 | 断网/防火墙阻断 8799 |
| ENV-RELAY | 中继专项 | 独立主机/容器跑 `agenthub-relay`（自签 TLS；`-insecure` 仅开发网） |
| ENV-MOBILE | 多端 | 手机浏览器（iOS Safari + Android Chrome）经局域网/中继 |
| ENV-MAC（待） | M2 macOS | 真机 Mac（darwin provider 验证） |

### 3.2 启动命令基线

```bash
# Hub（默认 127.0.0.1:8799，retention 永久）
go run ./cmd/agenthubd
# 常用变体
go run ./cmd/agenthubd -addr 127.0.0.1:18799 -data /tmp/qa-data -retention-days 90
go run ./cmd/agenthubd -feishu-webhook <url> -feishu-secret <sign> -wecom-webhook <url>
go run ./cmd/agenthubd -relay wss://relay.example/relay/hub/qa-hub?token=<rt> -relay-hub-id qa-hub

# 钩子安装
go build -o bin/hub-hook.exe ./cmd/hub-hook && ./bin/hub-hook.exe install --url http://127.0.0.1:8799 --token <t>
./bin/hub-hook.exe install --codex   # Codex hooks.json

# 接入 ACP agent（附加模式）
go run ./cmd/agenthub-connect -token <t> node /tmp/fake-agent.mjs
```

### 3.3 测试数据与替身

| 替身 | 用途 |
|---|---|
| `fake-agent.mjs`（node 假 ACP agent） | 经连接器建会话、发 `session/request_permission`、完成回合 |
| fake spawner（`SetSpawner`） | 任务队列 e2e：可控行为（成功/失败/挂起/JSONL 输出） |
| httptest webhook 接收器 | 飞书/企微收包断言（加签、字段、脱敏） |
| 风险用例命令集 | 低危 `Read/Grep/Glob/LS/TodoRead/TodoWrite/NotebookRead`；中危 `cargo build`、文件写入；高危 `rm -rf`、`git push`、`curl -X POST`、任意命令 |
| 规则用例集 | `Bash(cargo build)` 精确 / `Bash(cargo *)` glob / `Bash(cargo ?ild)` 单字符 / 裸 `Bash` |

### 3.4 端口纪律

8799 为默认；若被旧进程占用改用 18799（历史冒烟端口）。每轮测试前 `netstat -ano | findstr 8799` 确认无残留 daemon，避免"旧二进制 serve 旧 UI"类假象（历史教训：/tmp 陈旧二进制）。

---

## 4. 准入 / 准出标准

**准入（每轮测试开始）**：`go build ./...`、`go vet ./...`、`gofmt -l .` 为空、`go test ./...` 全绿、daemon 可启动且迁移幂等。

**准出（发布）**：
1. P0 用例 100% 通过；S1/S2 缺陷清零；P1 100%；P2 ≥95%。
2. PRD 硬性验收指标全达标：F3 全链路 ≤1s、F9 通知 ≤3s、F6/F7/F8 各验收句逐字满足。
3. 自动化回归（§9 映射表）全绿 + 实机验收脚本（附录 A）全 PASS。
4. 安全专项（§7.10）无 S1/S2。
5. 已知边界（§10）在发布说明中如实披露。

---

## 5. 风险分析与测试重点

| # | 风险 | 影响 | 测试对策 |
|---|---|---|---|
| R1 | 审批门控被绕过（高危自动放行/记忆误命中） | S1 安全事故 | 决策C 门控顺序专项（TC-F3-101~）＋fail-closed 全路径 |
| R2 | hook 通道阻塞卡死 Agent（hub 不可达、超时） | Agent 停摆 | 2min 超时、断网、hub 返回非 2xx、坏 JSON 四路径 |
| R3 | 审计不完整（四选项不可追溯） | 违反 PRD F3 验收 | 每个决策路径断言 approval_history + permission_resolved 落库 |
| R4 | daemon 重启状态错乱（pending 审批/运行中任务） | 数据可信度 | REL 专项：kill -9 后重启断言恢复语义 |
| R5 | 中继泄露（明文落地、token 泄露） | D8 红线违反 | 中继零存储断言、header 白名单、token 校验、401 透传 |
| R6 | webhook 凭据泄露（进前端 bundle/日志） | D17 安全 | 代码 grep 断言 + 掩码回显 + 通知仅摘要 |
| R7 | 并发竞态（队列 dispatch、waiters map、规则写） | 偶发错乱 | 并发用例 + `-count=2`（race 检测器不可用补偿） |
| R8 | 平台差异（wt.exe 缺失回退、路径分隔符） | Windows 一等公民卖点 | COMP 专项 + 平台 provider 单测 |

---

## 6. 缺陷管理

- 提交字段：ID / 标题 / 严重级 / 优先级 / 复现步骤 / 期望 vs 实际 / 环境（端口、二进制构建时间）/ 证据（日志、截图、事件 JSON）。
- 审批类缺陷必须附 `GET /api/events` 与 `GET /api/approvals` 原始 JSON。
- 修复后必跑：所属模块 L0 全包 + 关联 TC + 冒烟（§7.0）。

---

## 7. 测试用例

> 图例：类型 = `自动`（Go test 已覆盖）｜`脚本`（验收脚本）｜`手工`（浏览器）｜`实机`（真实 CLI/设备）

### 7.0 冒烟用例（每轮必跑，<10 分钟）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| SMK-01 | daemon 启动三件套净 | P0 | 自动 | `go build ./... && go vet ./... && gofmt -l .` | 零错误零 diff |
| SMK-02 | 单测全绿 | P0 | 自动 | `go test ./...`（`-count=2` 抽查） | 7 包全 PASS |
| SMK-03 | daemon 起服+健康检查 | P0 | 脚本 | 启动 → `GET /healthz` | 200；token 写入 `<data>/token`；日志无迁移报错 |
| SMK-04 | 控制台可开 | P0 | 手工 | 浏览器开 `http://127.0.0.1:8799/` | 态势大屏渲染；BOOT token 注入；WS 绿点"已连接" |
| SMK-05 | 审批门控活着 | P0 | 脚本 | 假 agent 触发 medium 请求 → `allow_once` 判决 | agent 收 `outcome:selected, optionId` 映射正确；`/api/events` 出现 permission_request+resolved |
| SMK-06 | 任务派发一圈 | P0 | 脚本 | `POST /api/tasks`（fake spawner 成功路径） | queued→started→finished；managed 会话建立 |
| SMK-07 | 通知链路通 | P1 | 脚本 | 配 httptest webhook → 触发高危请求 | webhook 收包；`/api/notify/status` 显示掩码尾号 |

### 7.1 F3 审批收件箱（核心卖点，D16 / Q-F4 / 决策B）

#### 7.1.1 四选项语义

| ID | 用例 | 优先级 | 类型 | 前置 | 步骤 | 预期 |
|---|---|---|---|---|---|---|
| TC-F3-001 | ①放行一次 | P0 | 自动 | pending 请求（medium，`cargo build`） | `POST /api/permission-requests/{id}/decision` `{decision:"allow_once"}` | 返回成功；agent 收放行；事件 `permission_resolved{outcome:"allowed_once", rule_scope:"none", decided_by:"hub"}` |
| TC-F3-002 | ②该 session 放行→规则生效 | P0 | 自动 | 同上 | 决策 `allow_session` → 同 session 再触发同类调用 | 第二次 `decided_by:"policy"` 自动放行，全链路 <1s，无新人工请求 |
| TC-F3-003 | ②规则随 session 失效 | P0 | 自动 | TC-F3-002 后 | 结束该 session；新 session 触发同类调用 | 新 session 不自动放行（session 作用域规则不跨会话），走人工 |
| TC-F3-004 | ③放行 Always→持久规则 | P0 | 自动 | pending 请求 | 决策 `allow_always`（两次点击确认） → 新 session 触发同类 | 自动放行 `decided_by:"policy"`；`GET /api/rules` 可见持久规则 |
| TC-F3-005 | ④拒绝+复验 note | P0 | 自动 | pending 请求 | 决策 `reject_once` + `note:"生产库勿动"` | agent 收拒绝；事件 `{outcome:"rejected", rule_scope:"none"}`；approval_history 与 resolved 事件均含 note 原文 |
| TC-F3-006 | 拒绝→hook 侧 exit 2 | P0 | 实机 | hub-hook 已 install，daemon 起服 | 触发 shell 高危，人在控制台拒绝，观察 hook 进程退出码 | hook exit 2（fail-closed 信号）；stderr 含 "AgentHub: denied"（+note） |
| TC-F3-007 | cancel/unavailable 词汇 | P1 | 自动 | pending 请求 | 超时路径（mock 2min 到期）/会话死亡 | `outcome:"unavailable"` + `decided_by:"timeout"`；词表不出格（EVENT_MODEL v0.2 §3） |

#### 7.1.2 永久判决二次确认（决策B × D16 选项3）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F3-010 | allow_always 需两步 | P0 | 自动 | 首次 `{decision:"allow_always",confirm:false}` | 响应 `requiresConfirmation:true`，**不落规则**、不回 agent |
| TC-F3-011 | allow_always 二次落地 | P0 | 自动 | 同上后 `{confirm:true}` | 规则落库；agent 收放行；记忆 `confirm_count=2` |
| TC-F3-012 | reject_always 需两步 | P0 | 自动 | 同 TC-F3-010 流程（reject_always） | 同上；二次后审批记忆出现 deny 记录 |
| TC-F3-013 | 武装态 UI 保持 | P1 | 手工 | UI 点击"放行 Always"一次 → 触发 WS 重渲染（另发一条事件）→ 再点击 | 按钮保持紫色 .arm 武装态不被重渲染清除；二次点击落地（历史缺陷回归） |
| TC-F3-014 | 武装态不过期 | P2 | 手工 | 武装后等待 >8s 再点击 | 仍可确认落地（服务端无过期；历史缺陷回归） |

#### 7.1.3 风险门控顺序（决策C）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F3-101 | 高危永不自动放行 | P0 | 自动 | 先建 `Bash(*)` allow_always 规则 → 触发高危 `rm -rf /` 类调用 | 仍进人工收件箱（高危拦截优先于 policy），事件 `risk:"high"` |
| TC-F3-102 | policy 优先于 deny 记忆？否——顺序验证 | P0 | 自动 | 造 deny 记忆 + 无 policy 规则 → 触发同参 medium | 直接拒（decided_by=policy/记忆路径），不弹人工 |
| TC-F3-103 | 低危被动放行 | P0 | 自动 | 触发 `Read`/`Grep` | 无人工请求，tool_call 正常留痕（risk=low） |
| TC-F3-104 | 记忆按 agent_kind 隔离 | P0 | 自动 | CC 侧 allow_always `Bash(x)` → Codex 侧触发 `Bash(x)` | Codex 侧不命中记忆，仍走人工（键含 kind） |
| TC-F3-105 | 高危判定口径（D11） | P1 | 自动 | 参数化：`rm`、`git push`、`curl -X POST`（网络发送）、删除/覆盖写 | 全部 `risk:"high"`；文件写入=medium；只读=low |

#### 7.1.4 规则引擎（Q-F4）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F3-201 | 精确 pattern 匹配 | P0 | 自动 | 规则 `Bash(cargo build)` → 触发 `cargo build` | 命中自动放行 |
| TC-F3-202 | 精确不误匹配 | P0 | 自动 | 规则 `Bash(cargo build)` → 触发 `cargo build --release` | 不命中（主参数精确匹配，非前缀） |
| TC-F3-203 | glob `*` | P0 | 自动 | 规则 `Bash(cargo *)` → 触发 `cargo test` / `cargo build --release` | 均命中 |
| TC-F3-204 | glob `?` 单字符 | P2 | 自动 | 规则 `Bash(cargo ?ild)` → `cargo build` 命中；`cargo rebuild` 不命中 | 按字面 glob 语义 |
| TC-F3-205 | 最具体优先 | P0 | 自动 | 同存 `Bash`（裸）与 `Bash(cargo *)` → 触发 `cargo build` | 命中更具体的 `Bash(cargo *)`（toolScore+patScore 评分） |
| TC-F3-206 | 文件工具主参数=file_path | P1 | 自动 | 触发 Write `D:\a.txt`；建规则 `Write(D:\a.txt)` | pattern 取 file_path 而非 command |
| TC-F3-207 | session 规则不跨 kind/会话持久化 | P1 | 自动 | 重启 daemon 后查 rules 表 | session 作用域规则已随会话清理；always 规则仍在 |
| TC-F3-208 | 规则删除即时生效 | P1 | 自动 | `DELETE /api/rules/{id}` → 再触发同类 | 不再自动放行，回人工 |

#### 7.1.5 hook 通道（D6 Go 版 hub-hook + Node hooks）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F3-301 | 低危本地短路 | P0 | 实机 | `echo '{"tool_name":"Read",…}' \| hub-hook.exe` | 立即 exit 0 输出 allow JSON，**不请求 hub**（无网络流量） |
| TC-F3-302 | 中危阻塞→人工放行 | P0 | 实机 | hook 输入 `Bash(cargo build)` → 控制台放行 | hook exit 0，输出含 `permissionDecision:"allow"` + rule_scope |
| TC-F3-303 | hub 不可达 fail-closed | P0 | 实机 | daemon 停服 → 触发 hook | exit 2 + deny JSON；stderr "hub unreachable"；**绝不默认放行** |
| TC-F3-304 | 2min 判决超时 | P0 | 脚本 | hook `--timeout-ms 2000` + 无人判决 | hub 返回 `outcome:"unavailable"`(timeout) → hook exit 2 deny |
| TC-F3-305 | hub 返回非 2xx | P1 | 实机 | 指向 404 端口 | exit 2 deny，stderr 带状态码 |
| TC-F3-306 | hub 返回坏 JSON | P1 | 实机 | mock 返回非 JSON | exit 2 deny（"unreadable verdict"） |
| TC-F3-307 | install 幂等合并 | P0 | 实机 | 连续 install 两次；settings.json 预置用户自有 hook | 自己的条目去重；**用户无关 keys 与他人 hook 条目原样保留** |
| TC-F3-308 | uninstall 只删自己 | P0 | 实机 | uninstall 后检查 settings.json / hooks.json | 仅含 ManagedMarker 的条目被移除；空容器清理；其余保留 |
| TC-F3-309 | --codex 安装路径 | P1 | 实机 | `hub-hook install --codex`（含 CODEX_HOME 环境变量场景） | 写入 `<CODEX_HOME|~/.codex>/hooks.json` 的 PermissionRequest；timeout 字段=timeoutMS/1000+30 |
| TC-F3-310 | argv 顺序兼容 | P2 | 实机 | `hub-hook install --url …` 与 `hub-hook --url … install` 两种顺序 | 均成功（子命令先分离再解析 flags） |
| TC-F3-311 | env 兜底 | P2 | 实机 | 仅设 `AGENTHUB_URL/TOKEN/TIMEOUT_MS` 不传 flag | 生效（envOr 链） |
| TC-F3-312 | intake 鉴权 | P0 | 脚本 | 无 token / 错 token POST `/api/hook/intake` | 401 |
| TC-F3-313 | 主参数进帧 | P0 | 脚本 | 触发 shell/文件请求 | permission_request payload 含 **command 原文** 与 args_pattern、`rule_supported:true`（PRD §6 验收回归） |

#### 7.1.6 审计与留痕

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F3-401 | 四选项全可审计 | P0 | 脚本 | 依次走 4 选项各一次 | 每次均有 approval_history 行 + permission_resolved 事件；`rule_scope`/`note`/`decided_by` 落库可查（PRD F3 验收句） |
| TC-F3-402 | 记忆撤销 | P1 | 自动 | `DELETE /api/approval-memory/{id}` | 记忆消失；后续同类回人工；UI 记忆页撤销按钮同效 |
| TC-F3-403 | 审批不串会话 | P1 | 自动 | 两个并行 session 各自 pending | 决策 A 不影响 B 的 pending；waiters 不串扰 |

### 7.2 F6 统一操作面——任务队列（D13）

| ID | 用例 | 优先级 | 类型 | 前置 | 步骤 | 预期 |
|---|---|---|---|---|---|---|
| TC-F6-001 | 派发全生命周期 | P0 | 自动 | fake spawner 成功 | `POST /api/tasks {agent_kind:"claude_code", cwd, prompt}` | queued→started→finished；每步 TaskEvent 进环+WS；任务台账状态丸正确 |
| TC-F6-002 | managed 会话建立 | P0 | 自动 | 同上 | 查 sessions 表 | `origin:"managed"` 行；task.session_id 与之链接；title=prompt 截断 |
| TC-F6-003 | 并发上限 | P0 | 自动 | `-task-concurrency 2` + 3 个慢任务 | 观察运行数 | 同时最多 2 个 running，第 3 个排队；先完成的唤醒后续 |
| TC-F6-004 | 失败自动重试 | P0 | 自动 | fake spawner 首次失败后成功 | 派发 | fail → `retries++` → 回 queued → 重跑 → finished；`maxRetries` 耗尽后 failed |
| TC-F6-005 | 手动 retry 仅限 failed | P1 | 自动 | 对 finished/running 任务 `POST /api/tasks/{id}/retry` | 4xx 报错"only failed tasks can be retried"；failed 任务重排队成功 |
| TC-F6-006 | 取消排队任务 | P0 | 自动 | 并发占满 + 2 个排队 → cancel | 直接 cancelled（不 spawn）；事件 cancelled |
| TC-F6-007 | 取消运行中任务 | P0 | 自动 | 慢任务运行中 cancel | 进程被 kill；终态 cancelled；事件 cancelled |
| TC-F6-008 | 暂停阻塞派发 | P0 | 自动 | pause → 派发新任务 | 任务停留 queued 不 spawn；**运行中任务不受影响继续跑**（M2 验收项） |
| TC-F6-009 | 恢复续跑 | P0 | 自动 | TC-F6-008 后 resume | 排队任务开始派发；`GET /api/tasks/queue` 状态正确（Paused()） |
| TC-F6-010 | 无效参数拒绝 | P1 | 自动 | agent_kind="gemini"/cwd 空/prompt 空 | 400 带明确错误（v1 仅 claude_code\|codex 可驱动） |
| TC-F6-011 | CC 无 hub-hook 拒绝 spawn | P1 | 脚本 | 不配 `-hook-path` 且旁无 hub-hook.exe → 派发 CC 任务 | 任务 failed，错误文案"需要 hub-hook"，**不静默降级为无审批运行** |
| TC-F6-012 | managed CC 审批进同一收件箱 | P0 | 实机 | 真实/假 CC headless + settings 注入 → 触发工具调用 | 审批出现在**同一个**收件箱（与附加模式同管线）；放行后任务继续 |
| TC-F6-013 | managed Codex 命令 | P1 | 自动 | 派发 codex 任务 | 命令为 `codex exec --json <prompt>`、Dir=cwd |
| TC-F6-014 | 生命周期事件词汇 | P1 | 自动 | 全路径观察 | 事件 action ∈ {queued,started,finished,failed,cancelled}，信封 type="task" |
| TC-F6-015 | spawn 失败会话回滚 | P2 | 自动 | spawner 报错 | 任务 failed；会话行 status=failed + last_error（看板不失真） |
| TC-F6-016 | JSONL 大输出 | P2 | 自动 | fake agent 输出 >64KB 单行 | scanner 8MB 预算内不崩、正常映射 |
| TC-F6-017 | 看板 UI 全操作 | P1 | 手工 | 任务视图 | 派发表单（Agent/工作目录/描述）→台账实时刷新（WS）→暂停/恢复→重试/取消按钮均有反馈 |

### 7.3 F7 会话复用与多会话并行（D14）

| ID | 用例 | 优先级 | 类型 | 前置 | 步骤 | 预期 |
|---|---|---|---|---|---|---|
| TC-F7-001 | 3 会话并行独立 | P0 | 实机 | 同一 agent 经连接器开 3 个 session，各触发 1 个 pending | 收件箱三张卡独立，各自会话字段正确；放行 A 不影响 B/C；sessions 表 3 行 running（PRD 验收句逐字） |
| TC-F7-002 | 继续此会话-托管路 | P0 | 自动 | managed 会话 | `POST /api/sessions/{id}/continue` 带新任务 | 经命令总线送达 agent；会话回合+1 |
| TC-F7-003 | 继续此会话-CC 原生恢复 | P0 | 脚本 | attached CC 会话（假 claude.cmd 可捕获 argv） | continue | 命令为 `claude --resume <id> --print --output-format stream-json --verbose <prompt>` 且 **cwd 正确携带**（CC 会话搜索依赖 cwd）；stream-json 行映射为 tool_call/tool_result/status 帧 |
| TC-F7-004 | Codex 原生恢复 | P0 | 脚本 | attached codex 会话 | continue | 命令为 `codex exec resume <id> --json <prompt>` |
| TC-F7-005 | --resume 显式 id 而非 --continue | P0 | 自动 | 检查 buildCommand | 命令含 `--resume <id>`；**不得**出现 `--continue`（CC --continue 会跳过 -p 会话，PRD D14 注记） |
| TC-F7-006 | 单飞保护 | P1 | 自动 | resume 运行中再次 continue | 第二次被拒（in-flight guard），不产生并发双进程 |
| TC-F7-007 | 分叉继续 | P1 | 自动 | `POST /api/sessions/{id}/fork` | 新会话 `parent_session_id` 指向父；UI 会话卡显示 fork 行（lineage 可见） |
| TC-F7-008 | title=首条 prompt | P1 | 自动 | 建会话带 prompt | sessions.title 为首条 prompt（截断 80） |
| TC-F7-009 | 未知会话 continue | P2 | 自动 | 不存在的 id | 404（errors.Is ErrNotFound） |

### 7.4 F8 会话视图与终端跳转（D15）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F8-001 | Hub 内只读视图 | P0 | 手工 | 会话卡展开详情 | 头部（kind/cwd/origin/状态/tokens）+ **按 session 过滤**的事件流（PRD 验收句） |
| TC-F8-002 | 三处入口跳转 | P0 | 手工 | 放行条「查看会话」/ 机位卡 / 任务卡 分别点击 | 均跳到对应会话视图并见当前状态（PRD 验收句：三处入口） |
| TC-F8-003 | wt.exe 拉起 | P0 | 实机 | `POST /api/sessions/{id}/terminal`（CC 会话） | 真实 wt.exe 窗口打开于会话 cwd，命令含 resume 参数（M1 验收 19/19 中该项回归） |
| TC-F8-004 | ACP 系 409 | P0 | 脚本 | 对 ACP 系（无原生 resume）会话调 terminal | 409 + 提示"在 Hub 内查看"（按钮隐藏） |
| TC-F8-005 | 终端缺失回退链 | P1 | 自动 | mock PATH 无 wt.exe | 回退 `cmd start`/conhost；darwin=osascript、linux=x-terminal-emulator 回退链由 provider 单测覆盖 |
| TC-F8-006 | 会话导出 | P1 | 脚本 | `GET /api/sessions/{id}/export` | JSON 档案（session+toolCalls+permissionRequests+approvals）与 Markdown 两种格式可下载、内容完整 |

### 7.5 F9 通知（D17 + M1.5 SW）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F9-001 | 手势授权门控 | P1 | 手工 | 设置页点「启用通知」 | `Notification.requestPermission` 仅在手势中调用；`file://` 或非安全上下文时入口隐藏/降级 |
| TC-F9-002 | 桌面通知内容脱敏 | P0 | 手工 | 触发高危审批 → 查看通知 | 仅摘要（Agent/风险/会话/动作），**不含完整命令参数**（D17 安全句） |
| TC-F9-003 | 通知策略矩阵 | P0 | 自动 | 参数化：高危待放行/失败失联/中低危待放行/任务完成 | 高危✅立即、失败✅立即、中低危→仅桌面不推 IM、完成→可配（localStorage notifyTask 开关生效） |
| TC-F9-004 | SW 推送关标签页可达 | P0 | 实机 | 注册 SW + 订阅 → **关闭所有 Hub 标签页** → 触发高危 | 系统级推送仍弹出（F9 验收句：关标签页仍可达）；点击通知聚焦/打开 Hub |
| TC-F9-005 | VAPID 持久化 | P1 | 自动 | 首启生成 → 重启 daemon | 密钥不变（data 目录持久化）；`/api/notify/status` 可见 |
| TC-F9-006 | 410 自动退订 | P1 | 自动 | mock push 返回 410 | 该订阅从注册表移除，不再重试 |
| TC-F9-007 | 30s 静默期合并 | P0 | 自动 | 同 session 连续 2 次同类通知（<30s） | 第 2 次被抑制（feishu/企微/桌面同策略；kind+session 键） |
| TC-F9-008 | webhook 掩码回显 | P1 | 手工 | 配置 webhook 后看设置页 | 仅尾号 6 位（WebhookTail）；完整 URL 任何接口不回显 |
| TC-F9-009 | tail_hash 24h 去重 | P1 | 自动 | 相同 tailHash 24h 内二次触发 | 第二次不发送（API 层+落库层双保险）；notifications 表单行 |
| TC-F9-010 | 飞书加签 | P1 | 自动 | 配 secret → httptest 收包 | 请求含合法 `sign` + `timestamp`（HMAC-SHA256 口径） |
| TC-F9-011 | 企微通道 | P1 | 自动 | `-wecom-webhook` + 触发高危/失败 | msgtype=text 内容正确；策略与飞书一致（静默/去重/掩码） |
| TC-F9-012 | notify/test | P2 | 脚本 | `POST /api/notify/test` | 未配置→明确错误；已配置→测试消息送达群 |
| TC-F9-013 | webhook 不进前端/日志 | P0 | 脚本 | `grep` index.html 与 daemon 日志输出 | URL/secret 零出现（只允许掩码尾号） |
| TC-F9-014 | 送达时延 ≤3s | P0 | 实机 | 触发高危→手机/群收到 | ≤3s（PRD F9 验收句；本机测） |
| TC-F9-015 | sw.js 服务 | P1 | 脚本 | `GET /sw.js`（免鉴权） | 200、JS mime、含 push/notificationclick handler |

### 7.6 F4 状态机与告警

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F4-001 | 7 态映射 | P0 | 自动 | 驱动各路径（start/prompt/审批/完成/失败/杀进程） | 状态 ∈ starting/running/waiting_approval/idle/finished/failed/dead，与 UI 稿 §1.4 一致 |
| TC-F4-002 | waiting_approval 进出 | P0 | 自动 | 触发 pending → 放行 | 进入 waiting_approval；判决后回 running/idle |
| TC-F4-003 | 假活检测 | P1 | 脚本 | 伪造 >120s 无活动的 running 会话 | UI 出现 ⚠疑似失联标记；**不擅自 kill**（仅告警）；失败/失联通知发出 |
| TC-F4-004 | 进程死亡→dead | P1 | 实机 | kill 真实 agent 进程 | 会话 dead + 失败通知 |

### 7.7 F1/F2 发现接入与统一展示

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F1-001 | 适配器注册 | P0 | 自动 | daemon 启动 → `GET /api/agents` | claude_code + codex 双适配器列出 |
| TC-F1-002 | 附加会话自动注册 | P0 | 脚本 | 假 agent 经连接器建会话 | sessions 表 `origin:"attached"` 行自动出现 |
| TC-F1-003 | /acp/frame 入站 | P0 | 自动 | POST 各帧型（initialize/session/new/prompt/tool_call/tool_result/session/update/request_permission） | 均被 HandleFrame 正确分发；未知帧型报错不崩 |
| TC-F2-001 | 事件环与 WS 推送 | P0 | 脚本 | 触发事件 → `GET /api/events` + WS | 环内可查（上限 500 条滚动）；WS 实时推送同内容（envelope v0.2 形状） |
| TC-F2-002 | tool_result 记录 | P1 | 自动 | 入站 tool_result 帧 | recordFrameIn 落库；tool_call 状态更新 |
| TC-F2-003 | 筛选维度 | P2 | 手工 | 态势大屏 roster 全部/仅活跃切换 | 数据正确切换 |

### 7.8 F5 多端一致 + D8 中继

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-F5-001 | 局域网绑定 | P1 | 实机 | `-addr 0.0.0.0:8799` 手机同网访问 | 手机可开控制台；默认不传参时仅 127.0.0.1 |
| TC-F5-002 | relay 端到端转发 | P0 | 自动 | relay 起 → hub 出站连 → 模拟浏览器 `GET {hubID}/api/sessions` | 返回 hub 真实响应（已覆盖 relay_test，回归） |
| TC-F5-003 | relay 401 透传 | P0 | 自动 | 带错 hub token 经 relay 请求 | 401 原样透传（不吞不改） |
| TC-F5-004 | relay hub 离线 502 | P0 | 自动 | 无 hub 连接时请求 | 502 "hub offline" |
| TC-F5-005 | relay token 校验 | P0 | 脚本 | `?token=` 错误连 `/relay/hub/{id}` | 401 "bad token" |
| TC-F5-006 | relay 不落地 | P0 | 脚本 | 全流程后检查 relay 主机磁盘/内存 | 零存储（§9 红线 3）；转发仅内存管道 |
| TC-F5-007 | header 白名单 | P1 | 自动 | 带 Cookie/X-Custom 的请求经 relay | 仅 Authorization + Content-Type 被转发（其余丢弃） |
| TC-F5-008 | body 上限 | P2 | 自动 | >16MB body | 拒绝/截断不崩 |
| TC-F5-009 | hub 断线重连退避 | P1 | 自动 | 杀 relay → hub 日志 | 退避 500ms→8s 指数；relay 恢复后自动重连成功 |
| TC-F5-010 | 30s 转发超时 | P2 | 自动 | hub 收请求不回 | 504 "hub timeout"，wait map 清理不泄漏 |
| TC-F5-011 | TLS 强制 | P1 | 脚本 | `agenthub-relay` 无 `-insecure` 无证书 | 拒绝启动并提示；`-insecure` 仅开发（警告输出） |
| TC-F5-012 | 默认关闭 | P0 | 自动 | 不带 `-relay` 启动 hub | 无任何出站中继连接（红线 1：默认关、显式开） |
| TC-F5-013 | 响应式断点 | P1 | 手工 | 1440/760/390 三档宽度 | ≥1180 检查器常驻；<760 rail 变底部 tab；手机可完成高危审批（完整命令+二次确认，D18） |

### 7.9 事件模型 v0.2 契约（冻结）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-EV-001 | 信封全字段 | P0 | 脚本 | 取任意事件 JSON | `v=2`、`event_id`(uuid 形)、`ts` RFC3339 毫秒 UTC、`agent{kind,version,instance_id}`、`session_id`、`type`、`risk`、`payload.rule_supported` |
| TC-EV-002 | request 侧必填 | P0 | 自动 | permission_request 事件 | 必含 risk/工具名/参数摘要/会话引用/rule_supported（PRD §6） |
| TC-EV-003 | resolved 侧词汇 | P0 | 自动 | 全决策路径 | `rule_scope∈{none,session,always}`、`decided_by∈{hub,policy,timeout,native_gui}`、outcome 词表不出格 |
| TC-EV-004 | status 携带 cwd/origin/parent | P1 | 自动 | status 事件 | C5 字段齐（含 managed parent 关系） |
| TC-EV-005 | task 事件 | P1 | 自动 | 任务生命周期 | type="task" 且 payload 符合 v0.2 加法（向后兼容） |
| TC-EV-006 | WS↔REST 一致 | P2 | 脚本 | 同一事件经 WS 与 /api/events 取回 | 形状 1:1（前端 wsToEvent 转换后与环一致） |

### 7.10 安全专项（SEC）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-SEC-001 | 全端点鉴权遍历 | P0 | 脚本 | 对 § routes 全部 `/api/*`、`/acp/frame`、`/ws` 无 token 各打一次 | 全部 401；豁免仅 `GET /healthz`、`GET /sw.js`、`GET /{$}` |
| TC-SEC-002 | token 三级来源 | P1 | 自动 | flag > env AGENTHUB_TOKEN > 生成文件 依次验证 | 优先级正确；生成 token 写 `<data>/token` |
| TC-SEC-003 | 默认 loopback | P0 | 自动 | 默认启动 | 监听 127.0.0.1（非 0.0.0.0） |
| TC-SEC-004 | 审批 nonce/重放 | P2 | 脚本 | 重放同一 decision 请求 | 幂等或拒绝，不产生双审计行 |
| TC-SEC-005 | webhook 凭据不落盘日志 | P0 | 脚本 | grep 日志 + 前端 bundle | 见 TC-F9-013 |
| TC-SEC-006 | 通知摘要红线 | P0 | 自动 | 检查各通道消息体 | 不含完整命令参数（D17） |
| TC-SEC-007 | fail-closed 全路径 | P0 | 自动 | 枚举门控所有错误分支（网络/超时/坏响应/异常） | 无一分支默认放行 |
| TC-SEC-008 | SQL 注入面 | P2 | 自动 | 规则 pattern/会话 id 带引号注入串 | 参数化查询，无异常；存储原样、匹配按字面 |

### 7.11 数据与保留（DATA / §5.6 D9）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-DATA-001 | 迁移幂等 | P0 | 自动 | 连续启动 3 次 | 0001–0005 各执行一次；重复启动不报错（"no such table" 容错路径） |
| TC-DATA-002 | 9 表齐全 | P0 | 自动 | 检查 sqlite_master | sessions/tool_calls/permission_requests/approval_history/approval_memory/notifications/migrations/rules/tasks（+sqlite_sequence 例外已知） |
| TC-DATA-003 | retention 默认永久 | P0 | 脚本 | 默认启动 → 造旧数据 | `-retention-days` 默认 0 = 不清理（D9 默认永久；回归 19/19 项） |
| TC-DATA-004 | 滚动窗口清理 | P1 | 自动 | `-retention-days 1` + 造 2 天前会话 | 清扫 job 删除超期会话及 FK 级联（tool_calls/permission_requests/notifications）；approval_memory 全局保留；**删除动作留痕**（system 通知行） |
| TC-DATA-005 | WAL 并发写 | P1 | 自动 | 并发决策+事件写入 | WAL+NORMAL+busy_timeout 下无 "database is locked" |
| TC-DATA-006 | 导出两种格式 | P1 | 脚本 | export?format=json / markdown | 内容完整可读（M1 验收含 Markdown 导出项） |

### 7.12 Web 控制台 UI（对照 UI 稿 v0.4）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-UI-001 | 布局对齐 UI 稿 | P0 | 手工 | 对照 `design/UI_UX_v0.1.md` §11/§12 | 暖色 token（#1c1714/#262019/#e0a458…）、三栏结构、六 rail（态势/放行台/会话/事件流/规则/设置）+ 琥珀徽标、statusline |
| TC-UI-002 | 态势大屏 | P0 | 手工 | 落地页 | 4 tile（在线 Agent/活跃会话/待放行/事件总数）真实计数 + roster + 待放行 + 最近事件（Q-U2） |
| TC-UI-003 | 四选项 dec-row | P0 | 手工 | 打开放行台 | `1 放行一次/2 该 session 下放行/3 放行 Always/4 拒绝(复验)` 四行 + 词汇说明；选中卡琥珀描边+左风险色条（high 红/medium 蓝） |
| TC-UI-004 | 复验框行为 | P0 | 手工 | 按 4 → 输入说明 → 确认；再试取消 | 打开不改队列；确认→rejected+note；取消→队列不变 |
| TC-UI-005 | 键盘 | P2 | 手工 | `1/2/3/4`、`j/k`、`Esc`（含输入框聚焦让位） | 全部生效；输入框内 Esc 仅 blur |
| TC-UI-006 | 三语 i18n | P2 | 手工 | 切 zh/EN/JA | UI chrome 全翻（TreeWalker）；**机器值与 Agent 产出不翻**；`tarmac.lang` 持久化；浏览器语言优先 |
| TC-UI-007 | 规则视图即时刷新 | P1 | 手工 | allow_session/always 判决后 | 规则视图**即时**出现（有意优于参考实现的整页重载）；删除按钮生效 |
| TC-UI-008 | WS 重连 | P1 | 手工 | 杀 daemon → 重启 | 前端指数退避 500ms→8s；恢复后自动重连绿点 |
| TC-UI-009 | BOOT 注入 | P1 | 脚本 | `GET /` 查看源码 | 含 `/*__BOOT__*/{token:…}`；页面免粘贴 token |
| TC-UI-010 | 移动端审批 | P1 | 手工 | 390 宽度触发高危 | 强制完整命令展示 + 二次确认（D18）；四选项可用 |
| TC-UI-011 | 重载后待办重建 | P1 | 手工 | 有 pending 时刷新页面 | 从事件环重建放行台待办（不丢卡） |
| TC-UI-012 | 工具调用重放含 requestId | P2 | 手工 | 展开 tool_call 详情 | requestId 字段在（M1.5 修复回归） |

### 7.13 可靠性（REL）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-REL-001 | 重启后 pending 审批恢复 | P0 | 脚本 | 造 pending → kill daemon → 重启 | 放行台从 DB/事件环重建该待办；可正常判决 |
| TC-REL-002 | 运行中任务孤儿回收 | P0 | 自动 | 任务 running 时 kill daemon → 重启 | 任务标 failed "daemon restart during run"；事件 failed；不复活僵尸进程 |
| TC-REL-003 | 规则/记忆持久 | P0 | 自动 | 建 always 规则+记忆 → 重启 | 仍在且生效 |
| TC-REL-004 | WS 订阅恢复 | P2 | 手工 | 重启后前端自动重连 | hello 事件后恢复正常推送（历史测试缺陷回归：先等 hello） |
| TC-REL-005 | 连接器断线 | P1 | 脚本 | 杀假 agent 进程 | 连接器报错退出，hub 会话标 dead/failed，不悬挂 |
| TC-REL-006 | 决策后 waiter 清理 | P1 | 自动 | 同请求重复判决/超时后再判决 | 无 goroutine/channel 泄漏（waiters map 清理） |

### 7.14 性能（PERF）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-PERF-001 | 审批全链路 ≤1s | P0 | 实机 | 真实 CC PreToolUse → hub policy 命中 → 放行 | 端到端 ≤1s（PRD F3 硬指标；M0 PoC 88–107ms 基线） |
| TC-PERF-002 | 通知 ≤3s | P0 | 实机 | 高危触发→群消息到达 | ≤3s（PRD F9 硬指标） |
| TC-PERF-003 | 事件环 500 上限 | P2 | 自动 | 灌 600 事件 | 环稳定 500，内存不涨（滚动丢弃最旧） |
| TC-PERF-004 | 并发压力烟测 | P2 | 脚本 | 10 并发会话 × 20 请求 + 5 任务 | 无死锁；p95 决策 <500ms；WS 不掉线 |

### 7.15 兼容性（COMP）

| ID | 用例 | 优先级 | 类型 | 步骤 | 预期 |
|---|---|---|---|---|---|
| TC-COMP-001 | 浏览器矩阵 | P1 | 手工 | Chrome/Edge/Firefox/Safari（macOS 如可得）走 SMK-04~07 UI 项 | 全部正常；Notification 权限差异仅降级不报错 |
| TC-COMP-002 | iOS Safari / Android Chrome | P1 | 实机 | 经局域网/中继访问 + 审批 | 可用；SW 推送按平台能力降级记录 |
| TC-COMP-003 | Windows 路径 | P1 | 自动 | cwd 含空格/中文（如 `E:\找工作\…`） | 会话/任务/终端跳转全正常 |
| TC-COMP-004 | darwin provider | P2 | 实机 | Mac 真机跑 terminal 跳转 | osascript 打开 Terminal（代码就绪，待真机） |
| TC-COMP-005 | linux provider | P2 | 自动 | provider 单测 mock | x-terminal-emulator 回退链正确 |

---

## 8. 端到端用户旅程（L4）

| ID | 旅程（PRD 依据） | 步骤 | 通过标准 |
|---|---|---|---|
| E2E-01 | **附加模式全链路**（F1–F3） | 启动 hub → `agenthub-connect node fake-agent.mjs` → hub 下发 session/new+prompt → agent 发 request_permission（cargo build, medium）→ 控制台四选项放行（allow_session）→ 同类第二次自动放行 → 回合完成 | agent 收到正确 optionId；全程审计可查；导出档案完整（HANDOFF §2 实机冒烟回归） |
| E2E-02 | **托管模式全链路**（F6 验收句逐字） | 控制台任务视图选 Agent → 派发 → 观察执行（WS）→ 收审批请求（managed CC hub-hook 注入）→ 放行 → 任务 finished | **全程不出 Hub**；任务与会话链接；生命周期事件齐全 |
| E2E-03 | **手机经中继放行高危**（D8+F5） | 部署 relay → hub `-relay` 出站 → 手机浏览器开 `{relay}/{hubID}/` → 触发高危 → 手机收 SW 推送 → 手机上完整命令+二次确认放行 | 放行生效、agent 继续执行；relay 磁盘零痕迹；token 双向校验通过 |
| E2E-04 | **关标签页通知**（M1.5/F9 验收句） | 启用通知+订阅 SW → 关闭全部标签页 → 触发高危 | 系统通知弹出 ≤3s；点击可回到 Hub |
| E2E-05 | **重启恢复**（REL） | 运行中：1 pending + 1 running 任务 + 1 always 规则 → kill -9 daemon → 重启 | pending 恢复可判；任务标 failed；规则仍生效；UI 重连正常 |
| E2E-06 | **多会话并行复用**（F7 验收句逐字） | 同一 Agent 并行 3 session → 各自审批 → 对其一「继续此会话」带新任务 | 三会话独立显示与审批；resume 成功且 cwd 正确 |
| E2E-07 | **数据保留与导出**（§5.6） | 默认启动造数据 → 导出 JSON/MD → `-retention-days 1` 重启 | 默认不清理；导出完整；清理留痕 |

---

## 9. 自动化覆盖映射（现有 Go 测试 ↔ TC）

> 执行 `go test ./...` 即回归；下表保证 L0 不漏测关键路径，也是新增用例的落位指引。

| 测试文件 | 现有用例主题 | 覆盖 TC |
|---|---|---|
| `internal/acpclient/adapter_test.go`（9） | 门控顺序、四选项、两步确认、记忆隔离、审计 | TC-F3-001~007/010~012/101~104/401~403 |
| `internal/acpclient/codex_adapter_test.go`（3） | kind 注册、apply_patch/shell 分类、入站 request_permission | TC-F3-105、TC-F1-003 |
| `internal/acpclient/db_test.go`（8） | 建表/迁移/CRUD | TC-DATA-001/002 |
| `internal/acpclient/db_extra_test.go`（4） | purge 级联、导出 bundle、记忆撤销、通知去重 | TC-DATA-004/006、TC-F3-402、TC-F9-009 |
| `internal/acpclient/risk_test.go`（2） | 风险分级口径 | TC-F3-105 |
| `internal/rules/rules_test.go` | glob/具体度/session 失效 | TC-F3-201~208 |
| `internal/connector/connector_test.go` + `ws_test.go` | spawn/passthrough、能力缺失报错、hub 注册、prompt 完成 | E2E-01、TC-F1-002 |
| `internal/runner/runner_test.go`（3） | 命令构造（--resume/cwd）、单飞、stream 映射 | TC-F7-003~006 |
| `internal/server/server_test.go`（5） | 鉴权、路由、帧分发 | TC-SEC-001/002 |
| `internal/server/hook_test.go`（2） | intake 阻塞判决、deny+note | TC-F3-301/302/401 |
| `internal/server/console_test.go`（3） | 数据端点、两步确认服务端语义 | TC-F3-010~012 |
| `internal/server/m2_test.go`（7） | 记忆撤销等 M2 面 | TC-F3-402 等 |
| `internal/tasks/tasks_test.go`（2） | 全生命周期+暂停阻塞 | TC-F6-001~009 |
| `internal/relay/relay_test.go`（1） | 端到端+401 透传+离线 502 | TC-F5-002~004 |

**缺口（需按 §7 补自动化/脚本）**：TC-F9-003/006/007（通知策略矩阵、410、静默期——部分在包内未系统化）、TC-F6-011（无 hook 拒绝 spawn）、TC-REL-002（孤儿回收已有 recoverOrphans 但无专项断言用例名）、TC-SEC-001 全端点遍历、TC-EV-001 信封字段断言脚本化、TC-PERF-003 环上限。

---

## 10. 已知不可测项与环境阻塞（发布说明需如实披露）

| 项 | 状态 | 恢复条件 |
|---|---|---|
| xterm.js 内嵌终端（M2 F8） | ⛔ 环境阻塞：CDN SSL 被拒，资产无法下载；后端 PTY 端点未写 | 有网环境补齐 xterm 资产 + 实现 PTY 端点后按 TC-F8 新增内嵌终端用例组 |
| macOS 真机 | ⚠️ darwin provider 代码就绪 | 需真机执行 TC-COMP-004 |
| **SW 推送（关标签页）实机** | ⛔ 产品决策改道：移动通知走企微 webhook（已实现，mock 验证）。实测华为浏览器 17.9.2.301（Android 10，非 GMS）对 `http://127.0.0.1` 不给 isSecureContext（设置页 Unsupported）且无 Web Push 服务——前提不成立 | GMS 设备 + 现代 Chrome/Firefox 可恢复（产品仍保留 SW 能力）；日常移动触达走企微/飞书外推 |
| 真实 CC/Codex 全量联调 | 部分依赖外网/账号 | hub-hook 实链路已验证（install→低危秒放→中危阻塞判决 exit 0）；Codex PermissionRequest 全量回归需真实 Codex 环境补跑 |
| `go test -race` | 环境无 CGO | 以 `-count=2` + 显式并发用例补偿 |

**Android 实机已验证（2026-11-07，USB adb + `adb reverse` 隧道）**：控制台渲染、WS 实时、TC-UI-010 高危审批全流程（完整命令+复验框+审计 note）、TC-UI-006 三语、F4 假活标记、F8 三入口——详见 `qa/README.md`。手机rail 徽标在多 tab 残留时显示旧值（手机浏览器展示问题；PC 三处计数一致）。

---

## 11. 测试执行计划（轮次）

| 轮次 | 内容 | 用例集 | 出口 |
|---|---|---|---|
| R1 冒烟 | 环境就绪 + 主干通 | §7.0 全部 | 全 PASS 才进 R2 |
| R2 功能 | 按 F 分组执行 | §7.1–7.9 P0/P1 | P0 100%；S1/S2 清零 |
| R3 专项 | 安全/数据/可靠/性能/兼容 | §7.10–7.15 + §8 旅程 | §4 准出 3–4 条 |
| R4 回归 | 缺陷修复后全量 | §7.0 + 修复关联 + 抽样 P1/P2 | 准出全条 |

**建议节奏**：每轮 R2 按 F3→F6→F7→F9→F5→其余 顺序执行（风险与卖点优先）；每轮结束输出缺陷清单 + 覆盖率表（已执行/通过/阻塞 计数）。

---

## 附录 A：验收脚本清单（`脚本` 类落位）

| 脚本 | 覆盖 | 现状 |
|---|---|---|
| M1 实机验收（python 19 项） | SMK-03、TC-F3-401、TC-DATA-003/006、TC-F8-003、信封全字段等 | ✅ 已有，19/19 PASS |
| M2 批次验收（python 10 项） | sw.js/VAPID/订阅、任务全链路、暂停阻塞、continue 契约 | ✅ 已有，10/10 PASS |
| `qa/scripts/qa_sec_endpoints.py` | TC-SEC-001 全端点 401 遍历 | ✅ 2026-11-07 落地，95/95 ×3 |
| `qa/scripts/qa_notify_matrix.py` | TC-F9-003/007 策略矩阵+静默期+掩码 | ✅ 落地，16/16 ×3 |
| `qa/scripts/qa_envelope.py` | TC-EV-001/003 信封与词汇契约（500 事件全扫） | ✅ 落地，14/14 ×3 |
| `qa/scripts/qa_recovery.py` | TC-REL-001/002/003 + E2E-05（独立 18801 daemon，kill -9） | ✅ 落地，10/10 ×3 |
| `qa/scripts/qa_perf_gate.py` | TC-PERF-001/002/003 计时门 | ✅ 落地，26/26 ×3（p95=24ms） |
| `qa/scripts/qa_queue_e2e.py` | TC-F6-001~013 + F7 shim 断言 | ✅ 落地，14/14 ×3 |

**R2 首轮（2026-11-07）合计：175 项 ×3 连跑 = 525 检查，0 FAIL。** 发现并修复缺陷 ENV-1（事件环 `agent.kind` 戳记不全，违反冻结 schema §6）——详见 `qa/README.md`。环境说明：Windows 回环阵发 RST 属测试装置噪声，脚本已含退避重试；剩余跳过项见 §10（macOS 无设备、xterm CDN 阻塞）。

---

*本文档随实现演进维护：语义变化（词汇表、阈值、路由）必须同步更新对应 TC；新增功能先补 TC 再实现（测试左移）。*
