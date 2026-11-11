#!/usr/bin/env python3
"""R7-2: special characters prompt end-to-end via codex."""
import json, urllib.request

token = open("data/token", encoding="utf-8").read().strip()
cases = [
    ("zh", "接受 R7 中文提示：列出当前目录并打印你好"),
    ("newline", "echo line1\nline2\nline3"),
    ("quotes", 'echo "double" and \'single\''),
    ("windows_path", 'type D:\\DevTools\\AgentHub\\qa\\vibe-trial\\work'),
]
for name, p in cases:
    body = json.dumps({"agentKind": "codex", "cwd": "D:/DevTools/AgentHub/qa/vibe-trial/work", "prompt": p}).encode()
    req = urllib.request.Request(
        "http://127.0.0.1:18899/api/tasks", data=body,
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            print(name, "->", r.status, r.read().decode()[:60])
    except urllib.error.HTTPError as e:
        print(name, "->", e.code, e.read().decode()[:120])
