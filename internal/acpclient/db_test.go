package acpclient

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// expectedTables is the full schema after all migrations (T2 + PRD F3 rules
// table + F7 session columns).
var expectedTables = []string{
	"sessions",
	"tool_calls",
	"permission_requests",
	"approval_history",
	"approval_memory",
	"notifications",
	"migrations",
	"rules",
	"tasks",
	"ui_events",
}

// testDBPath returns a fresh SQLite file path inside a per-test temp dir.
func testDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "agenthub.db")
}

// TestInitDBCreatesAllTables verifies (T2) that applying migrate_init.sql at
// runtime creates all 7 tables in a real SQLite database file.
func TestInitDBCreatesAllTables(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	got := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for _, want := range expectedTables {
		if !got[want] {
			t.Errorf("table %q not created by migration", want)
		}
	}
	if len(got) != len(expectedTables)+1 { // +1 for sqlite_sequence (AUTOINCREMENT)
		t.Errorf("unexpected table set: %v", got)
	}
}

// TestWALAndForeignKeys verifies the pragmas from decision G are in effect.
func TestWALAndForeignKeys(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	// FK enforcement: inserting a tool call for a missing session must fail.
	if _, err := db.Exec(`INSERT INTO tool_calls (session_id, agent_tool_call_id, tool_name) VALUES ('ghost', 'tc1', 'Bash')`); err == nil {
		t.Error("foreign key constraint not enforced")
	}
}

// TestInitDBIdempotentReopen verifies migration idempotency: reopening an
// existing database must not re-apply migrations nor fail the UNIQUE
// constraint on migrations.name (regression for the original InitDB bug).
func TestInitDBIdempotentReopen(t *testing.T) {
	dbPath := testDBPath(t)

	db1, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("first InitDB: %v", err)
	}
	ctx := context.Background()
	store := NewStore(db1)
	if err := store.CreateSession(ctx, &Session{ID: "s1", CWD: "D:\\tmp", Status: StatusActive}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	db1.Close()

	db2, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("second InitDB (idempotency broken): %v", err)
	}
	defer db2.Close()

	applied, err := Migrate(db2)
	if err != nil {
		t.Fatalf("Migrate after reopen: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("Migrate re-applied migrations on existing DB: %v", applied)
	}

	// Data written before the reopen must survive (WAL, same file).
	store2 := NewStore(db2)
	sess, err := store2.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("get session after reopen: %v", err)
	}
	if sess.CWD != "D:\\tmp" {
		t.Errorf("session cwd = %q, want D:\\tmp", sess.CWD)
	}
}

// TestStoreSessionCRUD covers the session row lifecycle.
func TestStoreSessionCRUD(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	if _, err := store.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession missing = %v, want ErrNotFound", err)
	}

	sess := &Session{ID: "s1", CWD: "D:\\work", Model: "claude-sonnet", Provider: "anthropic", Status: StatusActive}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.CreatedAt == 0 || sess.UpdatedAt == 0 {
		t.Error("CreateSession did not stamp timestamps")
	}

	sess.Status = StatusRunning
	sess.Turns = 3
	sess.InputTokens = 100
	sess.OutputTokens = 250
	if err := store.UpdateSession(ctx, sess); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}

	got, err := store.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Status != StatusRunning || got.Turns != 3 || got.InputTokens != 100 || got.OutputTokens != 250 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}

	all, err := store.ListSessions(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListSessions: %v (n=%d)", err, len(all))
	}
}

