# qa_sec_endpoints.py - TC-SEC-001/002: every authed route must reject
# missing/bad Bearer tokens; exemptions are only /healthz, /sw.js, / (index).
# Usage: python qa_sec_endpoints.py [base] [data_dir]
import sys
import time

sys.path.insert(0, r"D:/DevTools/AgentHub/qa/scripts")
from common import ROOT, api, check, request, summary, token_for  # noqa: E402

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18799"
DATA = sys.argv[2] if len(sys.argv) > 2 else "qa/data-main"
TOKEN = token_for(DATA)


def probe(m, p, headers=None, body=None):
    """Status probe with escalating backoff: Windows loopback emits bursty
    connection resets under rapid sequential connections; 0.3s/1s/2s cool-downs
    clear them. A status >0 after retries is authoritative."""
    delays = (0, 0.3, 1.0, 2.0)
    st = 0
    for i, d in enumerate(delays):
        if d:
            time.sleep(d)
        if headers is None:
            st, _ = request(m, BASE + p, body=body)
        else:
            st, _ = request(m, BASE + p, body=body, headers=headers)
        if st > 0:
            return st
        if i == len(delays) - 1:
            return st
    return st

# (method, path) — all routes that carry s.auth() in server.go.
ROUTES = [
    ("POST", "/acp/frame"),
    ("POST", "/api/hook/intake"),
    ("POST", "/api/agent-requests"),
    ("POST", "/api/agent-requests/x/response"),
    ("GET", "/api/sessions"),
    ("GET", "/api/sessions/x/export"),
    ("POST", "/api/sessions/x/fork"),
    ("POST", "/api/sessions/x/continue"),
    ("POST", "/api/tasks"),
    ("GET", "/api/tasks"),
    ("POST", "/api/tasks/x/retry"),
    ("POST", "/api/tasks/x/cancel"),
    ("POST", "/api/tasks/queue/pause"),
    ("POST", "/api/tasks/queue/resume"),
    ("POST", "/api/sessions/x/terminal"),
    ("GET", "/api/tool-calls"),
    ("GET", "/api/permission-requests"),
    ("POST", "/api/permission-requests/x/decision"),
    ("GET", "/api/approvals"),
    ("GET", "/api/approval-memory"),
    ("DELETE", "/api/approval-memory/x"),
    ("GET", "/api/notifications"),
    ("GET", "/api/events"),
    ("GET", "/api/agents"),
    ("GET", "/api/rules"),
    ("DELETE", "/api/rules/x"),
    ("GET", "/api/notify/status"),
    ("POST", "/api/notify/test"),
    ("POST", "/api/push/subscribe"),
    ("GET", "/ws"),
]

# 1. No token -> 401 for every authed route.
for m, p in ROUTES:
    st = probe(m, p, body={})
    check(f"no-token 401 {m} {p}", st == 401, f"got {st}")

# 2. Bad token -> 401.
for m, p in ROUTES:
    st = probe(m, p, body={}, headers={"Authorization": "Bearer wrong-token"})
    check(f"bad-token 401 {m} {p}", st == 401, f"got {st}")

# 3. Good token -> must NOT be 401 on any authed route.
# The agent-response probe carries agentKind so the bus "finished" event it
# produces is agent-stamped and won't pollute qa_envelope's contract scan.
BODY_OVERRIDES = {("POST", "/api/agent-requests/x/response"): {"agentKind": "claude_code"}}
for m, p in ROUTES:
    st, _ = api(BASE, TOKEN, m, p, body=BODY_OVERRIDES.get((m, p), {}))
    check(f"good-token not-401 {m} {p}", st != 401, f"got {st}")

# 4. Exemptions: healthz / sw.js / index are reachable WITHOUT a token.
st, _ = request("GET", BASE + "/healthz")
check("exempt GET /healthz (no auth)", st == 200, f"got {st}")
st, _ = request("GET", BASE + "/sw.js")
check("exempt GET /sw.js (no auth)", st == 200, f"got {st}")
st, _ = request("GET", BASE + "/")
check("exempt GET / (no auth)", st == 200, f"got {st}")

# 5. Token file exists in the data dir (level-3 token source).
tok_file = ROOT / DATA / "token"
check("token file exists in data dir", tok_file.exists(), str(tok_file))
check("token file value works", TOKEN == tok_file.read_text(encoding="utf-8").strip())

sys.exit(summary("qa_sec_endpoints"))
