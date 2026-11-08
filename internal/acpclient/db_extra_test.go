package acpclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPurgeSessionsBefore verifies decision-G retention: sessions older than
// the cutoff disappear along with their dependent rows, while approval memory
// (global) survives.
func TestPurgeSessionsBefore(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	old := &Session{ID: "old", CWD: ".", Status: StatusCompleted}
	if err := store.CreateSession(ctx, old); err != nil {
		t.Fatalf("create old: %v", err)
	}
	// Backdate the old session beyond any realistic cutoff.
	if _, err := store.DB().Exec(`UPDATE sessions SET updated_at = updated_at - 3600*24*120 WHERE id = 'old'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := store.CreateSession(ctx, &Session{ID: "new", CWD: ".", Status: StatusActive}); err != nil {
		t.Fatalf("create new: %v", err)
	}
	if err := store.InsertToolCall(ctx, &ToolCall{SessionID: "old", AgentToolCallID: "tc1", ToolName: "Bash", ToolStatus: ToolCompleted, Risk: RiskMedium}); err != nil {
		t.Fatalf("insert tool call: %v", err)
	}
	if err := store.UpsertApprovalMemory(ctx, &ApprovalMemory{AgentKind: AgentKindClaudeCode, Tool: "Bash", CommandPattern: "*", Decision: "allow"}); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}

	n, err := store.PurgeSessionsBefore(ctx, time.Now().Unix())
	if err != nil {
		t.Fatalf("PurgeSessionsBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d sessions, want 1", n)
	}
	if _, err := store.GetSession(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Error("old session survived purge")
	}
	if _, err := store.GetSession(ctx, "new"); err != nil {
		t.Error("new session was purged")
	}
	// Cascade removed the old session's tool calls.
	calls, err := store.ListToolCalls(ctx, "old")
	if err != nil || len(calls) != 0 {
		t.Errorf("cascade failed: %v (n=%d)", err, len(calls))
	}
	// Approval memory is global and survives.
	mem, err := store.GetApprovalMemory(ctx, AgentKindClaudeCode, "Bash", "*")
	if err != nil || mem == nil {
		t.Error("approval memory should survive session purge")
	}
}

// TestExportSession verifies the decision-G export bundle.
func TestExportSession(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	if err := store.CreateSession(ctx, &Session{ID: "s1", CWD: "D:\\x", Status: StatusCompleted}); err != nil {
		t.Fatalf("create: %v", err)
	}
	tc := &ToolCall{SessionID: "s1", AgentToolCallID: "tc1", ToolName: "Bash", ToolStatus: ToolCompleted, Risk: RiskMedium}
	if err := store.InsertToolCall(ctx, tc); err != nil {
		t.Fatalf("insert tool call: %v", err)
	}
	pr := &PermissionRequest{RequestUID: "pr_1", SessionID: "s1", ToolCallID: tc.ID, ToolName: "Bash", Risk: RiskMedium, Reason: "test"}
	if err := store.InsertPermissionRequest(ctx, pr); err != nil {
		t.Fatalf("insert permission request: %v", err)
	}
	if err := store.ResolvePermissionRequest(ctx, pr.ID, OutcomeApproved, DecisionAllowOnce); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := store.InsertApproval(ctx, &ApprovalRecord{PermissionRequestID: pr.ID, Decision: "allow", DecisionBy: "user"}); err != nil {
		t.Fatalf("insert approval: %v", err)
	}

	bundle, err := store.ExportSession(ctx, "s1")
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if bundle.Session == nil || bundle.Session.ID != "s1" {
		t.Errorf("bundle session: %+v", bundle.Session)
	}
	if len(bundle.ToolCalls) != 1 || len(bundle.PermissionRequests) != 1 || len(bundle.Approvals) != 1 {
		t.Errorf("bundle contents: calls=%d reqs=%d approvals=%d",
			len(bundle.ToolCalls), len(bundle.PermissionRequests), len(bundle.Approvals))
	}
	if bundle.ExportedAt == 0 {
		t.Error("export timestamp missing")
	}

	if _, err := store.ExportSession(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session export err = %v", err)
	}
}

// TestDeleteApprovalMemory verifies the revoke action behind the Hub UI.
func TestDeleteApprovalMemory(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	if err := store.UpsertApprovalMemory(ctx, &ApprovalMemory{AgentKind: AgentKindCodex, Tool: "shell", CommandPattern: "npm install", Decision: "allow"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	mem, _ := store.GetApprovalMemory(ctx, AgentKindCodex, "shell", "npm install")
	if err := store.DeleteApprovalMemory(ctx, mem.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if again, _ := store.GetApprovalMemory(ctx, AgentKindCodex, "shell", "npm install"); again != nil {
		t.Error("memory survived delete")
	}
	if err := store.DeleteApprovalMemory(ctx, mem.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete err = %v", err)
	}
}

// TestNotificationDedup verifies tail-hash de-duplication for the outbox.
func TestNotificationDedup(t *testing.T) {
	dbPath := testDBPath(t)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := NewStore(db)

	if err := store.CreateSession(ctx, &Session{ID: "s1", CWD: ".", Status: StatusActive}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	n := &Notification{SessionID: "s1", NotificationType: "feishu", Title: "t", Body: "b", TailHash: "hash-1"}
	first, err := store.InsertNotificationWithDedup(ctx, n)
	if err != nil || !first {
		t.Fatalf("first insert: %v %v", first, err)
	}
	dup := &Notification{SessionID: "s1", NotificationType: "feishu", Title: "t", Body: "b", TailHash: "hash-1"}
	again, err := store.InsertNotificationWithDedup(ctx, dup)
	if err != nil {
		t.Fatalf("dup insert: %v", err)
	}
	if again {
		t.Error("duplicate notification recorded")
	}
	records, _ := store.ListNotifications(ctx, 10)
	if len(records) != 1 {
		t.Errorf("notification count = %d, want 1", len(records))
	}
}
