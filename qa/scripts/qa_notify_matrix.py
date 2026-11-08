# qa_notify_matrix.py - TC-F9-003/007 + TC-PERF-002 (notification policy).
# Preconditions: daemon started with
#   -feishu-webhook http://127.0.0.1:18900/feishu -feishu-secret qa-secret
#   -wecom-webhook  http://127.0.0.1:18900/wecom
# and qa/mock_webhook.py running (logs to qa/out/webhook.log).
# Usage: python qa_notify_matrix.py [base] [data_dir]
import pathlib
import sys
import time

sys.path.insert(0, r"D:/DevTools/AgentHub/qa/scripts")
from common import ROOT, api, check, intake_id, resolve_pending, summary, token_for  # noqa: E402

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18799"
DATA = sys.argv[2] if len(sys.argv) > 2 else "qa/data-main"
TOKEN = token_for(DATA)
LOG = ROOT / "qa/out/webhook.log"


def log_len():
    return len(LOG.read_text(encoding="utf-8").splitlines()) if LOG.exists() else 0


def wait_log_ge(mark, want, timeout_s=5.0):
    """Wait until (log_len - mark) >= want. Returns (ok, elapsed_s)."""
    t0 = time.time()
    while time.time() - t0 < timeout_s:
        if log_len() - mark >= want:
            return True, time.time() - t0
        time.sleep(0.1)
    return False, timeout_s


def frame(method, params):
    return api(BASE, TOKEN, "POST", "/acp/frame", {"method": method, "params": params})


epoch = str(int(time.time()))

# ---- 1. high-risk pending -> IM (feishu+wecom) immediate, latency <=3s ----
sess_hi = f"qa-hi-{epoch}"
mark = log_len()
th, box = intake_id(BASE, TOKEN, sess_hi, command="rm -rf D:/tmp/qa-x")
ok, lat = wait_log_ge(mark, 2, 5.0)  # expect feishu + wecom lines
check("high-risk -> feishu delivered", ok, f"only {log_len()-mark} lines in 5s")
check("high-risk -> wecom delivered", ok, "same window")
check("TC-PERF-002 notification latency <=3s", ok and lat <= 3.0, f"{lat:.2f}s")
body = LOG.read_text(encoding="utf-8")
new_lines = "\n".join(body.splitlines()[mark:])
check("IM payload contains session id (summary present)", sess_hi in new_lines)
# PRD D17: 通知不含完整命令参数。规则 pattern（前两 token "rm -rf"）允许出现
# （它是放行/建规则的决策上下文），但命令其余参数必须不出现。
check("notification body has no full command args", "D:/tmp/qa-x" not in new_lines and "rm -rf D:" not in new_lines, "command args leaked")

pid, res = resolve_pending(BASE, TOKEN, sess_hi, "allow_once")
check("high-risk request resolvable", pid is not None and res and res[0] == 200, f"{res}")
th.join(timeout=10)
check("intake got allowed_once verdict", box.get("body", {}).get("outcome") == "allowed_once", f"{box.get('body')}")

# ---- 2. quiet window: same session again within 30s -> suppressed ----
mark2 = log_len()
th2, box2 = intake_id(BASE, TOKEN, sess_hi, command="git push --force origin main")
time.sleep(2.5)
check("quiet window suppresses 2nd high-risk within 30s", log_len() == mark2, f"{log_len()-mark2} unexpected lines")
pid2, res2 = resolve_pending(BASE, TOKEN, sess_hi, "allow_once")
check("suppressed request still resolvable", pid2 is not None and res2 and res2[0] == 200, f"{res2}")
th2.join(timeout=10)
check("2nd intake verdict still delivered to hook", box2.get("body", {}).get("outcome") == "allowed_once", f"{box2.get('body')}")

# ---- 3. medium pending -> desktop only, NO IM ----
sess_md = f"qa-md-{epoch}"
mark3 = log_len()
th3, _ = intake_id(BASE, TOKEN, sess_md, command="cargo build --release")
time.sleep(2.5)
check("medium pending -> no IM push", log_len() == mark3, f"{log_len()-mark3} unexpected lines")
pid3, res3 = resolve_pending(BASE, TOKEN, sess_md, "allow_once")
check("medium request resolvable", pid3 is not None and res3 and res3[0] == 200, f"{res3}")

# ---- 4. session failed -> IM immediate (via inbound session/update relay) ----
sess_f = f"qa-fail-{epoch}"
th4, _ = intake_id(BASE, TOKEN, sess_f, command="echo register-session")
time.sleep(1.0)  # low-risk: passive allow + session auto-registered
st, out = frame("session/update", {"sessionId": sess_f, "status": "failed", "error": "qa-matrix failure"})
check("session/update failed frame accepted", st == 200, f"{st} {out}")
ok4, lat4 = wait_log_ge(mark3, 2, 5.0)
check("session failed -> feishu+wecom delivered", ok4, f"{log_len()-mark3} lines")
check("failed latency <=3s", lat4 <= 3.0, f"{lat4:.2f}s")

# ---- 5. /api/notify/status masks webhook (tail only) ----
_, status = api(BASE, TOKEN, "GET", "/api/notify/status")
raw = str(status)
check("notify status has no full webhook URL", "127.0.0.1:18900" not in raw, f"leaked: {raw[:200]}")

sys.exit(summary("qa_notify_matrix"))
