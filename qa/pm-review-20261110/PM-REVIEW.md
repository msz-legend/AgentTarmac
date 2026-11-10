# AgentHub 资深 PM 体验复核报告（2026-11-10）

> 本报告是对 2026-11-09 那份「12 条优化清单」的**推倒重做**。上一版把已在 git 提交里修好的问题（移动端导航、xterm CDN）当成现场观察写进清单，用户质疑「你确定你通过模拟真人点击体验过了么」成立。本次全部结论只认**当前版本的现场证据**，每条优化标注证据类型与代码定位，并单列「已修复、明确剔除」章节。

## 1. 版本基线（可复现）

| 项 | 值 |
| --- | --- |
| git HEAD | `a01bb01 docs(license): unify repository under MIT, retire Open Core decision H` |
| 工作树（未提交） | `M cmd/agenthubd/main.go`、`M internal/server/web/index.html`、`M internal/tasks/tasks.go`、`?? internal/tasks/acp_task.go` —— 即 m17006「派发下拉加 opencode/hermes」的改动，**尚未 commit**，但 daemon 已用这份代码编译运行 |
| daemon | `v0.2.0-m2`，PID 3592，`D:\DevTools\start-agenttarmac.ps1` 启动，`-addr 127.0.0.1:8799 -data D:\DevTools\AgentHub\data -retention-days 90 -task-concurrency 2` |
| 健康检查 | `{"agentKinds":["claude_code","codex","opencode","hermes"],"status":"ok"}` |
| 浏览器 | `http://127.0.0.1:8799/?v=pm-review-20261110`，视口 1264×805，Asia/Shanghai |
| 复核时间 | 2026-11-10 21:20–22:00（+08:00） |
| 证据目录 | `qa/pm-review-20261110/`（12 张 PNG，逐视图 + 关键状态） |

复核开始时 8799 拒绝连接（隔夜 daemon 已停），用启动脚本拉起后继续。**未修改任何产品代码**，只做真实点击与取证；复核产生的交互状态（语言、通知开关、暂停、规则、待放行）已全部复原。

## 2. 真人点击覆盖矩阵

全部由 ego-browser `locator.click()` / `keyboard.press()` / `selectOption()` 触发，非 `page.evaluate` 直接调函数。

| # | 视图 / 动作 | 结果 | 证据 |
| --- | --- | --- | --- |
| 1 | 8 个视图逐个点开并截图 | 8/8 渲染，无 JS 致命错误 | `01-board.png`、`02-desk.png`、`view-{sessions,tasks,stream,rules,terminal,settings}.png` |
| 2 | 全局态势：Agent roster、最近事件 | 在线 4 / 活跃 2 / 总会话 9 / 待放行 0 / 事件 6 | `01-board.png` |
| 3 | 放行台空态 | 「放行队列已清空 · 新请求会即时出现在这里」 | `02-desk.png` |
| 4 | 构造 4 条真实 `session/request_permission`，点「拒绝」→ 填复验说明 → 确认 | 弹窗展示命令原文，确认后队列清空 | `03-desk-pending.png` + 点击日志 |
| 5 | 点「放行 Always」两次 | 第一次 toast「需要二次确认」，第二次落规则 | 点击日志 + `04-rules-with-data.png` |
| 6 | 键盘 `2`（该 session 下放行）、`1`（放行一次） | 各消费 1 条请求，队列归零；规则页出现 `allow·session` | `04-rules-with-data.png` |
| 7 | 规则页点「删除」 | 规则从 2 条减到 1 条，随后经 API 清至 0 条 | 点击日志 |
| 8 | 三语往返 `[data-lang-set]` en → ja → zh-CN | 按钮/下拉/语言标记全部正确还原（无损 i18n 生效） | 点击日志 |
| 9 | 设置页「任务完成通知」关 → 开 → 关 | 状态切换生效并复原 | 点击日志 + `06-final-restored.png` |
| 10 | 设置页「发送测试」 | 抛出企微 846607 原始英文错误 | `05-settings-en.png` + 点击日志 |
| 11 | 任务页「暂停派发」/「恢复派发」 | 仅 `.on` class 变化，无 toast | 点击日志 |
| 12 | 「选择目录…」→ 下钻 `AgentHub` → 「使用此目录」 | cwd 回填 `D:\DevTools`，最近路径写入 1 条 | 点击日志 |
| 13 | 最近路径下拉选择 | 回填 `#task-cwd` | 点击日志 |
| 14 | 「查看会话」链接 | 切到会话视图，但目标卡片不高亮、不滚动 | 点击日志（`highlighted:false, scrollY:0, targetRectTop:466`） |
| 15 | 会话卡「继续此会话」→ 输入框聚焦 → 取消 | 弹层展开、textarea 自动聚焦、取消收起 | 点击日志 |
| 16 | 任务台账「拒绝」（实为取消）、「↻」重试 | toast「已取消」/「重试已入队」，但行状态不回写 | 点击日志 |
| 17 | 会话卡展开详情 | 显示 `origin=managed · tokens 0/0` 与事件时间线 | 点击日志 |