// TestStoreToolCallLifecycle covers tool_calls insert → approve → result.
func TestStoreToolCallLifecycle(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	if err := store.CreateSession(ctx, &Session{ID: "s1", CWD: ".", Status: StatusActive}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	tc := &ToolCall{SessionID: "s1", AgentToolCallID: "tc_1", ToolName: "Bash", ToolKind: "execute", ToolStatus: ToolPending, Risk: RiskMedium}
	if err := store.InsertToolCall(ctx, tc); err != nil {
		t.Fatalf("InsertToolCall: %v", err)
	}
	if tc.ID == 0 {
		t.Fatal("InsertToolCall did not set row id")
	}

	got, err := store.GetToolCallByAgentID(ctx, "s1", "tc_1")
	if err != nil {
		t.Fatalf("GetToolCallByAgentID: %v", err)
	}
	if got.ToolName != "Bash" || got.Risk != RiskMedium {
		t.Errorf("tool call mismatch: %+v", got)
	}

	if err := store.UpdateToolCallStatus(ctx, tc.ID, ToolApproved); err != nil {
		t.Fatalf("UpdateToolCallStatus: %v", err)
	}
	if err := store.UpdateToolCallResult(ctx, tc.ID, ToolCompleted, `{"ok":true}`); err != nil {
		t.Fatalf("UpdateToolCallResult: %v", err)
	}

	calls, err := store.ListToolCalls(ctx, "s1")
	if err != nil || len(calls) != 1 {
		t.Fatalf("ListToolCalls: %v (n=%d)", err, len(calls))
	}
	if calls[0].ToolStatus != ToolCompleted || calls[0].CompletedAt == 0 {
		t.Errorf("lifecycle not finalized: %+v", calls[0])
	}
}

// TestStorePermissionRequestFlow covers pending → resolved and the
// double-resolve guard.
func TestStorePermissionRequestFlow(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	if err := store.CreateSession(ctx, &Session{ID: "s1", CWD: ".", Status: StatusActive}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	pr := &PermissionRequest{RequestUID: "pr_x", SessionID: "s1", ToolName: "Bash", Risk: RiskMedium, CommandPattern: "npm install", Reason: "test"}
	if err := store.InsertPermissionRequest(ctx, pr); err != nil {
		t.Fatalf("InsertPermissionRequest: %v", err)
	}

	byUID, err := store.GetPermissionRequestByUID(ctx, "pr_x")
	if err != nil || byUID.ID != pr.ID || byUID.Outcome != OutcomePending {
		t.Fatalf("GetPermissionRequestByUID: %v %+v", err, byUID)
	}

	pending, err := store.ListPermissionRequests(ctx, OutcomePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPermissionRequests(pending): %v (n=%d)", err, len(pending))
	}

	if err := store.ResolvePermissionRequest(ctx, pr.ID, OutcomeApproved, DecisionAllowOnce); err != nil {
		t.Fatalf("ResolvePermissionRequest: %v", err)
	}
	if err := store.ResolvePermissionRequest(ctx, pr.ID, OutcomeRejected, "x"); err == nil {
		t.Error("double resolve accepted")
	}

	resolved, err := store.GetPermissionRequest(ctx, pr.ID)
	if err != nil || resolved.Outcome != OutcomeApproved || resolved.OptionID != DecisionAllowOnce || resolved.DecidedAt == 0 {
		t.Errorf("resolved request mismatch: %+v (err %v)", resolved, err)
	}
}

// TestStoreApprovalMemory covers upsert/overwrite/list of decision-B memory.
func TestStoreApprovalMemory(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	if mem, err := store.GetApprovalMemory(ctx, AgentKindClaudeCode, "Bash", "npm install"); err != nil || mem != nil {
		t.Fatalf("empty memory lookup: %v %v", mem, err)
	}

	m1 := &ApprovalMemory{AgentKind: AgentKindClaudeCode, Tool: "Bash", CommandPattern: "npm install", Decision: "allow", Scope: "always", ConfirmCount: 2}
	if err := store.UpsertApprovalMemory(ctx, m1); err != nil {
		t.Fatalf("UpsertApprovalMemory: %v", err)
	}
	m2 := &ApprovalMemory{AgentKind: AgentKindClaudeCode, Tool: "Bash", CommandPattern: "npm install", Decision: "deny", Scope: "always", ConfirmCount: 3}
	if err := store.UpsertApprovalMemory(ctx, m2); err != nil {
		t.Fatalf("UpsertApprovalMemory overwrite: %v", err)
	}

	got, err := store.GetApprovalMemory(ctx, AgentKindClaudeCode, "Bash", "npm install")
	if err != nil {
		t.Fatalf("GetApprovalMemory: %v", err)
	}
	if got.Decision != "deny" || got.ConfirmCount != 3 {
		t.Errorf("memory not overwritten: %+v", got)
	}

	all, err := store.ListApprovalMemory(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListApprovalMemory: %v (n=%d)", err, len(all))
	}
}

// TestApprovalHistorySystemEntries verifies audit entries without a
// permission_requests row (system gate decisions) are recorded.
func TestApprovalHistorySystemEntries(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	if err := store.InsertApproval(ctx, &ApprovalRecord{Decision: "allow", DecisionBy: "system", Notes: "approval memory: allow"}); err != nil {
		t.Fatalf("InsertApproval (no request id): %v", err)
	}
	if err := store.InsertApproval(ctx, &ApprovalRecord{Decision: "deny", DecisionBy: "user"}); err != nil {
		t.Fatalf("InsertApproval: %v", err)
	}
	records, err := store.ListApprovals(ctx, 10)
	if err != nil || len(records) != 2 {
		t.Fatalf("ListApprovals: %v (n=%d)", err, len(records))
	}
	if records[0].ID <= records[1].ID {
		t.Error("ListApprovals not newest-first")
	}
}
