#!/usr/bin/env python3
"""R7-1: send 50KB prompt to /api/tasks via urllib (avoid shell arg limits)."""
import json, urllib.request, sys

token = open("/d/DevTools/AgentHub/qa/vibe-trial/data/token", encoding="utf-8").read().strip()
body = json.dumps({
    "agentKind": "claude_code",
    "cwd": "D:/DevTools/AgentHub/qa/vibe-trial/work",
    "prompt": "x" * 50000,
}).encode()
req = urllib.request.Request(
    "http://127.0.0.1:18899/api/tasks",
    data=body,
    headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
    method="POST",
)
try:
    with urllib.request.urlopen(req, timeout=10) as r:
        print("STATUS", r.status, r.read().decode()[:200])
except urllib.error.HTTPError as e:
    print("HTTP", e.code, e.read().decode()[:200])
except Exception as e:
    print("ERR", type(e).__name__, e)
