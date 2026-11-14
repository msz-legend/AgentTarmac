# AgentHub 三轮点击交互回归报告（2026-11-11）

> 角色：重度 Vibe Coding 用户（键盘 1-4 审批、目录跳转、三语切换、取消/重试、重启恢复）。
> 方法：每轮 = 真实浏览器点击交互发现问题 → 修复 → **真实点击验收**；
> 数据层用 `tools/dbg.py`（API/SQLite/事件流三层核对）与 `fake.log`（agent 侧 stdin/stdout）双重取证。
> 隔离环境：daemon `127.0.0.1:18899`，数据目录 `qa/vibe-trial/data/`，四种 agent 全部走确定性垫片。

## 循环总览

| 轮次 | 发现 | 修复 commit | 点击验收结果 |
| --- | --- | --- | --- |
| 试用（Round 0） | 38 条（`VIBE-TRIAL.md`：A 数据流 10 / B 持久化 14 / C 安全 4 / D 语义 10） | `6b5ea28`（第一批） | — |
| Round 1 | 验收中追加 2 条（cancelled 显示、导出缺列） | `6b5ea28` + `3bc1d73` | ✅ P0 全链/审批/取消/暂停持久化/导出/安全门禁全部达标 |
| Round 2 | 8 条（F1-F5、A6/A7/A10、B12 尾巴） | `3bc1d73` | ✅ 全部达标（详见下） |
| Round 3 | 5 条（终端诚实化、目录跳转、i18n 补全、规则-memory 键不对称、JA/EN 静态标签） | 本批 | ✅ 全部达标 |

## Round 1 验收明细（点击 + 三层核对）

- **P0 全链**（UI 派发 opencode）：`FAKE_LOG` 出现 `initialize → session/new → session/prompt(sessionId=ses_FAKE_0001)`；任务 `finished`；会话行 `completed/turns=1/isTerminal=True`。（A1/A2/A3/A4）
- **高危审批**（UI 放行台点「放行一次」）：`permission_requests.approved/allow_once`、`approval_history` 带 session、任务完成。（A9：迟到判决不再覆盖 completed）
- **取消**（UI 点「取消任务」）：`cancelled / "cancelled by user"`、会话 `closed`、**无自动重跑**。（A5）
- **暂停持久化**（UI 暂停 → 硬杀 daemon → 重启）：`paused=true` 保持，队列未开闸。（B11）
- **tokens**（claude_code）：会话行 `in=12 out=7`——result 行映射生效。（A8）
- **导出**：bundle 含 `tasks/rules/events`；审批含系统放行。（B3/B4）
- **安全**：`?token=` 普通 API 401（C3）；WS 恶意 Origin 403（C1）；terminal-ws 非默认端口可达 handler（C2）。
- **ACP 人判回传**：`FAKE_LOG: PERMISSION_RESPONSE ... outcome selected opt_once`——判决原文到达 agent stdin。

## Round 2 验收明细

- **A7 高危规则闭环**：UI 点「放行 Always」二次确认 → 规则落库 → 同 pattern 高危重派 **pending=0**（tool_call `approved/high`）→ UI 删规则 → 重派 pending 回归。
- **B12 规则↔memory**：铸规则时 memory 双写；删规则联动清 memory（含短 pattern 键的前缀语义）。
- **A6 孤儿同步**：running 任务撞上 daemon 重启 → 任务 `failed/daemon restart during run` **且会话行 `failed/is_terminal=1`**。
- **A10 血缘**：UI「继续此会话」→ 子会话行 `parent_session_id` **落库**（修复了与队列 tryStart 的建行竞态：服务端预建子行 + `EnqueueInSession` 预设 id）。
- **F1**：cancelled 行独立琥珀徽标 + ↻ 重试按钮（点击重试成功）。
- **F2**：`task.event` WS 广播（事件流实时）；status 事件携带 content/plan。
- **F4**：`[perm]` 高危 ACP → 真人放行 → `PERMISSION_RESPONSE` 回传 agent → prompt 完成。
- **F5**：initialize 后 2 条 unsolicited `session/update` 原文进事件流（A2 无丢帧 + A3 映射同时得证）。

## Round 3 验收明细

