# AgentHub 重度 Vibe Coding 用户试用报告（2026-11-10 深夜）

> 试用环境：隔离 daemon `127.0.0.1:18899`，独立数据目录 `qa/vibe-trial/data/`，
> 全部四种 agent 走确定性垫片（`shims/` 下 fakeacp / fake claude / fake codex，
> 源码 `shims-src/`，prompt 标记 `[highrisk]`/`[hold]`/`[perm]`/`[fail]`/`[hang]` 驱动场景）。
> 代码基线：`b0dcaf4`（= a01bb01 + m17006 四文件 + QA 证据归档）。
> 方法：每个场景三层核对——API 响应 → SQLite 落库（`tools/dbg.py`）→ 事件流一致性；
> 审批闭环追到 agent 侧伪进程的 stdin/stdout（`fake.log`）。
> 本轮**不重复计入** PM-REVIEW 已覆盖的纯显示问题；全部发现聚焦
> 「操作步骤串联、数据流转、最终数据处理结果正确性」。

## 结论（TL;DR）

托管派发链路（F6/D13）的**任务状态机自身是通的**（queued→running→finished、
暂停/恢复、失败重试、重启孤儿标记全部按设计工作），但**任务与任务之外的一切
数据目标断连**：会话行不落终态、ACP 握手在合规 agent 下 100% 卡死、进度/最终
回答/tokens 三个维度全丢、审批 Always 规则对高危永久死信、取消被降级成 failed
并自动重跑。**用户在 UI 上做出的承诺（放行 Always / 取消 / 暂停）有三件会被
系统静默违背。**共 38 条发现：A 档 10 条数据流断点、B 档 14 条持久化与一致性、
C 档 4 条安全、D 档 10 条体验与语义。

---

## A 档：数据流断点（修复优先级最高）

### A1. ACP 握手在合规 agent 下 100% 永久卡死（含 P0-1，比交接文档记载更严重）
- 现象：派发 opencode 任务，任务 `running` 永不结束；伪 ACP 日志显示 Hub 发完
  `initialize` 后**再无任何帧**。
- 根因：`internal/tasks/acp_task.go:104` `dec.Decode(&resp)` 在等一个永远不会来的
  `session/new` 响应——代码注释写了「2. session/new」但**从未 encode 发出**（P0-1）。
- 为什么上一轮动态复现"通了"？旧 fakeacp 在 initialize 后主动推 2 条
  `session/update`，第二条被这次 `Decode` 误当成"session/new 响应"消费掉，
  程序才侥幸走到 `session/prompt`（且 sessionId 错用 Hub 的 `task-*`）。
  合规 agent（不乱推通知）必卡死。
- 连带：握手无超时看门狗 → 卡死任务**永久占用一个并发槽**（concurrency=2 时
  两个卡死任务即瘫痪整个队列）。
- 修复：显式 encode `session/new`；握手/轮次全程单 reader 泵 + 按 id 关联响应；
  加任务级 watchdog（如 30min 与 F7 对齐）。
- 验证边界：`FAKE_LOG` 出现 `METHOD=session/new`；`session/prompt.sessionId` 为
  `ses_*`；卡死场景有超时落 `failed`。

### A2. json.Decoder 与 bufio.Scanner 混读同一 stdout，静默丢帧（P0-2）
- 根因：`acp_task.go:65` `json.NewDecoder(stdout)` 的预读缓冲吞掉 initialize 响应
  之后已到达的字节，`:129` 又换 `bufio.Scanner` 接管同一管道 → 缓冲内的
  unsolicited `session/update` 全部丢失。
- 证据：旧 fake.log 里 Hub 发出的帧只有 initialize/session/prompt；fake 推的
  2 条 update 在 Hub `/api/events` 无任何痕迹。
- 修复：全生命周期单一 reader（建议逐行 Scanner 统一到 A1 的泵）。

### A3. `session/update` 参数不映射，进度 100% 丢弃（P0-3）
- 根因：`acp_task.go:168` 把 ACP 原生 `{sessionId: ses_*, update:{...}}` 原样
  `Dispatch`；`adapter.go handleSessionUpdate` 只解顶层 `sessionId/status/content`
  → 解析全空 + `unknown session ses_*`（Hub 只有 `task-*` 行）。
