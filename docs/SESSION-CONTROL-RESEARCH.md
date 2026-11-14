# 终端"控制同一个会话"实现方案调研（2026-11-14）

> 背景问题：终端功能无法查看实际信息——托管任务运行中，终端页对托管会话返回 409（S17 诚实门槛），
> 就算放开也只会 `--resume` 另起一个进程，既看不到运行中 agent 的实时输出，也不是"同一个会话"的接管。
> 本文基于本机 CLI 实测（非文档推测），给出"控制同一个会话"的语义拆解、方案对比与推荐路线。

## 1. "控制同一个会话"的三层语义（先拆清楚再谈方案）

| 层 | 语义 | 现状 |
| --- | --- | --- |
| L1 同一份对话历史 | 进程可以换，上下文延续（claude `--resume` / codex `resume` 语义） | attached 会话已支持；托管会话缺 agent_session_id（claude/codex 路径未捕获，ACP 路径已有） |
| L2 同一个进程实例 | 实时观察/驱动**正在运行**的那个 agent 进程 | 不可能通过 `--resume` 实现——那是新进程；需要常驻进程（见方案 B） |
| L3 实时查看输出 | 运行中看到 agent 在干什么 | **数据已经在 Hub 手里**：headless 进程的 stdout（stream-json / ACP 帧）被 Hub 全量消费并持久化（ui_events + answer 列）。查看=渲染问题，不是数据问题 |

关键洞察：L3 不需要接管 stdout（Hub 已经在读，接管反而打架），只需要**把已消费的事件实时转显到终端页**。

## 2. 本机 CLI 能力实测（2026-11-14，claude/codex/opencode 均为本机安装版）

### claude —— 双向流式多轮（方案 B 的地基，官方支持）
```
--input-format <format>   Input format (only works with --print): "text"(默认) | "stream-json"(realtime streaming input)
--replay-user-messages    Re-emit user messages from stdin back on stdout (only works with --input-format=stream-json ...)
```
即 `claude -p --output-format stream-json --input-format stream-json --verbose` =
**进程常驻 + stdin 写入 user 轮次 + stdout 持续出结构化事件**。每轮输入形如
`{"type":"user","message":{"role":"user","content":[...]}}`。
- 与现有 F6 管线完全兼容：事件映射（mapClaude）不变、PreToolUse hub-hook 审批不变（hooks 按调用触发，与输入模式无关——需实测确认一次）、并发模型不变。
- 相比 `claude -p`（one-shot），唯一的语义变化：**进程退出 ≠ 任务结束**，"回合结束"以 result 行为界。

### codex —— 两条路径
```
codex exec resume <id> --json    # 一次性续轮：新进程，但加载同一会话历史，JSONL 事件输出
codex app-server                 # [experimental] JSON-RPC 常驻服务（附 generate-ts 绑定 / JSON Schema）
codex exec fork <id>             # 分叉会话
```
- 轻路径：每轮 `exec resume <agent_session_id> --json`（L1 语义，改动极小）。
- 重路径：`app-server` 常驻 JSON-RPC（L2 语义，等于再写一个 codex 适配器；实验期协议有变动风险）。

### opencode —— 已是常驻形态
`opencode acp` 是 ACP server：`session/prompt` 一轮一请求，**进程在回合间保活**（我们的 fakeacp 主动退出是假象；
真实 ACP server 常驻）。另有 `opencode serve`（headless server）+ `attach`。
即 ACP 两个 kind 天然具备 L2，只差任务层语义（现在 streamACP 把 prompt 结果当任务终结，进程随后被收割）。

### internal/pty —— ConPTY 桥已就绪但与本需求无关
`pty.NewSession/Write/Read/Resize/Close` 完整可用（terminal-ws 在用）。注意：方案 C/B 都**不需要 PTY**——
结构化 stdio 比 TTY 拼图干净得多；PTY 只在"真要原生 REPL"时才用。

## 3. 方案对比

### 方案 A：全部 agent 改为 PTY 托管交互进程
把 headless 换成 ConPTY 起交互 REPL，终端页直连 PTY。
- ✅ 最"原生"的 L2。
- ❌ 破坏三支柱：交互 TUI（ink/ANSI）无法可靠解析回结构化事件 → 审批闭环/任务板/usage 全丢；
  `stream-json` 事件流在 REPL 模式不存在；并发队列变成多 PTY 管理。
- ❌ 结论：**否决**作为主方案（PTY 仅保留给"用户真的要敲原生 REPL"的边缘场景）。