**逐视图可交互元素采样**：sessions 22 buttons / 1 input；tasks 12 buttons / 5 inputs；stream **0** buttons / **0** inputs；rules 空态 **0** buttons / **0** inputs；terminal 4 buttons / 2 inputs；settings 6 buttons / 0 inputs。

> 截图里 `DeepSeek · ●任务` 之类的黑色浮层是 ego/DSH 宿主的操作提示叠加层，**不是 AgentHub UI 缺陷**，不计入优化项。

## 3. 已修复、明确剔除（不再冒充现场发现）

| 旧清单条目 | 修复提交 | 本轮复核佐证 |
| --- | --- | --- |
| 移动端 8 tab 拥挤、横向溢出 | `a20dc39` / `82a3b59` | 桌面 1264×805 导航完整；历史安卓 1080×2280 截图已入仓 |
| xterm CDN 在本机 SSL 被拒、favicon 404 | `a20dc39` / `82a3b59` / `baef4a0` | `view-terminal.png` 黑色终端区正常渲染（本地 vendor 生效） |
| 三语切回中文无法还原 | `29d3d0a` | en → ja → zh-CN 往返，按钮与下拉全部正确还原 |
| 派发工作目录无原生选取框 / 无最近路径记忆 | `29d3d0a` | 目录浏览器真实下钻 + 最近路径回填全通过 |
| 派发下拉只有 Claude Code / Codex | m17006（未提交） | healthz 四种 agentKind；`#task-kind` 四个 option 齐全 |

## 4. 优化清单（14 条，全部来自本轮现场证据）

优先级口径：**P0** = 会让用户误判系统状态或做错决策；**P1** = 明显降低效率/可信度；**P2** = 体验打磨与长期可维护性。

### P0-1 「拒绝」按钮名不副实，且取消后状态不回写
- **现象**：任务台账每条 running 任务右侧的按钮文案是「拒绝」，但 `data-action="taskCancel"`（`index.html:1062`）——它是"取消任务"，与放行台的「拒绝」（`reject_once`）语义完全冲突。真机点击后 toast 显示「已取消」，但刷新前后该行仍是 `running`、仍带「拒绝」按钮，用户无法确认取消是否生效。
- **证据**：现场截图 `view-tasks.png` + 真实点击日志（`afterCancel.firstRows` 状态未变）+ 源码 `internal/server/web/index.html:1062`。
- **影响**：高危场景（误派发删除命令）下用户以为已拒绝，实际任务仍在跑。
- **建议**：文案改「取消任务」并加二次确认；`taskCancel` 成功后立刻把行状态置 `cancelled` 或重新拉取 `/api/tasks`，失败要给明确错误而不是「已取消」。

### P0-2 任务状态与错误文本自相矛盾，跨天僵尸任务不清理
- **现象**：`task_4628f029` 状态 `running`，同一行错误文本却是 `initialize response: EOF`；`task_ea0c1087` / `task_795f87ba` 状态 `failed` 配 `daemon restart during run`；会话视图里这些会话又全部显示 `idle`。两条 11-09 21:23 启动的 running 任务到 11-10 21:58 仍是 running。
- **证据**：`view-tasks.png`、`view-sessions.png`、DOM 文本采样；`taskRowHTML` 把 `task.error` 无条件拼进第二行（`index.html:1068`）。
- **影响**：台账不可信，用户无法据此判断要不要重试；重启后的状态修复逻辑不一致（部分标 failed、部分留 running）。
- **建议**：daemon 启动时统一把孤儿 `running` 标记为 `interrupted` 并写明原因；`error` 只在终态（failed/cancelled）展示；状态与错误互斥。

### P0-3 通知通道 UI 与后端实际通道错配，原始错误整串抛给用户
- **现象**：设置页写「飞书 webhook 未配置」+「发送测试」，但 `/api/notify/status` 返回 `feishu:false, wecom:true, wecom_tail:"0IMo-0"` —— 实际生效的是**企微智能机器人长连接**。点击测试，toast 直接抛出服务端原文：`wecom aibot send: errcode 846607: aibot send msg frequency limit exceeded, hint: [...], from ip: 120.244.160.86, more info at https://...`，含 hint、出口 IP、外链，未本地化、未脱敏、无处置建议。
- **证据**：`05-settings-en.png`、`view-settings.png`、真实点击日志、API 响应 `{"desktop":true,"feishu":false,...,"wecom":true,"wecom_tail":"0IMo-0"}`；源码 `internal/server/web/index.html:559-565`、`:1447-1454`。
- **影响**：用户以为在测飞书，失败信息却指向企微；内部网络信息直接暴露在截图里。
- **建议**：设置页按 `/api/notify/status` 动态渲染已启用通道名称与尾号；错误码映射成本地化短文案（如「企微发送受限，请稍后重试」），hint/IP 只进日志；通道分开测试。

