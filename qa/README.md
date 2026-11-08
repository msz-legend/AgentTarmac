# AgentTarmac QA 测试环境（Windows + Android）

> 配套文档：`docs/TEST_PLAN.md`（测试方案与全部 TC 编号）、`qa/CU_TEST_REPORT.md`（2026-11-08 真人操作 computer-use 全路径测试报告：2 个缺陷 CU-001/CU-002 已修复并回归）
> 范围：Windows 全量；Android 手机（局域网/中继、SW 推送）；macOS 部分跳过（无设备）

## R2 首轮执行结果（2026-11-07，全电池 ×3 连跑）

| 脚本 | 覆盖 | 结果 |
|---|---|---|
| qa_sec_endpoints | TC-SEC-001/002 全端点鉴权遍历（30 路由 ×3 态 + 豁免 + token 文件） | **95/95 ×3** |
| qa_envelope | TC-EV-001~005 事件模型 v0.2 契约（500 事件全扫） | **14/14 ×3** |
| qa_queue_e2e | TC-F6-001~013 托管任务全链路（settings 注入/shim argv/并发/参数校验） | **14/14 ×3** |
| qa_notify_matrix | TC-F9-003/007/008 + PERF-002 通知策略矩阵 | **16/16 ×3** |
| qa_perf_gate | TC-PERF-001/002/003（p95=24ms ≤1s；通知 <1s ≤3s；环 500 封顶） | **26/26 ×3** |
| qa_recovery | TC-REL-001/002/003 kill -9 重启恢复（独立 18801 daemon） | **10/10 ×3** |
| **合计** | | **175 项 ×3 = 525 检查，0 FAIL** |

**本轮发现并已修复**：
- **ENV-1（S3→已修）**：事件环 `permission_resolved`/`task`/部分 `permission_request` 事件缺 `agent.kind` 戳记（违反冻结 schema §6「每个信封必带 agent」）。修复：`bridge.SetStore` + `recordUIEvent` 按 session 补戳；`recordPermissionResolved`/`TaskEvent`/`AgentResponse` 显式携带 agentKind；连接器回包带 agentKind。`go test ./...` 全绿后经 qa_envelope 闭环。
- **ENV-2（P1→已修，手机实测发现）**：审批卡/复验框命令显示用的是 `commandPattern`（前两 token），违反 D18「移动端强制完整命令」。修复：`wsToEvent` 改用帧内 `mainArg`（=命令原文）。手机实测：复验框显示完整命令 `rm -rf D:/tmp/qa-phone-target-3` ✓。
- **ENV-3（P2→已修）**：控制台待办只从内存环重建，daemon 重启前的 pending（DB 存活）在放行台不可见。修复：boot 时从 `/api/permission-requests?outcome=pending` 合并。PC 端验证：重启后顶栏/tile/rail 徽标三处一致 3，放行台 3 卡可见（DB 合并路径命令显示 pattern 为已知降级，原文随环事件）。
- 通知正文脱敏口径澄清：IM 摘要含规则 pattern（前两 token，如 `rm -rf`），**不含**命令其余参数——符合 PRD「不含完整命令参数」与参考实现语义（断言已按此校准）。
- 测试装置层（非产品）：Windows 回环快速连发存在阵发 RST——脚本已加退避重试；`/api/permission-requests` 空列表序列化为 `null`——脚本已防御。

## Android 实机测试（2026-11-07，华为 Android 10，USB adb 驱动）

- **接入方式**：路由器 AP 隔离（手机→PC 不通）→ `adb reverse tcp:18799 tcp:18799` USB 隧道，手机访问 `http://127.0.0.1:18799/`（localhost 亦是安全上下文）。
- **通过**：控制台渲染（态势/暖色主题/statusline）；WS 实时（高危触发→待放行 tile/徽标实时 +1）；**TC-UI-010 手机高危审批全流程**（放行台卡片→键盘 4 →复验框完整命令→键入复验说明→确认拒绝→PC 侧审计落库 `outcome=rejected, note=qa-phone-reject-verify`）；TC-UI-006 三语切换（中→EN）；设置页全行渲染；会话卡 stale (no heartbeat) 假活标记（F4）；F8 三按钮（继续/分叉/终端）。
- **环境阻塞（按产品决策改道）**：SW 推送（关标签页可达）在本设备不可测——华为浏览器 17.9.2.301（非 GMS）对 `http://127.0.0.1` 不给 isSecureContext（设置页显示 Unsupported）且无 Web Push 服务提供者。**产品决策：移动通知走企微 webhook 外推通道**（daemon 侧，已实现并经 mock 验证），SW 推送保留为 GMS 设备可选项。手机 rail 徽标在多 tab 残留时显示旧值——手机浏览器多开 tab 的展示问题，PC 端三处计数一致无此问题。
- 驱动备注：华为浏览器对 `input tap` 在网页内容上的 click 合成不可靠（原生 UI/滚动正常），全部页面交互经键盘通道（TAB 焦点 + ENTER 激活 + 数字键判决）完成。

## 端口分配

| 端口 | 用途 |
|---|---|
| 18799 | 主 QA daemon（**0.0.0.0 绑定，手机可达**），数据 `qa/data-main` |
| 18801 | `qa_recovery.py` 专用临时 daemon（仅 127.0.0.1），数据 `qa/data-recovery` |
| 18900 | mock webhook（替身飞书/企微群机器人），日志 `qa/out/webhook.log` |
| 8799 | 默认端口（本环境空闲，留给真实使用） |

## 目录

