# qa_recovery.py - TC-REL-001/002/003 + E2E-05 (daemon kill -9 recovery).
# Spawns its OWN daemon on port 18801 with data dir qa/data-recovery so it can
# kill and restart it without touching the interactive 18799 instance.
# Usage: python qa_recovery.py
import json
import sqlite3
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, r"D:/DevTools/AgentHub/qa/scripts")
from common import ROOT, api, check, intake_id, resolve_pending, summary  # noqa: E402

PORT = 18801
BASE = f"http://127.0.0.1:{PORT}"
DATA = ROOT / "qa/data-recovery"
EXE = ROOT / "bin/agenthubd.exe"


def wait_health(timeout_s=20):
    t0 = time.time()
    while time.time() - t0 < timeout_s:
        try:
            with urllib.request.urlopen(BASE + "/healthz", timeout=2) as r:
                if r.status == 200:
                    return True
        except Exception:
            time.sleep(0.3)
    return False


def port_listener_pid():
    """Returns the PID listening on PORT (0 if none) via netstat."""
    raw = subprocess.run(["netstat", "-ano"], capture_output=True).stdout or b""
    out = raw.decode("utf-8", "ignore")  # netstat on zh-CN Windows is GBK
    for line in out.splitlines():
        if f":{PORT}" in line and "LISTENING" in line:
            return int(line.split()[-1])
    return 0


def kill_port():
    """Heal zombies: kill whatever still listens on PORT (prior crashed run)."""
    pid = port_listener_pid()
    if pid:
        subprocess.run(["taskkill", "/F", "/PID", str(pid)], capture_output=True)
        time.sleep(0.8)


def start():
    kill_port()
    proc = subprocess.Popen(
        [str(EXE), "-addr", f"127.0.0.1:{PORT}", "-data", str(DATA)],
        cwd=str(ROOT), stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        creationflags=subprocess.CREATE_NEW_PROCESS_GROUP,
    )
    assert wait_health(), "daemon did not become healthy"
    return proc


def kill9(proc):
    subprocess.run(["taskkill", "/F", "/PID", str(proc.pid)], capture_output=True)
    proc.wait(timeout=10)
    # The kill must actually free the port before the next phase; otherwise a
    # zombie would answer healthz and poison later phases (seen live).
    t0 = time.time()
    while time.time() - t0 < 10:
        if port_listener_pid() == 0:
            break
        subprocess.run(["taskkill", "/F", "/PID", str(port_listener_pid())], capture_output=True)
        time.sleep(0.5)
    assert port_listener_pid() == 0, "port 18801 still held after kill"


epoch = str(int(time.time()))
sess = f"qa-rec-{epoch}"

# Deterministic runs: this script owns its daemon AND its data dir.
import shutil
shutil.rmtree(DATA, ignore_errors=True)

# ---- Phase A: pending request + persistent rule, then kill -9 ----
proc = start()
TOKEN = (DATA / "token").read_text(encoding="utf-8").strip()

th, box = intake_id(BASE, TOKEN, sess, command="cargo build --release")
pid, res = resolve_pending(BASE, TOKEN, sess, "allow_always", confirm=False)
check("allow_always first click requiresConfirmation", res and res[1].get("requiresConfirmation") is True, f"{res}")
pid, res = resolve_pending(BASE, TOKEN, sess, "allow_always", confirm=True)
check("allow_always second click lands", res and res[0] == 200, f"{res}")
_, rules = api(BASE, TOKEN, "GET", "/api/rules")
rule_count = len(rules.get("rules") or [])
check("rule visible before restart", rule_count >= 1, f"{rules}")

# A SECOND request stays UNRESOLVED — this is the one that must survive the kill.
# Its caller uses a short client deadline: across the kill the caller must end
# in a FAILURE (RST or timeout) — never a verdict from the dead daemon.
sess_pend = f"qa-rec-pend-{epoch}"
th_pend, box_pend = intake_id(BASE, TOKEN, sess_pend, command="git push --force origin main", timeout=8)
import time as _t
_t.sleep(1.0)  # let it register as pending (high-risk: blocked awaiting human)

kill9(proc)  # sess_pend's request is still unanswered in DB (the intake thread dies with it)

# ---- Phase B: pre-seed an orphan 'running' task row into SQLite ----
db = DATA / "agenthub.db"
con = sqlite3.connect(str(db))
con.execute(
    "INSERT INTO tasks (id, agent_kind, cwd, prompt, state, max_retries) VALUES (?,?,?,?,?,?)",
    (f"task_orphan{epoch[-4:]}", "claude_code", str(ROOT), "orphan probe", "running", 1),
)
con.commit()
con.close()

# ---- Phase C: restart -> pending recoverable, orphan failed, rule alive ----
proc = start()
TOKEN = (DATA / "token").read_text(encoding="utf-8").strip()  # same token file persists

pend = None
_, out = api(BASE, TOKEN, "GET", "/api/permission-requests?outcome=pending")
for p in (out.get("permissionRequests") or []):
    if p.get("sessionId") == sess_pend or p.get("session_id") == sess_pend:
        pend = p
check("TC-REL-001 pending approval survives restart", pend is not None, f"{out}")

pid = (pend.get("requestId") or pend.get("id") or pend.get("uid")) if pend else None
st, out = api(BASE, TOKEN, "POST", f"/api/permission-requests/{pid}/decision", {"decision": "allow_once"})
check("recovered pending resolvable", st == 200, f"{st} {out}")
# The ORIGINAL blocking caller's TCP connection died with the kill -9 (fail-
# closed is the only possible outcome for it); what matters is that it FAILED
# FAST instead of hanging for the full 2-minute intake timeout.
th_pend.join(timeout=15)
body_pend = box_pend.get("body") or {}
check("pre-kill caller failed fast (fail-closed, no 2min hang)",
      not th_pend.is_alive() and body_pend.get("outcome") in ("connection_error", "unavailable"),
      f"status={box_pend.get('status')} body={body_pend}")

_, tasks = api(BASE, TOKEN, "GET", "/api/tasks")
orphan = [t for t in (tasks.get("tasks") or []) if t.get("prompt") == "orphan probe"]
check("TC-REL-002 orphan running task -> failed", orphan and orphan[0].get("state") == "failed",
      f"{orphan[:1]}")
check("orphan error mentions daemon restart", orphan and "restart" in (orphan[0].get("error") or ""),
      f"{orphan[:1]}")

_, rules = api(BASE, TOKEN, "GET", "/api/rules")
check("TC-REL-003 allow_always rule survives restart", len(rules.get("rules") or []) >= rule_count,
      f"before={rule_count} after={len(rules.get('rules', []))}")

# rule still effective: new intake same command auto-allows via policy
th2, box2 = intake_id(BASE, TOKEN, sess + "-post", command="cargo build --release")
th2.join(timeout=10)
check("policy rule still effective after restart", box2.get("body", {}).get("outcome") == "allowed_once",
      f"{box2.get('body')}")

# cleanup
kill9(proc)
sys.exit(summary("qa_recovery"))