- 修复：`handleACPLine` 解 `{sessionId, update}`，把 agent sessionId 映射回 Hub
  task 会话行，翻译成 adapter 语义（agent_message_chunk→content、tool_call→
  tool_call 帧、plan→plan、status 变迁→status）后再 Dispatch。

### A4. 任务终态从不回写会话行（P0-D，四种 agent 全中）
- 现象：6 个 finished 任务 + 1 个 failed 任务的会话行全部
  `status=active/turns=0/tokens=0/is_terminal=false`（SQLite 实证）。
- 根因：`tasks.go:317-321` 任务结束时只 `UpdateTask + emit("finished")`，
  从不 `UpdateSession`；`runner.go mapClaude/mapCodex` 也不映射任何会话级事件。
- 后果：任务板说"完成"，会话卡说"活跃 0 轮"——同一份数据两个结论；
  UI 由此派生「疑似失联」误报（截图 `evidence-ui-3-sessions.png`）。

### A5. 取消 running 任务：落库被跳过 + 自动重跑 + 用户意图被抹掉（P0-5）
- 实测时间线：cancel → 响应仍 `state:"running"` → 任务重跑（events 再次
  queued+started，retries 0/1→1/1）→ 二次取消 → 终态 `failed`，
  error=`session/new response: EOF`。
- 根因：`tasks.go:197-203` running 分支只 `cancel()` 不写库；run goroutine 返回
  `ctx.Err()` 走 `fail()` → `Retries<MaxRetries` 改回 `queued`（`:334`）；
  `:304` 的 `fresh.State==TaskCancelled` 检查因无人写 cancelled 而永不成立。
- 修复：Cancel 时先落库 `cancelled` 再 cancel()；`fail()` 尊重 cancelled 不重跑；
  API 响应返回落库后的状态；错误文案区分「用户取消」与「运行失败」。

### A6. 重启孤儿只修任务不修会话（P0-6）
- 实测：杀 daemon 重启后任务 `failed/daemon restart during run`，
  会话行仍 `active/is_terminal=0/is_closed=0/last_error=''`。
- 根因：`tasks.go:92-105 recoverOrphans` 不触碰 sessions。

### A7. 高危审批的「放行 Always」规则永久死信（新发现，试用 S13b 实测）
- 现象：对高危 `rm -rf` 判 `allow_always`（二次确认后 rule 落库成功），再派发
  同 pattern 高危任务 → **仍然 pending**，规则从未命中。
- 根因：`adapter.go gateToolCallLocked` 第 1 步 `risk==RiskHigh` 直接短路进
  `raisePermissionLocked`，**永远走不到第 2 步规则匹配**。
- 语义矛盾：UI 对高危卡提供 Always 选项 → 落一条注定不生效的规则 = 虚假承诺。
- 修复方向（二选一，建议前者）：规则匹配提前到高危拦截之前（用户显式授予的
  always 优先）；或高危卡隐藏 Always 选项并在文案说明。

### A8. claude / codex 的最终回答与 token 用量全链路丢失（新发现）
- 根因：`runner.go mapClaude` 不映射 `{"type":"result"}` 行（含最终回答 + usage）；
  `mapCodex` 不映射 `item.completed(agent_message)` 与 `turn.completed(usage)`。
- 后果：即便 A4 修了回写，也没有数据可写——turns/tokens 恒 0，最终回答无处可查；
  会话档案缺"结果"这一最关键字段。
- 修复：两处映射补 `session/update`（content + usage + isComplete）。

### A9. 迟到判决覆盖已终结状态（新发现，试用 S6 实测）
- 时间线：tool_result 15:41:16 把 tool_call 写成 `completed`（raw_output/completed_at
  均落库）→ 人 15:41:40 投 `allow_once` → `ResolvePermission` 把 tool_status
  **改回 `approved`**；同刻会话 status 被改回 `running`（任务 15:41:16 已 finished）。
- 根因：判决回写无时序保护，不检查 tool_call/会话当前状态。
- 修复：判决回写前检查当前态，completed/failed/终态会话不可被降级。

### A10. continue 血缘只活在 API 响应里（P1-8，试用实测）
- 实测：`POST /api/sessions/{id}/continue` 响应带 `parent_session_id`，但
  sessions 表两行派生会话的 `parent_session_id` 均为空串；fork 反而落库。
- 后果：会话派生链在库中断链，无法追溯/审计/归档还原。

## B 档：持久化与一致性