### P0-4 企微 846607 限频无退避、无降级、无提示
- **现象**：11-09 07:00 与 11-10 21:31 两次测试均 `846607 frequency limit exceeded`，间隔远超 30 条/分钟窗口，说明是账号/小时级限频。产品侧既不做指数退避，也不降级到飞书/桌面/Service Worker，每次点击都把同一条英文错误甩给用户。
- **证据**：`D:\DevTools\wecom-aibot-diff.mjs` 独立 Node+`ws` 参考实现同凭据同样 846607（已排除 Go 实现与 chat_type）；本轮两次真实点击。
- **影响**：告警通道在真正需要时静默失效，且用户无从判断是配置错还是限频。
- **建议**：`WecomAibotNotifier` 增加限频状态机（收到 846607 后进入静默窗口，UI 显示剩余时间）、失败时按 `feishu → push → desktop` 顺序降级，并在设置页显示「上次发送失败原因 + 下次可重试时间」。

### P1-5 六个视图标题缺 en/ja 词条，三语形同半残
- **现象**：8 个 `<h1 data-i18n>` 中只有 `tasks.title`、`term.title` 在 `DICT.en` / `DICT.ja` 有词条；`board.title`、`desk.title`、`sessions.title`、`stream.title`、`rules.title`、`settings.title` 全部缺。切到 English 后 h1 仍是「设置」「会话」「全局态势」，而同页按钮已变成 `Pause dispatch` / `Dispatch →`。
- **证据**：`05-settings-en.png`、真实点击日志 `headingsEn`（8 个 h1 全中文）、`dictKeys`（6 个 key 无 en/ja）+ 源码 `index.html:329,366,377,478,489,500` 对比 `:752,772,776`。
- **建议**：补齐 6×2 词条，并在 `captureI18NSource()` 里加一条开发期断言：所有 `data-i18n` key 必须在三语字典中存在，缺则 console.warn，防止再漏。

### P1-6 运行时数据完全不参与本地化
- **现象**：切到 English 后，任务错误仍是 `initialize response: EOF` / `daemon restart during run` / `session/prompt: Invalid params: session not found: task-7a766059`；设置页 `90 days`、`已开启 · tab`、`v0.2.0-m2` 中英混排。中文界面下这些后端错误本来也就是英文。
- **证据**：真实点击日志 `taskErrorsEn` + `view-settings.png`。
- **建议**：定义错误码枚举（`ERR_ACP_EOF` / `ERR_DAEMON_RESTART` / `ERR_SESSION_NOT_FOUND`），daemon 返回结构化 code + 参数，前端三语映射；`90 days` / `tab` 走词条。

### P1-7 暂停/恢复派发没有可见反馈
- **现象**：点「暂停派发」与「恢复派发」后，按钮文字恒为「暂停派发」/「恢复派发」，仅 `.on` class 变化（颜色极接近），无 toast、无队列状态文案。真机点击日志里 `toasts:[]`、`disabled:false`、文字不变。
- **证据**：真实点击日志 `pauseOn`/`resumeOn`（只有 class 变化）+ `view-tasks.png`。
- **建议**：顶部状态条显示「派发已暂停 · N 个任务排队中」，暂停期间派发按钮禁用并说明原因。

### P1-8 「查看会话」不定位目标会话
- **现象**：台账点「查看会话」只做 `setView("sessions")`，目标卡片 `getBoundingClientRect().top = 466`（需滚动），`highlighted:false`、`scrollY:0`。9 张卡片长得几乎一样，用户得自己找 `task-4628f029`。
- **证据**：真实点击日志 `openSession`。
- **建议**：跳转后 `scrollIntoView({block:"center"})` + 目标卡片高亮 2s，或直接展开该会话详情。

### P1-9 会话卡片长 prompt 硬截断，无展开/复制
- **现象**：`task-4628f029 · 回归测试：OpenCode 托管派发验证，请只回复"OpenCode 派发已验◇…` —— 60 字符截断且出现 U+FFFD 替换字符（`◇`），没有 tooltip、展开或复制按钮。
- **证据**：`view-sessions.png`、DOM 文本采样 `sessionActions`；源码 `index.html:1252` 附近 `String(prompt).slice(0, 60)`。
- **建议**：按码点安全截断（`Array.from(s).slice()` 或 `Intl.Segmenter`），加 `title` 全文 + 点击展开 + 一键复制。

