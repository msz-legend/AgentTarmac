# qa/scripts/common.py - shared helpers for AgentHub QA acceptance scripts.
import json
import pathlib
import urllib.error
import urllib.request

ROOT = pathlib.Path(r"D:/DevTools/AgentHub")
PASS, FAIL = 0, 0


def check(name, ok, detail=""):
    global PASS, FAIL
    tag = "PASS" if ok else "FAIL"
    print(f"[{tag}] {name}" + (f"  -- {detail}" if detail and not ok else ""))
    if ok:
        PASS += 1
    else:
        FAIL += 1
    return ok


def summary(title):
    print(f"\n=== {title}: {PASS} PASS / {FAIL} FAIL ===")
    return 0 if FAIL == 0 else 1


def token_for(data_dir="qa/data-main"):
    return (ROOT / data_dir / "token").read_text(encoding="utf-8").strip()


def api(base, token, method, path, body=None, timeout=15):
    """Returns (status, parsed_json_or_empty). Non-2xx is returned, not raised.
    One retry on Windows loopback ConnectionReset (transient RST, harness
    artifact — QA scripts fire many rapid sequential connections)."""
    import time as _t
    data = json.dumps(body).encode() if body is not None else None
    for attempt in range(3):
        req = urllib.request.Request(
            base + path, method=method, data=data,
            headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                raw = r.read().decode()
                return r.status, (json.loads(raw) if raw else {})
        except urllib.error.HTTPError as e:
            raw = ""
            try:
                raw = e.read().decode()
            except Exception:
                pass
            try:
                return e.code, json.loads(raw)
            except Exception:
                return e.code, {}
        except ConnectionResetError:
            if attempt < 2:
                _t.sleep(0.3)
                continue
            return 0, {}


def request(method, url, body=None, headers=None, timeout=10):
    """Raw request without auth (for 401 probing). Returns (status, text).
    One retry on Windows loopback ConnectionReset (transient RST under rapid
    connect/abandon cycles — a harness artifact, not a product failure)."""
    import time as _t
    data = json.dumps(body).encode() if body is not None else None
    for attempt in range(2):
        req = urllib.request.Request(url, method=method, data=data, headers=headers or {})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.status, r.read().decode()
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode()
        except ConnectionResetError:
            if attempt == 0:
                _t.sleep(0.2)
                continue
            return 0, "connection reset"


def wait_task(base, token, task_id, timeout_s=60, want="finished"):
    import time
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        _, out = api(base, token, "GET", f"/api/tasks")
        for t in (out.get("tasks") or []):
            if t.get("id") == task_id and t.get("state") == want:
                return t
        time.sleep(0.3)
    return None


def pending_for_session(base, token, session_id, timeout_s=10):
    import time
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        _, out = api(base, token, "GET", "/api/permission-requests?outcome=pending")
        for p in (out.get("permissionRequests") or []):
            if p.get("sessionId") == session_id or p.get("session_id") == session_id:
                return p
        time.sleep(0.2)
    return None


def intake_id(base, token, session_id, tool="Bash", command="", agent_kind="claude_code", timeout=140):
    """Fire one hook intake in a background thread; returns (thread, result_box)."""
    import threading
    box = {}

    def run():
        payload = {
            "session_id": session_id, "tool": tool, "tool_name": tool,
            "args_digest": command, "command": command,
            "cwd": str(ROOT), "agent_kind": agent_kind,
        }
        try:
            st, out = api(base, token, "POST", "/api/hook/intake", payload, timeout=timeout)
            box["status"] = st
            box["body"] = out
        except Exception as e:  # conn reset when daemon dies mid-call = fail-closed
            box["status"] = 0
            box["body"] = {"outcome": "connection_error", "note": f"{type(e).__name__}: {e}"}

    th = threading.Thread(target=run, daemon=True)
    th.start()
    return th, box


def resolve_pending(base, token, session_id, decision, note="", confirm=False, timeout_s=10):
    p = pending_for_session(base, token, session_id, timeout_s)
    if not p:
        return None, None
    pid = p.get("requestId") or p.get("id") or p.get("uid")
    st, out = api(base, token, "POST", f"/api/permission-requests/{pid}/decision",
                  {"decision": decision, "confirm": confirm, "note": note})
    return pid, (st, out)