### B1. `UpdateSession` 不持久化 cwd/agent_kind/origin/parent_session_id（P1-1）
`store.go:108-116` UPDATE 列表缺这些列，内存改了不落库。

### B2. retention 只删 sessions，tasks/rules 变孤儿（P1-2）
`store.go:503` 仅 `DELETE FROM sessions`；无 FK（`pragma foreign_key_list` 实证
tasks/rules 均无）。会话过期后任务行残留且「查看会话」悬空。

### B3. 会话导出漏掉全部系统放行（P1-3，实测）
导出 bundle 的 approvals 只有 1 条 user 判决；库里 2 条
`permission_request_id=NULL, decision_by=system` 的低危自动放行被
INNER JOIN 排除。审计视角=低危放行历史全部丢失。

### B4. 会话导出缺 tasks/notifications/rules/事件（P1-4，实测）
bundle 只有 `session/toolCalls/permissionRequests/approvals/exportedAt` 五个键。

### B5. 事件流纯内存，重启即蒸发（P1-5，实测）
重启前 30+ 条事件，重启后 `/api/events` 只剩 17 条（全是重启后的）；
库中无 `ui_events` 表。会话档案与事件互证能力归零。

### B6. 通知设置硬编码 `desktop:true, task_completion:true`（P1-6）
`/api/notify/status` 从不反映浏览器授权/localStorage，开关是摆设。

### B7. 任务完成不触发任何通知（P1-7，实测）
6 个 finished/failed 后 `notifications` 表 0 行；`bridge.go TaskEvent` 只
recordUIEvent。飞书/企微 webhook 的高危/failed 推送也接不到任务事件。

### B8. fork 产出 ghost 会话且可继续派生（P1-9+，实测）
fork 返回的会话 `status=active, origin=managed`，无 agent 接管、与真会话不可区分；
对 ghost 再 continue 竟然成功派出新任务（父子全是幽灵）。

### B9. 派发无前置校验（P1-11）
cwd 存在性、二进制可解析性、prompt 长度、幂等键全无；坏任务要走完
spawn 失败 → auto-retry 白跑一轮才 failed。

### B10. Retry 只接受 failed，cancelled 永不可复活（P1-12）
`tasks.go:169`。与 A5 叠加：用户取消的任务既不可重跑也被记成 failed。

### B11. 暂停状态不持久（P1-13，实测）
重启后 `paused=false`，重启前被暂停拦下的 queued 任务立即开跑——
用户"暂停排队"的意图被重启静默撤销。

### B12. 判决只写 rules 不写 approval_memory（P1-14 前半，实测 memory 0 行）
`allow_session/allow_always` 走规则路径；memory 只剩 legacy 语义；
`RemoveRule` 与 memory 无联动清理。

### B13. 规则匹配不分 agent_kind（P1-15）
`rules.MatchFirst` 无 kind 参数，`rules.agent_kind` 仅审计展示——
opencode 的放行规则会自动作用于 claude_code（B/A7 修复后需一并收口）。

### B14. 状态映射缺 `completed→finished`（P1-16）
bridge 只认 isComplete；上游报 `status:completed` 无 isComplete 时会话落 `idle`。

## C 档：安全

### C1. `/ws` 事件端点 WS origin 全放行（P2-2，实测 101）
`server.go:570` `CheckOrigin: func(r) bool { return true }`——交接文档声称的
"CheckOrigin 收窄"只存在于另一个 upgrader。恶意页面可尝试跨站 WS 连接
（配合 C3 的 URL token 进一步放大）。

### C2. terminal-ws 的严格 upgrader 比较对象写错，非默认端口全废（新发现，实测 401）
`console.go:289-299` 拿 **r.Host（服务端自己）** 与写死的 `127.0.0.1:8799` 常量比较：
- 端口≠8799（如 18899）：连合法同源请求也 401——换端口部署=终端功能整体不可用；
- 端口=8799：`r.Host` 恒等于 8799 → **任意 Origin 都放行**，防护形同虚设。
应改为对 `Origin` 头做同源判定。

### C3. `?token=` 查询参数对所有端点生效（P2-3，实测 200）
`server.go bearerToken` 的 WS fallback 无路径限制 → token 进 URL =
进访问日志/代理日志/Referer/浏览器历史。应收窄到 `/ws`（或 terminal-ws）。

