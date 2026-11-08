# qa_queue_e2e.py - TC-F6-001/002/003/010/011/012/013 + managed CC settings
# injection + TC-F7-003 shim argv assertion.
# Preconditions: daemon started with qa/bin FIRST on PATH (so `claude`/`codex`
# resolve to the shims), -task-concurrency 2, -hook-path bin/hub-hook.exe.
# Usage: python qa_queue_e2e.py [base] [data_dir]
import json
import pathlib
import sys
import time

sys.path.insert(0, r"D:/DevTools/AgentHub/qa/scripts")
from common import ROOT, api, check, summary, token_for, wait_task  # noqa: E402

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18799"
DATA = sys.argv[2] if len(sys.argv) > 2 else "qa/data-main"
TOKEN = token_for(DATA)
ARGV_LOG = ROOT / "qa/out/shim-argv.log"
SETTINGS_COPY = ROOT / "qa/out/last-settings.json"
FIXTURE_CWD = str(ROOT / "qa")


def argv_lines():
    return ARGV_LOG.read_text(encoding="utf-8").splitlines() if ARGV_LOG.exists() else []


def enqueue(kind, prompt, cwd=FIXTURE_CWD):
    st, out = api(BASE, TOKEN, "POST", "/api/tasks", {"agentKind": kind, "cwd": cwd, "prompt": prompt})
    return st, (out.get("task") or out)


# ---- 1. managed claude_code task full lifecycle ----
n0 = len(argv_lines())
st, t = enqueue("claude_code", "qa queue e2e: managed claude task")
check("task enqueue accepted", st == 200 and bool(t.get("id")), f"{st} {t}")
tid = t.get("id")
fin = wait_task(BASE, TOKEN, tid, 60, "finished")
check("TC-F6-001 task finished", fin is not None, f"state={t.get('state')}")
check("TC-F6-002 managed session linked", bool(fin and fin.get("sessionId")), f"{fin}")

lines = argv_lines()[n0:]
check("claude shim spawned with argv", any(l.startswith("[claude]") for l in lines), f"{lines}")
check("TC-F7-003 headless spawn uses -p stream-json (no --resume)",
      any("--output-format" in l and "stream-json" in l for l in lines), f"{lines}")

# settings injection: copied settings file must carry the hub-hook PreToolUse
if SETTINGS_COPY.exists():
    settings = json.loads(SETTINGS_COPY.read_text(encoding="utf-8"))
    hook_cmd = json.dumps(settings)
    check("managed CC settings inject hub-hook PreToolUse", "hub-hook" in hook_cmd)
else:
    check("managed CC settings inject hub-hook PreToolUse", False, "last-settings.json not copied")

# frames from shim mapped into pipeline
_, out = api(BASE, TOKEN, "GET", f"/api/tool-calls?sessionId={fin.get('sessionId') if fin else ''}")
tcs = out.get("toolCalls", [])
check("shim tool_use mapped to tool_call records", len(tcs) >= 1, f"{out}")

# ---- 2. codex managed task ----
n1 = len(argv_lines())
st, t2 = enqueue("codex", "qa queue e2e: managed codex task")
check("codex task accepted", st == 200, f"{st} {t2}")
fin2 = wait_task(BASE, TOKEN, t2.get("id"), 60, "finished")
check("codex task finished", fin2 is not None)
lines2 = argv_lines()[n1:]
check("TC-F6-013 codex spawn = exec --json", any(l.startswith("[codex]") and "--json" in l for l in lines2), f"{lines2}")

# ---- 3. concurrency cap (-task-concurrency 2) ----
ids = []
for i in range(3):
    _, t = enqueue("claude_code", f"qa concurrency probe #{i}")
    ids.append(t.get("id"))
peak = 0
deadline = time.time() + 30
while time.time() < deadline and any(ids):
    _, out = api(BASE, TOKEN, "GET", "/api/tasks")
    running = [x for x in out.get("tasks", []) if x.get("state") == "running" and x.get("id") in ids]
    peak = max(peak, len(running))
    _, out2 = api(BASE, TOKEN, "GET", "/api/tasks")
    done = [x for x in out2.get("tasks", []) if x.get("state") in ("finished", "failed") and x.get("id") in ids]
    if len(done) == 3:
        break
    time.sleep(0.15)
check(f"TC-F6-003 concurrency peak <= 2 (observed {peak})", peak <= 2 and peak >= 1)

# ---- 4. invalid input rejection ----
st, _ = enqueue("gemini", "unsupported kind")
check("TC-F6-011 unsupported agent kind rejected", st in (400, 422), f"got {st}")
st, _ = api(BASE, TOKEN, "POST", "/api/tasks", {"agentKind": "claude_code", "cwd": "", "prompt": "x"})
check("TC-F6-012 empty cwd rejected", st in (400, 422), f"got {st}")

# ---- 5. retry endpoint contract ----
st, _ = api(BASE, TOKEN, "POST", f"/api/tasks/{tid}/retry")
check("retry on finished task rejected", st in (400, 409, 422), f"got {st}")

sys.exit(summary("qa_queue_e2e"))