### P1-10 「继续此会话」对不可用会话无差别开放
- **现象**：9 张会话卡（含 2 条标红「疑似失联」、多条 failed 任务会话）全部带「继续此会话」「分叉继续」，无禁用态。已知契约：attached 的 `opencode`/`hermes` 会话 `/continue` 返回 `409 {"error":"该会话不支持原生续跑（仅 attached 的 claude_code/codex）"}`，用户输入完内容点发送才被告知失败。
- **证据**：`view-sessions.png` + 真实点击日志（弹层展开、textarea 聚焦）+ 既有 `three-client-contract.mjs` 45/45 契约结论。
- **建议**：按 `origin` + `agentKind` 预计算可续跑能力，不支持的按钮置灰并给 tooltip；或在弹层顶部预告本次将走的路径（`task-redispatch` / `acp_prompt` / `native-resume`）。

### P1-11 规则页只能被动产生，主动管理能力为零
- **现象**：空态 0 buttons / 0 inputs，只有「暂无数据」；说明写着「会写成审批记忆」，但没有任何手工新建、编辑、停用入口。有规则时仅一个「删除」，删除后无 toast。
- **证据**：`view-rules.png`、逐视图采样、真实点击日志 `ruleDelete`（`toasts:[]`）。
- **建议**：加「新建规则」表单（agentKind / 工具名 / 命令模式 / scope / 到期时间）、停用开关、删除确认与 toast、按 agent 筛选。

### P1-12 事件流是纯只读列表，与「持久审计在 SQLite」的承诺脱节
- **现象**：0 buttons / 0 inputs，无筛选、无搜索、无导出、无分页；正文只渲染 `S.events.slice(-12)`，页面文案却写「持久审计在 SQLite」。
- **证据**：`view-stream.png`、逐视图采样、源码 `index.html:1197`。
- **建议**：按 agentKind / 事件类型 / 风险等级 / 时间范围筛选，支持关键词搜索与 CSV 导出，接 `/api/events` 的历史分页。

### P2-13 目录浏览器（Web 降级）缺搜索与路径输入
- **现象**：点「选择目录…」平铺 40+ 个目录 + 4 个盘符，共 42 个按钮、**0 个 input**；深路径只能逐级点，无法粘贴绝对路径，无搜索/收藏。
- **证据**：真实点击日志 `fsModal`（`D:\DevTools` → 下钻 `D:\DevTools\AgentHub` 成功）+ `inputs: []`。
- **建议**：加「输入绝对路径」框（服务端校验 `/api/fs/list`）、目录名过滤、常用目录收藏置顶。

### P2-14 终端能力与派发能力脱节，token 展示无防护
- **现象**：终端页注明「仅 claude_code / codex 支持」，会话下拉默认 `—`，而派发已支持 opencode/hermes（m17006）——用户从 opencode 会话点进终端是死路，且没有一句提示。设置页 token 以 `04f5551bfd…390147` 首尾明文展示，文案写「不要分享给他人」，却无隐藏/复制/轮换按钮。
- **证据**：`view-terminal.png`、`view-settings.png`；源码 `index.html:1841`。
- **建议**：终端页按 agentKind 明示支持范围并给替代动作（「在系统终端打开」）；token 默认全遮蔽，点击显示需二次确认，提供复制与轮换入口（轮换需 human gate）。

## 5. 复核产生的残留（已披露，未擅自清库）

本轮为验证审批链路，向运行中的 daemon 写入了 4 条 `pm-review-live-*` 会话、4 条权限请求、若干事件，并触发过 1 次取消、1 次重试。**交互状态已全部复原**：待放行 0 条、规则 0 条、派发已恢复、语言回 zh-CN、任务完成通知回「关」、页面回到全局态势（`06-final-restored.png`）。

历史会话/事件记录**保留未删**——「测试完清空全部记录」是 m15556 的独立指令，本轮未重新授权，不擅自扩大目标。用户之前的归档流程在 `D:\DevTools\_archive\agenttarmac-20261109-0730\`，需要归零时按同一流程操作即可。

## 6. 遗留边界（非本轮可解）

| 项 | 状态 |
| --- | --- |
| 企微 846607 限频 | 官方账号维度服务端限频，独立 Node 参考实现同样失败，与 AgentHub 代码无关；等窗口过后可拿 `{"channel":"wecom","ok":true}` |
| Codex 受限沙箱真机闭环 | `D:\DevTools\codex-elevated-verify.mjs` 已修 4 处，需用户在**管理员 PowerShell** 运行（本机非提升令牌缺 `SeAssignPrimaryTokenPrivilege`） |
| m17006 改动未提交 | 4 个文件已验证可用但仍在 `git status` 未提交区，需单独 commit |
| M3 开源运营 | 用户明确「M3 先不管，按照 MIT 做」；纯 MIT 统一已由 `a01bb01` 完成 |
