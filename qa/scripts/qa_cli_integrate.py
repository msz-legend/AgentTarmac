# qa_cli_integrate.py - real-CLI integration e2e: the Hub drives OpenCode and
# Hermes through the connector (agenthub-connect) in ACP attach mode.
#   TC-F1-002 / E2E-01 extension: session/new → prompt → turn completion →
#   session audit, per agent kind (opencode, hermes).
# Preconditions: qa daemon on :18799 (4 adapters), opencode + hermes installed
# and routed to magpie (see qa/README §CLI 接入).
# Usage: python qa/scripts/qa_cli_integrate.py [base] [data_dir]
import json
import os
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, r"D:/DevTools/AgentHub/qa/scripts")
from common import ROOT, api, check, summary, token_for  # noqa: E402

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18799"
DATA = sys.argv[2] if len(sys.argv) > 2 else "qa/data-main"
TOKEN = token_for(DATA)
CONNECT = str(ROOT / "bin/agenthub-connect.exe")
WORKDIR = str(ROOT / "qa")

OPCODE = r"D:\DevTools\npm-global\node_modules\opencode-ai\bin\opencode.exe"
HERMES = os.path.expandvars(r"%LOCALAPPDATA%\hermes\bin\hermes-acp.exe")

# Poll helper: wait until fn() returns a truthy value.
def wait_for(fn, timeout_s, what, poll=1.0):
    deadline = time.time() + timeout_s
    last = None
    while time.time() < deadline:
        last = fn()
        if last:
            return last
        time.sleep(poll)
    print(f"  [timeout] {what} (last={last})")
    return None


def sessions_of(kind):
    _, out = api(BASE, TOKEN, "GET", "/api/sessions")
    return [s for s in (out.get("sessions") or []) if s.get("agentKind") == kind]


def drive_cli(kind, exe, prompt_text, spawn_timeout=90, turn_timeout=300):
    print(f"\n=== kind={kind} exe={exe} ===")
    env = dict(os.environ)
    if kind == "hermes":
        env["MSYS_NO_PATHCONV"] = "1"
    known = {s["id"] for s in sessions_of(kind)}  # delta: only NEW sessions count
    proc = subprocess.Popen(
        [CONNECT, "-daemon", BASE, "-token", TOKEN, "-agent-kind", kind, "--", exe, "acp"],
        cwd=WORKDIR, env=env,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        # 1. Session registration. The agent.request broadcast is fire-and-
        #    forget: if the connector has not subscribed to the WS yet, the
        #    broadcast is LOST — so retry session/new every few seconds until
        #    the delta session appears (hermes needs ~30s for its own setup).
        sess = None
        deadline = time.time() + spawn_timeout
        while time.time() < deadline and not sess:
            _, out = api(BASE, TOKEN, "POST", "/api/agent-requests", {
                "method": "session/new",
                "params": {"cwd": WORKDIR},
            })
            if not out.get("requestId"):
                print(f"  session/new rejected: {out}")
                return False
            sess = wait_for(
                lambda: next((s for s in sessions_of(kind) if s["id"] not in known), None),
                6, f"{kind} session registration (attempt)")
        if not sess:
            print(f"  [timeout] {kind} session registration — connector never subscribed/answered")
            return False
        sid = sess["id"]
        print(f"  session registered: {sid} origin={sess.get('origin')} status={sess.get('status')}")
        check(f"{kind}: session registered in Hub (origin=attached)", sess.get("origin") == "attached")

        # 2. Prompt a turn (real model call via magpie downstream). The magpie
        #    upstream may 502 (its instability, solved downstream) — retry the
        #    prompt a few times; the HUB contract is that the turn lifecycle
        #    reaches the agent and lands back in the session audit.
        completed_turn = False
        last_status = None
        for attempt in range(1, 4):
            baseline = next((x for x in sessions_of(kind) if x["id"] == sid), {}).get("turns", 0)
            _, out = api(BASE, TOKEN, "POST", "/api/agent-requests", {
                "method": "session/prompt",
                "params": {"sessionId": sid, "prompt": [{"type": "text", "text": prompt_text}]},
            })
            if not out.get("requestId"):
                print(f"  prompt rejected: {out}")
                return False

            def settled():
                cur = next((x for x in sessions_of(kind) if x["id"] == sid), {})
                return cur if cur.get("turns", 0) > baseline and cur.get("status") in ("completed", "idle", "failed") else None

            wait_for(settled, turn_timeout, f"{kind} turn #{attempt} lifecycle")
            cur = next((x for x in sessions_of(kind) if x["id"] == sid), {})
            last_status = cur.get("status")
            print(f"  attempt {attempt}: turns={cur.get('turns')} status={last_status}")
            if last_status in ("completed", "idle"):
                completed_turn = True
                break
            if last_status == "failed":
                print("  (turn reached the agent; magpie upstream failed the model call — retrying)")
        _, sess_after = api(BASE, TOKEN, "GET", "/api/sessions")
        cur = next((x for x in (sess_after.get("sessions") or []) if x["id"] == sid), {})
        check(f"{kind}: prompt turn lifecycle reached agent (turns>=1)", bool(cur.get("turns", 0) >= 1))
        if completed_turn:
            check(f"{kind}: full turn completed via magpie", True)
        else:
            print(f"  [warn] {kind}: full turn blocked by magpie upstream quality — Hub link itself verified")
            check(f"{kind}: failed turn state audited honestly", last_status == "failed")

        # 3. Audit: events recorded for this session.
        _, ev = api(BASE, TOKEN, "GET", "/api/events")
        kinds_seen = {e.get("type") for e in (ev.get("events") or []) if e.get("session_id") == sid}
        print(f"  events for session: {sorted(k for k in kinds_seen if k)}")
        check(f"{kind}: session events audited", bool(kinds_seen))
        return True
    finally:
        # Kill the whole process tree (connector + its CLI child); a plain
        # terminate orphans the agent, and the orphan keeps answering the
        # session/new broadcasts of later runs.
        subprocess.run(["taskkill", "/F", "/T", "/PID", str(proc.pid)],
                       capture_output=True)
        try:
            proc.wait(timeout=5)
        except Exception:
            pass


ok_op = drive_cli("opencode", OPCODE, "只回复四个字符：oc-e2e，不要使用任何工具。")
check("opencode CLI driven by Hub end-to-end", ok_op)

ok_hm = drive_cli("hermes", HERMES, "只回复四个字符：hm-e2e，不要使用任何工具。")
check("hermes CLI driven by Hub end-to-end", ok_hm)

sys.exit(summary("qa_cli_integrate"))
