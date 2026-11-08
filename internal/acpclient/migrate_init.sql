-- SQLite migration script for the AgentHub ACP adapter backend (M1.5).
-- This is migration "0001_initial": it creates the full 7-table schema.
-- The script is embedded via go:embed and applied idempotently by Migrate()
-- on every database open, so no separate migration step is required.

-- 1/7 sessions: Claude Code session metadata and usage rollups
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    cwd TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active',
    turns INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    is_terminal INTEGER NOT NULL DEFAULT 0,
    is_closed INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);

-- 2/7 tool_calls: every tool call observed by the adapter, keyed by the
-- agent's own tool call id (agent_tool_call_id) plus a local autoincrement id
CREATE TABLE IF NOT EXISTS tool_calls (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    agent_tool_call_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    tool_kind TEXT,
    tool_status TEXT NOT NULL DEFAULT 'pending',
    risk TEXT NOT NULL DEFAULT 'medium',
    tool_content TEXT,
    raw_input TEXT,
    raw_output TEXT,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    completed_at INTEGER,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_calls_agent_id ON tool_calls(session_id, agent_tool_call_id);

-- 3/7 permission_requests: approval prompts raised by the adapter (decision B/C)
CREATE TABLE IF NOT EXISTS permission_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_uid TEXT NOT NULL UNIQUE,
    session_id TEXT NOT NULL,
    tool_call_id INTEGER,
    tool_name TEXT,
    risk TEXT,
    command_pattern TEXT,
    reason TEXT,
    outcome TEXT NOT NULL DEFAULT 'pending',
    option_id TEXT,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    decided_at INTEGER,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    FOREIGN KEY (tool_call_id) REFERENCES tool_calls(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_permission_requests_outcome ON permission_requests(outcome);
CREATE INDEX IF NOT EXISTS idx_permission_requests_session ON permission_requests(session_id);

-- 4/7 approval_history: audit trail of every approval decision. System-side
-- gate decisions (approval-memory hits, low-risk pass-through) have no
-- permission_requests row, so permission_request_id is nullable.
CREATE TABLE IF NOT EXISTS approval_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    permission_request_id INTEGER,
    decision TEXT NOT NULL,  -- 'allow' or 'deny'
    decided_at INTEGER NOT NULL DEFAULT (unixepoch()),
    decision_by TEXT NOT NULL DEFAULT 'user',  -- 'user' or 'system'
    notes TEXT,
    FOREIGN KEY (permission_request_id) REFERENCES permission_requests(id) ON DELETE SET NULL
);

-- 5/7 approval_memory: persisted verdicts keyed by (agent_kind, tool, command_pattern)
-- (decision B: permanent verdicts, written only after double confirmation)
CREATE TABLE IF NOT EXISTS approval_memory (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_kind TEXT NOT NULL,
    tool TEXT NOT NULL,
    command_pattern TEXT NOT NULL DEFAULT '*',
    decision TEXT NOT NULL,  -- 'allow' or 'deny'
    scope TEXT NOT NULL DEFAULT 'always',
    confirm_count INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (agent_kind, tool, command_pattern)
);

-- 6/7 notifications: desktop/feishu notification outbox with de-duplication
CREATE TABLE IF NOT EXISTS notifications (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT,
    notification_type TEXT NOT NULL,  -- 'desktop' or 'feishu'
    title TEXT,
    body TEXT,
    sent_at INTEGER NOT NULL DEFAULT (unixepoch()),
    read_at INTEGER,
    tail_hash TEXT,  -- for de-duplication
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

-- 7/7 migrations: tracks applied migrations
CREATE TABLE IF NOT EXISTS migrations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    applied_at INTEGER NOT NULL DEFAULT (unixepoch())
);
