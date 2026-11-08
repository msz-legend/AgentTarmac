# qa_perf_gate.py - TC-PERF-001 (policy auto-allow <=1s), TC-PERF-002
# (high-risk notification <=3s), TC-PERF-003 (event ring cap 500).
# Usage: python qa_perf_gate.py [base] [data_dir]
import statistics
import sys
import time

sys.path.insert(0, r"D:/DevTools/AgentHub/qa/scripts")
from common import ROOT, api, check, intake_id, resolve_pending, summary, token_for  # noqa: E402

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18799"
DATA = sys.argv[2] if len(sys.argv) > 2 else "qa/data-main"
TOKEN = token_for(DATA)
LOG = ROOT / "qa/out/webhook.log"

epoch = str(int(time.time()))
RULE_CMD = "echo qa-perf-gate"

# ---- setup: allow_always rule for the perf command (idempotent) ----
sess = f"qa-perf-{epoch}"
th, box = intake_id(BASE, TOKEN, sess, command=RULE_CMD)
th.join(timeout=15)
if box.get("body", {}).get("outcome") == "allowed_once":
    check("perf rule already exists (persisted from prior run)", True)
else:
    resolve_pending(BASE, TOKEN, sess, "allow_always", confirm=False)
    res = resolve_pending(BASE, TOKEN, sess, "allow_always", confirm=True)
    check("perf rule created (allow_always)", res and res[0] == 200, f"{res}")

# ---- TC-PERF-001: N policy auto-allows, latency each <=1s, p95 reported ----
N = 20
lat = []
for i in range(N):
    t0 = time.time()
    th, box = intake_id(BASE, TOKEN, f"{sess}-{i}", command=RULE_CMD)
    th.join(timeout=15)
    dt = time.time() - t0
    ok = box.get("body", {}).get("outcome") == "allowed_once"
    check(f"policy auto-allow #{i+1}", ok, f"{box.get('body')}")
    lat.append(dt)
p95 = statistics.quantiles(lat, n=20)[-1] if N >= 20 else max(lat)
check(f"TC-PERF-001 p95 <=1s over {N} auto-allows (p95={p95*1000:.0f}ms)", p95 <= 1.0,
      f"max={max(lat)*1000:.0f}ms")

# ---- TC-PERF-002: high-risk -> IM latency <=3s ----
sess_hi = f"qa-perf-hi-{epoch}"
mark = len(LOG.read_text(encoding="utf-8").splitlines()) if LOG.exists() else 0
t0 = time.time()
th_hi, box_hi = intake_id(BASE, TOKEN, sess_hi, command="rm -rf D:/tmp/qa-perf-x")
seen = False
while time.time() - t0 < 5.0:
    if LOG.exists() and len(LOG.read_text(encoding="utf-8").splitlines()) - mark >= 2:
        break
    time.sleep(0.05)
lat_notify = time.time() - t0
check("TC-PERF-002 notify latency <=3s", lat_notify <= 3.0, f"{lat_notify:.2f}s")
pid, res = resolve_pending(BASE, TOKEN, sess_hi, "allow_once")
check("perf-gate high-risk resolvable", pid is not None, f"{res}")

# ---- TC-PERF-003: event ring capped at 500 ----
ring_sess = f"qa-ring-{epoch}"
st, out = api(BASE, TOKEN, "POST", "/acp/frame",
              {"method": "session/new", "params": {"sessionId": ring_sess, "cwd": str(ROOT)}})
check("ring session registered", st == 200, f"{st} {out}")
for i in range(600):
    api(BASE, TOKEN, "POST", "/acp/frame",
        {"method": "session/update", "params": {"sessionId": ring_sess, "status": "running"}},
        timeout=5)
_, out = api(BASE, TOKEN, "GET", "/api/events")
evs = out.get("events", out.get("items", []))
check(f"TC-PERF-003 event ring capped (got {len(evs)})", len(evs) <= 500)

sys.exit(summary("qa_perf_gate"))
