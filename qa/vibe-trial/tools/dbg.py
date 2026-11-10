#!/usr/bin/env python3
"""Vibe-trial inspector: API + SQLite + event-stream consistency check.

Usage: python dbg.py <port> [--db <path>] [--events N]
"""
import json
import sqlite3
import sys
import urllib.request
from collections import Counter


def get(port, path, token):
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}")
    req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read().decode())


def main():
    port = sys.argv[1]
    root = "D:/DevTools/AgentHub/qa/vibe-trial"
    token = open(f"{root}/data/token", encoding="utf-8").read().strip()
    db_path = f"{root}/data/agenthub.db"
    if "--db" in sys.argv:
        db_path = sys.argv[sys.argv.index("--db") + 1]

    tasks = get(port, "/api/tasks", token).get("tasks") or []
    sessions = get(port, "/api/sessions", token).get("sessions") or []
    events = get(port, "/api/events?limit=200", token).get("events") or []
    pend = get(port, "/api/permission-requests", token)
    pends = pend.get("requests") or pend.get("pending") or []

    print("== TASKS ==")
    for t in tasks:
        print(f"  {t['id']} {t['agentKind']:<11} state={t['state']:<9} retries={t['retries']}/{t['maxRetries']} session={t.get('sessionId','')} err={t.get('error','')[:60]}")
    print("== SESSIONS ==")
    for s in sessions:
        print(f"  {s['id']} {s['agentKind']:<11} status={s['status']:<10} turns={s['turns']} in={s['inputTokens']} out={s['outputTokens']} terminal={s['isTerminal']} closed={s['isClosed']} origin={s['origin']}")
    print("== PERMISSION PENDING ==")
    for p in pends:
        print(" ", json.dumps(p, ensure_ascii=False)[:200])
    print("== EVENTS ==", dict(Counter(e["type"] for e in events)))
    for e in events:
        p = e.get("payload", {})
        brief = p.get("action") or p.get("message", "")
        if e["type"] == "frame.in":
            brief = f"{e.get('method','')} sess={p.get('sessionId','')}"
        print(f"  [{e['ts'][11:23]}] {e['type']:<12} {e.get('method',''):<24} sess={e.get('session_id','') or p.get('session_id','')} {str(brief)[:80]}")

    print("== SQLITE ==", db_path)
    con = sqlite3.connect(db_path)
    try:
        rows = con.execute("select id,state,retries,max_retries,session_id,substr(coalesce(error,''),1,50) from tasks order by created_at").fetchall()
        for r in rows:
            print("  task:", r)
        rows = con.execute("select id,status,turns,input_tokens,output_tokens,is_terminal,is_closed,origin,parent_session_id from sessions order by created_at").fetchall()
        for r in rows:
            print("  sess:", r)
        n = con.execute("select count(*) from tool_calls").fetchone()[0]
        print("  tool_calls:", n)
        n = con.execute("select count(*) from permission_requests").fetchone()[0]
        print("  permission_requests:", n)
        n = con.execute("select count(*) from approval_history").fetchone()[0]
        print("  approval_history:", n)
        rows = con.execute("select tool,scope,effect,created_at from approval_memory limit 10").fetchall()
        for r in rows:
            print("  approval_memory:", r)
        rows = con.execute("select id,agent_kind,tool,scope,effect,session_id from rules").fetchall()
        for r in rows:
            print("  rule:", r)
    finally:
        con.close()


if __name__ == "__main__":
    main()
