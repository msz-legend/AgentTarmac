import sqlite3, sys, json
db = sys.argv[1]
c = sqlite3.connect(db)
tables = [r[0] for r in c.execute("select name from sqlite_master where type='table' order by name")]
print("TABLES:", tables)
for t in tables:
    print("COUNT", t, c.execute("select count(*) from " + t).fetchone()[0])
cols = [r[1] for r in c.execute("pragma table_info(sessions)")]
print("SESSION_COLS:", cols)
print("SESSIONS:", c.execute("select id,status,turns,input_tokens,output_tokens,is_terminal,is_closed,origin,cwd,parent_session_id from sessions").fetchall())
print("TASKS:", c.execute("select id,state,retries,max_retries,session_id,error from tasks").fetchall())
print("FK_TASKS:", c.execute("pragma foreign_key_list(tasks)").fetchall())
print("FK_RULES:", c.execute("pragma foreign_key_list(rules)").fetchall())
