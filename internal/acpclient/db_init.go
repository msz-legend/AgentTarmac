package acpclient

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	// Pure-Go SQLite driver: the daemon builds without a C toolchain
	// (CGO_ENABLED=0), which matters for the Windows-first distribution.
	_ "modernc.org/sqlite"
)

//go:embed migrate_init.sql
var migrateInitSQL string

// registeredMigrations is the ordered list of schema migrations applied to
// every database on open. Each migration runs at most once; application is
// tracked in the migrations table.
var registeredMigrations = []struct {
	Name string
	SQL  string
}{
	{"0001_initial", migrateInitSQL},
	// 0002 adds the agent_kind column the 会话 view needs (UI 稿 §2.1: one
	// card per session shows the agent type). ALTER works both on fresh
	// databases (0001 just created the table without it) and on databases
	// created before this migration existed.
	{"0002_session_agent_kind", `ALTER TABLE sessions ADD COLUMN agent_kind TEXT NOT NULL DEFAULT 'claude_code'`},
	// 0003 creates the policy rule table (PRD F3 / EVENT_MODEL v0.2 §3.1:
	// rule granularity = tool name + argument pattern, M1 must ship it).
	{"0003_policy_rules", `
CREATE TABLE IF NOT EXISTS rules (
    id TEXT PRIMARY KEY,
    agent_kind TEXT NOT NULL DEFAULT '',
    tool TEXT NOT NULL,
    arg_pattern TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL,
    effect TEXT NOT NULL DEFAULT 'allow',
    session_id TEXT NOT NULL DEFAULT '',
    created_from TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_rules_tool ON rules(tool);
CREATE INDEX IF NOT EXISTS idx_rules_session ON rules(session_id);`},
	// 0004 adds the F7 session-reuse columns (PRD D14: Hub maintains
	// cwd/origin/parent_session_id/title on the sessions table).
	{"0004_session_f7", `
ALTER TABLE sessions ADD COLUMN origin TEXT NOT NULL DEFAULT 'managed';
ALTER TABLE sessions ADD COLUMN parent_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT '';`},
	// 0005 creates the task queue (PRD F6/D13: 任务队列+看板，全托管派发).
	{"0005_tasks", `
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    agent_kind TEXT NOT NULL,
    cwd TEXT NOT NULL,
    prompt TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    session_id TEXT NOT NULL DEFAULT '',
    retries INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER NOT NULL DEFAULT 1,
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    started_at INTEGER,
    finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);`},
	// 0006 persists the console event stream (vibe-trial B5: events used to
	// live only in a 500-entry in-memory ring and evaporated on every
	// daemon restart, severing the audit trail from the session archive).
	{"0006_ui_events", `
CREATE TABLE IF NOT EXISTS ui_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    type TEXT NOT NULL,
    agent_kind TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    payload TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_ui_events_ts ON ui_events(ts);
CREATE INDEX IF NOT EXISTS idx_ui_events_session ON ui_events(session_id);`},
	// 0007 attributes approval_history rows to their session (vibe-trial
	// B3: system verdicts with permission_request_id IS NULL were lost by
	// the export's INNER JOIN because the table carried no session column).
	{"0007_approval_session", `ALTER TABLE approval_history ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`},
}

// ErrNotFound is returned by store lookups when the requested row does not exist.
var ErrNotFound = fmt.Errorf("not found")

// InitDB opens (creating if needed) the SQLite database at dbPath, applies
// pending migrations and returns the handle.
//
// Pragmas follow decision G: WAL journal mode with synchronous=NORMAL, applied
// on every pooled connection via DSN _pragma parameters (the modernc driver
// applies them per connection). Foreign keys are enforced and a busy timeout
// guards against concurrent writer lock contention.
func InitDB(dbPath string) (*sql.DB, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create data directory: %w", err)
		}
	}

	dsn := dbPath + "?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// mattn/go-sqlite3 opens connections lazily; force one so failures
	// surface here instead of on first use.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	applied, err := Migrate(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, name := range applied {
		fmt.Printf("🟢 migration applied: %s\n", name)
	}

	return db, nil
}

// Migrate applies any registered migrations that have not been recorded in the
// migrations table yet and returns the names of the migrations it applied.
// It is idempotent: repeated calls (and repeated database opens) are no-ops.
func Migrate(db *sql.DB) ([]string, error) {
	var applied []string
	for _, m := range registeredMigrations {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM migrations WHERE name = ?", m.Name).Scan(&count)
		if err != nil {
			// First ever open: the migrations table itself does not exist yet.
			if !isNoSuchTable(err) {
				return nil, fmt.Errorf("failed to check migration status for %s: %w", m.Name, err)
			}
			count = 0
		}
		if count > 0 {
			continue
		}

		if _, err := db.Exec(m.SQL); err != nil {
			return nil, fmt.Errorf("failed to apply migration %s: %w", m.Name, err)
		}
		if _, err := db.Exec("INSERT INTO migrations (name) VALUES (?)", m.Name); err != nil {
			return nil, fmt.Errorf("failed to record migration %s: %w", m.Name, err)
		}
		applied = append(applied, m.Name)
	}
	return applied, nil
}

// isNoSuchTable reports whether err is SQLite's "no such table" error.
func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	// mattn/go-sqlite3 surfaces this as "no such table: migrations (1)".
	return strings.Contains(err.Error(), "no such table")
}

// CloseDB safely closes the database connection.
func CloseDB(db *sql.DB) error {
	return db.Close()
}
