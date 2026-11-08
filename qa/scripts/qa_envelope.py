# qa_envelope.py - TC-EV-001/002/003/005: EVENT_MODEL v0.2 frozen contract.
# Usage: python qa_envelope.py [base] [data_dir]
import re
import sys
import time

sys.path.insert(0, r"D:/DevTools/AgentHub/qa/scripts")
from common import ROOT, api, check, intake_id, resolve_pending, summary, token_for, wait_task  # noqa: E402

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18799"
DATA = sys.argv[2] if len(sys.argv) > 2 else "qa/data-main"
TOKEN = token_for(DATA)

TYPES = {"message", "tool_call", "tool_result", "permission_request",
         "permission_resolved", "status", "cost", "error", "task"}
RISKS = {"low", "medium", "high"}
SCOPES = {"none", "session", "always"}
DECIDERS = {"hub", "policy", "timeout", "native_gui", ""}
OUTCOMES = {"allowed_once", "rejected", "cancelled", "unavailable"}
TS_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$")

epoch = str(int(time.time()))


def events():
    _, out = api(BASE, TOKEN, "GET", "/api/events")
    return out.get("events", out.get("items", []))


# ---- generate one of every interesting event shape ----
sess = f"qa-ev-{epoch}"
th, box = intake_id(BASE, TOKEN, sess, tool="Write", command="", agent_kind="claude_code")
# Write without file_path -> file tools take file_path; give digest instead:
# force medium via a Bash write-like command is simpler; keep Write pending.
th2, box2 = intake_id(BASE, TOKEN, sess + "-md", command="cargo build --release")
pid, res = resolve_pending(BASE, TOKEN, sess + "-md", "allow_session")
check("allow_session decision accepted", pid and res and res[0] == 200, f"{res}")
pid2, res2 = resolve_pending(BASE, TOKEN, sess, "reject_once", note="qa-envelope note")
check("reject decision accepted", pid2 and res2 and res2[0] == 200, f"{res2}")
th.join(timeout=15)
th2.join(timeout=15)

# policy auto-allow: same session same command after allow_session
th3, box3 = intake_id(BASE, TOKEN, sess + "-md", command="cargo build --release")
th3.join(timeout=10)
check("policy auto-allow after allow_session", box3.get("body", {}).get("outcome") == "allowed_once", f"{box3.get('body')}")

# task event
_, t = api(BASE, TOKEN, "POST", "/api/tasks",
           {"agentKind": "claude_code", "cwd": str(ROOT / "qa"), "prompt": "qa envelope task"})
tid = t.get("task", {}).get("id") or t.get("id")
check("task created", bool(tid), f"{t}")
fin = wait_task(BASE, TOKEN, tid, 60, "finished") if tid else None
check("task finished (shim spawn)", fin is not None, f"task {tid} not finished in 60s")

evs = events()
check("event ring non-empty", len(evs) > 0)

# ---- envelope contract on every event ----
# agent required on ALL events (frozen schema §6); risk required on
# tool_call / permission_request (UIEvent tag: 必填 for those two).
bad = []
for e in evs:
    why = []
    if e.get("v") != 2:
        why.append(f"v={e.get('v')}")
    if not e.get("event_id"):
        why.append("no event_id")
    ts = e.get("ts") or e.get("TS") or ""
    if isinstance(ts, str) and not TS_RE.match(ts):
        why.append(f"ts={ts!r}")
    agent = e.get("agent") or e.get("Agent") or {}
    if not isinstance(agent, dict) or not agent.get("kind"):
        why.append("agent.kind missing")
    if e.get("type") not in TYPES:
        why.append(f"type={e.get('type')}")
    if e.get("type") in ("tool_call", "permission_request") and e.get("risk") not in RISKS:
        why.append(f"risk={e.get('risk')}")
    if why:
        bad.append(f"{e.get('type')}: {why}")
check(f"envelope contract on {len(evs)} events", not bad, "; ".join(bad[:5]))

# ---- permission_request carries rule_supported + risk + command summary ----
pr = [e for e in evs if e.get("type") == "permission_request"]
check("permission_request events present", len(pr) > 0)
bad_pr = [e for e in pr if "rule_supported" not in (e.get("payload") or {})]
check("permission_request.payload.rule_supported present", not bad_pr, f"{len(bad_pr)} missing")
cmd_seen = [e for e in pr if (e.get("payload") or {}).get("command")]
check("permission_request carries command原文", len(cmd_seen) > 0)

# ---- resolved vocabulary ----
res_ev = [e for e in evs if e.get("type") == "permission_resolved"]
check("permission_resolved events present", len(res_ev) > 0)
bad_res = []
for e in res_ev:
    p = e.get("payload") or {}
    if p.get("rule_scope") not in SCOPES:
        bad_res.append(f"scope={p.get('rule_scope')}")
    if p.get("decided_by", p.get("decidedBy")) not in DECIDERS:
        bad_res.append(f"decided_by={p.get('decided_by')}")
    if p.get("outcome") not in OUTCOMES:
        bad_res.append(f"outcome={p.get('outcome')}")
check("resolved vocabulary within frozen wordlists", not bad_res, "; ".join(bad_res[:5]))
note_ev = [e for e in res_ev if (e.get("payload") or {}).get("note")]
check("rejected note carried in resolved payload", len(note_ev) > 0)

# ---- task events ----
task_ev = [e for e in evs if e.get("type") == "task"]
check("task events present (queued/started/finished)", len(task_ev) >= 3, f"{len(task_ev)}")

sys.exit(summary("qa_envelope"))