```
qa/
  fake-agent.mjs        ACP 假 agent（node fake-agent.mjs [low|medium|high]）
  bin/claude.cmd        托管任务 CC shim：记录 argv、拷贝 --settings 文件、输出 stream-json
  bin/codex.cmd         托管任务 Codex shim：记录 argv、输出 --json JSONL
  mock_webhook.py       httptest webhook 接收器（18900）
  scripts/common.py     脚本公共库
  scripts/qa_sec_endpoints.py   TC-SEC-001/002 全端点鉴权遍历
  scripts/qa_notify_matrix.py   TC-F9-003/007/008 + PERF-002 通知策略矩阵
  scripts/qa_envelope.py        TC-EV-* 事件模型 v0.2 契约
  scripts/qa_recovery.py        TC-REL-001/002/003 kill -9 重启恢复（自带 18801 daemon）
  scripts/qa_perf_gate.py       TC-PERF-001/002/003 性能门
  scripts/qa_queue_e2e.py       TC-F6-001~013 + F7 shim 断言（托管任务全链路）
  out/                  运行日志（shim-argv.log / webhook.log / last-settings.json）
  data-main/            主 daemon 数据（token 在 data-main/token）
  data-recovery/        recovery 脚本数据
```

## 启动顺序

```bash
cd /d/DevTools/AgentHub

# 1. mock webhook（后台）
python qa/mock_webhook.py &

# 2. 主 QA daemon（后台；PATH 前置 qa/bin 使 claude/codex 解析到 shim）
PATH="/d/DevTools/AgentHub/qa/bin:$PATH" ./bin/agenthubd.exe \
  -addr 0.0.0.0:18799 -data qa/data-main \
  -feishu-webhook http://127.0.0.1:18900/feishu -feishu-secret qa-secret \
  -wecom-webhook http://127.0.0.1:18900/wecom \
  -task-concurrency 2

# 3. 跑脚本（顺序）
python qa/scripts/qa_sec_endpoints.py
python qa/scripts/qa_envelope.py
python qa/scripts/qa_queue_e2e.py
python qa/scripts/qa_notify_matrix.py
python qa/scripts/qa_perf_gate.py
python qa/scripts/qa_recovery.py        # 独立端口，随时可跑
```

## 已知边界（本环境）

- **真实 claude CLI 未安装**：CC 相关真实联调（真实 PreToolUse、真实 `--resume`）列入人工清单；托管任务/`--settings` 注入/`--resume` 命令构造已由 shim + Go 测试覆盖。
- **xterm.js**：环境阻塞（CDN SSL），跳过。
- **macOS**：无设备，TC-COMP-004 跳过（darwin provider 代码就绪）。
- **SW 真机推送**：需要 Android 手机浏览器实际订阅（人工清单步骤）。

## CLI 接入（Claude Code / OpenCode / Hermes → magpie 路由）

> 分工：**Hub↔CLI 链路由本项目负责**；**CLI→magpie（模型）链路由 magpie 下游解决**。

### 安装位置（D 盘）

| CLI | 位置 | 版本 | 启动面 |
|---|---|---|---|
| Claude Code | `D:\DevTools\npm-global`（npm prefix） | 2.1.319 | headless `claude -p` + PreToolUse hook |
| OpenCode | `D:\DevTools\npm-global\node_modules\opencode-ai\bin\opencode.exe` | 1.19.5 | **`opencode acp`**（ACP server） |
| Hermes | CLI 源码 `D:\Hermes\hermes-agent`（启动器/数据在 `%LOCALAPPDATA%\hermes`，保留登录态） | 0.22.7 | **`hermes acp`**（ACP server，审批四选项原生） |

### magpie 路由（模型链路，下游负责）

- magpie = 本机 `127.0.0.1:3425` 的 OpenAI 兼容路由（另有 Anthropic 兼容 `/v1/messages`）。
- Claude Code：`ANTHROPIC_BASE_URL=http://127.0.0.1:3425` + `ANTHROPIC_AUTH_TOKEN=<dummy>` + `ANTHROPIC_MODEL=group/never-stuck`（已实测单跑回包 `cc-ok`）。
- OpenCode：`~/.config/opencode/opencode.json` 自定义 provider `magpie` + 默认 `model: magpie/group/never-stuck`（已实测单跑回包 `oc-ok`）。
- Hermes：`hermes config set model.base_url http://127.0.0.1:3425/v1` + `model.default group/never-stuck`（已实测 `hermes chat -q` 回包）。
- 已知：magpie 路由组上游偶发 ECONNRESET 502 / 长首包——**由 magpie 侧处理**。

### Hub 对接（本项目职责，已交付）

1. daemon 注册 4 个 kind：claude_code / codex / **opencode** / **hermes**（同一 BaseAdapter，审批/会话/记忆按 kind 隔离）。
2. 连接器补齐 ACP 主机义务：
   - **initialize 握手**（真 agent 拒绝未初始化的 session/new）——单 scanner 结构，杜绝双 reader 分行。
   - **fs/read_text_file**（只读直返，2MB 封顶）。
   - **fs/write_text_file**（写 = 中危 → 走 Hub 审批门控，allow 落盘 / deny 回 JSON-RPC 错误）。
   - **session/new 参数默认化**（opencode Zod 校验要求 mcpServers 数组）。
   - **session/prompt 双形状**（Hub 记账要纯文本，agent 要 ACP 内容块——flattenPromptText 桥接）。
3. 判决 API 多 kind 化：不带 kind 的判决按 DB（request→session→agent_kind）路由到正确适配器（原实现永远落在 claude_code）。
4. e2e：`python qa/scripts/qa_cli_integrate.py`——真实 `opencode acp` / `hermes acp` 经 `agenthub-connect` 挂入 Hub，断言会话注册（origin=attached）、prompt 回合生命周期、事件审计。