- **codex tokens**：会话 `in=21 out=9 turns=1 completed`（A8 codex 侧）。
- **终端诚实化**：托管会话卡不再渲染必败按钮，改为灰字提示「Managed session cannot resume directly」；API 直查返回 S17 解释文案。
- **fork 门槛**：fork 行 `origin=fork/awaiting_input`；对 fork 卡点「继续此会话」→ UI 上屏「分叉副本没有接入的 agent，无法继续…」。
- **目录浏览器**：新增路径输入框（Enter 跳转 `D:/DevTools/AgentHub/qa` 秒达）+ 逐级点入叶子目录（P2-7 关闭）。
- **i18n**：EN/JA 的 h1、顶栏统计（Pending/Sessions）、连接指示全部翻译（补 33+ 个缺失点分 key）；JA「全体ステータス / 接続中…」、EN「Board overview / connecting…」。
- **最终冒烟**：UI 派发 opencode → `finished`、会话 `completed/turns=1`；事件库 118 行持久。
- **白屏疑云**：受控复现两次（含杀 daemon 场景）页面均保持 76/69 个按钮——判定为 agent-browser 截图/快照瞬态，非产品缺陷；留观。

## 追加（2026-11-14）：会话回话内容可视化（用户需求）

用户提出「看到回话的具体内容」——数据已在（R2-F5 后 status 事件携带 content 且持久化于 ui_events），缺前端呈现：

- **实现**（`e37bc0e`）：展开会话详情时拉取 `/api/events?session=<id>&limit=500`（持久化、不受内存环限制），把带 content/plan 的 status 事件渲染为「Agent 回话」气泡——**连续 chunk（间隔 <5s）合并为一个 turn**，流式输出读起来是一整段回答；同时渲染「任务描述」块显示完整 prompt（回话的用户半边）。
- **点击验收**：展开 task-961528b2 → 截图 `accept-talk-content.png`：任务描述完整、回话气泡把两条 unsolicited chunk 合并为一turn（"fake agent warming up… / still alive…"）。
- **测试教训**：`.row` 选择器在 board/会话两个视图重名——eval 验收必须用 `#view-sessions .row` 定位（board 行无 data-action，点击是 no-op，非产品缺陷）。

## 追加（2026-11-14）：终端"控制同一个会话"——调研 + 方案 C 落地

- 调研：`docs/SESSION-CONTROL-RESEARCH.md`（L1/L2/L3 语义拆解、CLI 能力实测、A/B/C/D 方案对比）
- **C 落地（903d4d0）**：三 kind 捕获 agent_session_id；continue 对 claude/codex 升级
  `session-resume`（同会话行续轮、--resume agent id、零 child）；adapter 每帧重取 store
  行修复跨回合状态陈旧；会话详情实时 tail；终端提示仅对遗留无 id 行
- **冒烟（全部通过）**：claude 续轮 turns=2/answer 两段；codex 续轮同验；opencode 仍走
  redispatch（回归）；任务台账 ↳ 回答摘要行；no-store 头生效
- **B 决策**：缓行（C 已覆盖用户价值，B 仅省启动开销，生命周期重构风险不划算；触发条件见调研文档）
- 顺手修：handleIndex `Cache-Control: no-store`（浏览器启发式缓存曾导致升级后整页 JS 失效）

## 残留清单（明知未修，非本轮可关闭）

| 项 | 性质 | 说明 |
| --- | --- | --- |
| 企微 846607 | 外部限制 | 官方服务端账号维度限频，独立参考实现同样失败；窗口过后复测 |
| 页面注入完整 token | 架构后续 | WS 鉴权需要；真正解法是短时会话 token（设计已记 HANDOFF） |
| CSP 头 | 硬化后续 | 需先审计内联脚本面 |
| 托管任务原生 resume | 功能后续 | 需捕获 agent 侧真实 session id（当前终端按钮已诚实禁用） |
| `-task-timeout-minutes` 默认 60 | 已文档化 | 0 可关；超时任务落 failed 带明确文案 |

## 复现资产

- 垫片源码：`qa/vibe-trial/shims-src/`（fakeacp 支持 `[perm]`/`[hold]` prompt 标记 + initialize 后主动推 update；fake claude/codex 支持 `[highrisk]`/`[hang]`/`[fail]`）
- 巡检脚本：`qa/vibe-trial/tools/dbg.py`（API + SQLite + 事件流三层）
- 证据：`fake.log`（agent 侧帧记录）、`daemon.log`、`accept-r1/r3-*.png`、`round0-evidence/`
- 生产数据核查：全程未污染（`D:/DevTools/AgentHub/data` 保持 13 会话，试验曾误起旧二进制于生产目录，已核实零写入并记录教训：**daemon 启动必须绝对路径**）

## 测试门禁

- `go build ./...` / `go vet ./...` / `gofmt`（qa 垫片已格式化）/ `go test ./... -count=1`：**8/8 包全绿**
- 新增测试断言：规则↔memory 联动、continue 血缘落库、MatchFirst kind 过滤、导出 session_id