### C4. 控制台 HTML 注入完整明文 token（P2-1，实测 grep 命中）
首页源码含完整 64 hex token。任何 XSS/本机文件转储即 token 泄漏。
应掩码 + 按需显示。

## D 档：体验与语义（轻显示，重语义）

### D1. 取消按钮语义混用（P2-1 文档项）`index.html:1062`「拒绝」实为 taskCancel。
### D2. queued 行无条件拼接旧 error 文本（P2-2 文档项）重排队任务显示历史错误。
### D3. 暂停/恢复无 toast 反馈（P2-3 文档项）。
### D4. 「查看会话」不滚动定位（P2-4 文档项）。
### D5. 6 个 h1 缺 en/ja 词条（P2-5 文档项）。
### D6. 运行时错误文本不本地化（P2-6 文档项）。
### D7. 目录浏览器无路径输入/搜索（P2-7 文档项）。
### D8. 终端页无"为何不可用"说明（P2-8 文档项）；且 C2 使非默认端口下全体不可用。
### D9. token 首尾明文展示（P2-9 文档项）。
### D10. 企微限频无退避降级（P2-10 文档项；846607 服务端限制，窗口过后复测）。

## 补充：试用过程顺带发现的小项（计入总数，低优先）

- S1. `POST /api/tasks` 400 错误直接回显 Go json 解析原文（`invalid escape sequence \D`），
  对非开发者用户不友好（且 Windows 反斜杠路径在 curl 场景易踩）。
- S2. `notify/test` 忽略 `channel` 参数：feishu/wecom 未配置时返回同一句
  "no webhook notifier configured"，不回显实际测试的通道（P1-17 关联）。
- S3. 同秒入队任务按 `ORDER BY created_at, id` 字母序调度，FIFO 顺序被打乱
  （store.go QueuedTasks；实测 S19-1 入队最早却最后跑）。
- S4. continue 请求体字段名是 `text` 而非语义自然的 `prompt`，且会话存在性校验
  后置于字段校验（不存在的会话同样报 "text is required"，误导排障）。
- S5. 事件类型学分裂：同一事件流里 `task`/`tool_call`/`tool_result`/`status`/
  `permission_request` 并存，`status` 与 `session/update` 语义重叠（EVENT_MODEL
  v0.2 的 type 词表执行不严格）。
- S6. UI 页脚「待执行 N」实为待**审批**数（与放行台徽标同源），与任务队列的
  待执行概念冲突（截图 evidence-ui-2-tasks.png）。
- S7. `%TEMP%` 残留历史 `agenthub-task-*-settings.json`（实测 2 个旧文件）：
  daemon 崩溃/重启不清扫，recoverOrphans 应按前缀回收。
- S8. 会话卡「疑似失联」徽标对已 finished 任务误报（A4 的 UI 投影，修 A4 即消）。
- S9. `DELETE /api/rules/{id}` 重复删除返回 `unknown rule`（建议 404 语义化，
  附带幂等删除语义可选）。
- S10. `/api/events` 仅 `limit` 无游标/类型过滤，深翻页成本高（B5 落库后一并设计）。

## 修复顺序建议（接手即用）

1. **A1+A2+A3**（`acp_task.go` 单 reader 泵重写握手与会话更新映射）——一个改动面。
2. **A4+A8**（任务终态回写 + claude/codex 结果/usage 映射）。
3. **A5+A6**（取消落库/禁自动重跑/孤儿同步会话）。
4. **A7**（高危规则次序或 UI 收口，需产品拍板，建议规则前置）。
5. **A9**（判决回写时序保护）。
6. **C1+C2+C3+C4**（WS upgrader 统一 + `?token=` 收窄 + token 掩码）。
7. B 档按 B1→B5→B7→B3/B4→B2→B11→B12/B13→B6/B8/B10/B14 推进。
8. D 档随前端顺手清。

## 证据索引

- `qa/vibe-trial/fake.log`——伪 ACP 收发帧记录（A1/A2/A3 直接证据）
- `qa/vibe-trial/daemon.log`——daemon 全程日志
- `qa/vibe-trial/data/agenthub.db`——试用数据库（19 会话/12 任务原样保留）
- `qa/vibe-trial/evidence-ui-{1-board,2-tasks,3-sessions}.png`——UI 投影截图
- `qa/vibe-trial/tools/dbg.py`——三层巡检脚本（可复用）
- `qa/vibe-trial/shims-src/`——四垫片源码（prompt 标记驱动，可复用）