### 方案 B：常驻结构化 stdio（推荐终态）
所有 kind 的 agent 进程**常驻**，Hub 持有结构化管道，多轮 = 向同一进程发下一轮：
- claude_code：`-p --output-format stream-json --input-format stream-json`（官方双向流式）✅ 地基已验证
- opencode/hermes：已是 ACP 常驻，把"prompt 返回=任务结束"改为"回合结束、进程保活"（acp_task.go 收割逻辑延后 + 空闲回收）
- codex：`app-server` JSON-RPC（实验期，协议需冻结观察）或先走轻路径过渡
- 终端页：**事件直播 + 受控输入框**（输入=下一轮 user turn；输出=该 session 的事件流实时渲染到 xterm/日志面板）
- ✅ 真 L2；审批/队列/usage 全保留；Windows 不需要 PTY
- ⚠️ 改动面：tasks.go 生命周期（回合结束 vs 进程结束、空闲回收、并发槽按"回合"计）、continue 从"重派发"改为"复用进程续轮"、进程崩溃恢复
- 风险点：`--input-format stream-json` 与 hub-hook 审批的兼容性实测；codex app-server 协议稳定性

### 方案 C：事件直播 + 受控续轮（推荐第一步，改动最小）
不做进程接管，把终端页改造成该会话的**直播/回放视图**：
1. **补数据**：mapClaude 捕获 `system/init` 行的 `session_id`、mapCodex 捕获其 JSONL 中的 session id（若 JSONL 不带，从 `~/.codex/sessions` 或 exec resume 输出侧证）→ 三种 kind 的 agent_session_id 齐备（ACP 已有）。
2. **终端页对托管会话**：去掉 409，渲染"会话直播"：
   - 实时：复用现有 `/ws`（frame.in / frame.out / task.event / permission_* 全部已广播），按 session_id 过滤渲染（xterm 或日志面板）
   - 回放：boot 拉取 `/api/events?session=<id>`（已有持久化，含 answer/content）
   - 输入：「继续对话」= POST continue；claude_code/codex 的 continue 从"重派发新任务"升级为
     `claude --resume <agent_session_id> -p --output-format stream-json "<用户输入>"` 一轮追加（同 cwd、同会话历史，事件继续流入同一会话行）；ACP kind 若进程仍活，直接 session/prompt。
3. ✅ 立刻解决 L3（查看实际信息）+ L1（历史延续）；不碰审批/队列
- ⚠️ L2 缺席：运行中的那个进程不能被"打字"，只能看（它本来也只在等回合结果）

### 方案 D：外部终端复用（tmux 类）
Windows 无原生 tmux；Hub 自己管理多 PTY 等价于方案 A。否决。

## 4. 推荐路线

**第一阶段（C，前端为主 + 两处小后端）——已于 2026-11-14 实施完成（903d4d0）**：
- ✅ runner 捕获（mapClaude init / mapCodex session_configured → agentSessionId 帧）+ adapter 落 agent_session_id
- ✅ continue 升级 session-resume：同会话行续轮（--resume agent id），turns/answer 累积，零 child
- ✅ 附带修复：adapter 内存会话状态跨回合陈旧（每帧重取 store 行）——续轮 turns=2/answer 两段实测通过
- ✅ 会话详情实时 tail（WS 事件追加 eventsBySession）+ 终端提示仅对无 id 遗留行
- [ ] runner.go：mapClaude 捕获 init 行 session_id → 复用 A8 的 session/update 通道带 agentSessionId；mapCodex 同调研
- [ ] terminalUnavailable 拆分：有 agent_session_id 的托管会话不再 409，改为直播视图；无 id 的保留诚实提示
- [ ] 终端页（或会话详情）：实时事件流（WS 过滤）+ 历史回放（/api/events?session=）+ 「继续对话」输入框（走 continue，claude/codex 用 --resume 续轮）
- [ ] 验收：派发任务 → 终端页实时看到 tool_call/回话流 → 结束后输入下一轮 → 同一会话行 turns+1、answer 追加

**第二阶段（B，claude_code 先行）**：
- [ ] streamACP/spawnTask 改常驻：claude `--input-format stream-json`；实测 hub-hook 审批兼容性
- [ ] 任务语义：回合结束=result 行（state 保持 running→idle），进程保活 + N 分钟空闲回收 + 崩溃自动 fail
- [ ] continue 复用常驻进程（不再新起）；并发槽改按"活动回合"计数
- [ ] codex 视 app-server 协议稳定度决定接入时机

**明确不做**：PTY 托管交互 REPL 作为主链路（A）；外部 tmux（D）。

## 5. 与现有修复的关系

- S17 的 409 是当时正确的诚实门槛（Hub 合成 id 确实不可 resume）；本路线落地后，它的提示对象收缩为
  "无 agent_session_id 的遗留会话"，有 id 的托管会话直接进直播视图。
- R8 的 agent_session_id 列/迁移继续沿用；C 阶段补齐 claude/codex 捕获后即三 kind 齐备。
- ui_events（B5）+ answer（0009）已经是直播视图的数据底座，无需新表。
