package acpclient

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/rules"
)

// Store is the typed SQLite data access layer used by the adapters and the
// daemon. All methods are safe for concurrent use (database/sql handles
// pooling; SQLite WAL allows a single writer with concurrent readers).
type Store struct {
	db *sql.DB
}

// NewStore wraps an opened database handle.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// DB exposes the underlying handle (used by the daemon for direct queries and
// by tests).
func (s *Store) DB() *sql.DB { return s.db }

func wrapErr(what string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// ---------------------------------------------------------------- sessions

const sessionCols = `id, agent_kind, cwd, model, provider, status, turns, input_tokens, output_tokens,
	last_error, is_terminal, is_closed, origin, parent_session_id, title, created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var s Session
	var lastError sql.NullString
	err := row.Scan(&s.ID, &s.AgentKind, &s.CWD, &s.Model, &s.Provider, &s.Status, &s.Turns,
		&s.InputTokens, &s.OutputTokens, &lastError, &s.IsTerminal, &s.IsClosed,
		&s.Origin, &s.ParentSessionID, &s.Title, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	s.LastError = lastError.String
	return &s, nil
}

// CreateSession inserts a new session row.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	now := time.Now().Unix()
	if sess.CreatedAt == 0 {
		sess.CreatedAt = now
	}
	if sess.AgentKind == "" {
		sess.AgentKind = AgentKindClaudeCode
	}
	if sess.Origin == "" {
		sess.Origin = "managed"
	}
	sess.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(id, agent_kind, cwd, model, provider, status, turns, input_tokens, output_tokens, last_error, is_terminal, is_closed, origin, parent_session_id, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.AgentKind, sess.CWD, sess.Model, sess.Provider, sess.Status, sess.Turns,
		sess.InputTokens, sess.OutputTokens, sess.LastError, sess.IsTerminal, sess.IsClosed,
		sess.Origin, sess.ParentSessionID, sess.Title, sess.CreatedAt, sess.UpdatedAt)
	return wrapErr("create session", err)
}

// GetSession fetches one session by id.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if err != nil {
		return nil, wrapErr("get session", err)
	}
	return sess, nil
}

// ListSessions returns all sessions, newest first.
func (s *Store) ListSessions(ctx context.Context) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, wrapErr("list sessions", err)
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, wrapErr("list sessions", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// UpdateSession persists the mutable fields of a session (status, usage
// counters, error) and bumps updated_at.
func (s *Store) UpdateSession(ctx context.Context, sess *Session) error {
	sess.UpdatedAt = time.Now().Unix()
	// Persist the identity columns too (vibe-trial B1: cwd/agent_kind/
	// origin/parent_session_id used to survive only in memory, so a retry's
	// reset and a continue's lineage never reached the row).
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET
		status = ?, turns = ?, input_tokens = ?, output_tokens = ?, last_error = ?,
		is_terminal = ?, is_closed = ?, title = ?, cwd = ?, agent_kind = ?, origin = ?,
		parent_session_id = ?, updated_at = ? WHERE id = ?`,
		sess.Status, sess.Turns, sess.InputTokens, sess.OutputTokens, sess.LastError,
		sess.IsTerminal, sess.IsClosed, sess.Title, sess.CWD, sess.AgentKind, sess.Origin,
		sess.ParentSessionID, sess.UpdatedAt, sess.ID)
	return wrapErr("update session", err)
}

// ---------------------------------------------------------------- rules (PRD F3)

// AddRule persists one policy rule (EVENT_MODEL v0.2 §3.1; created via a
// allow_session / allow_always verdict).
func (s *Store) AddRule(ctx context.Context, r rules.Rule) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO rules
		(id, agent_kind, tool, arg_pattern, scope, effect, session_id, created_from, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.AgentKind, r.Tool, r.ArgPattern, r.Scope, r.Effect, r.SessionID, r.CreatedFrom, r.CreatedAt.Unix())
	return wrapErr("add rule", err)
}

// RemoveRule deletes one policy rule; false = unknown id.
func (s *Store) RemoveRule(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, id)
	if err != nil {
		return false, wrapErr("remove rule", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, wrapErr("remove rule", err)
	}
	return n > 0, nil
}

// ListRules returns every persisted policy rule, newest first.
func (s *Store) ListRules(ctx context.Context) ([]rules.Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent_kind, tool, arg_pattern, scope, effect, session_id, created_from, created_at
		FROM rules ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, wrapErr("list rules", err)
	}
	defer rows.Close()
	var out []rules.Rule
	for rows.Next() {
		var r rules.Rule
		var agentKind, createdFrom sql.NullString
		var ts int64
		if err := rows.Scan(&r.ID, &agentKind, &r.Tool, &r.ArgPattern, &r.Scope, &r.Effect,
			&r.SessionID, &createdFrom, &ts); err != nil {
			return nil, wrapErr("list rules", err)
		}
		r.AgentKind, r.CreatedFrom = agentKind.String, createdFrom.String
		r.CreatedAt = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- tool calls

// InsertToolCall records a new tool call; the generated row id is stored in tc.ID.
func (s *Store) InsertToolCall(ctx context.Context, tc *ToolCall) error {
	now := time.Now().Unix()
	if tc.CreatedAt == 0 {
		tc.CreatedAt = now
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO tool_calls
		(session_id, agent_tool_call_id, tool_name, tool_kind, tool_status, risk, tool_content, raw_input, raw_output, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tc.SessionID, tc.AgentToolCallID, tc.ToolName, tc.ToolKind, tc.ToolStatus, tc.Risk,
		tc.ToolContent, tc.RawInput, tc.RawOutput, tc.CreatedAt)
	if err != nil {
		return wrapErr("insert tool call", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return wrapErr("insert tool call", err)
	}
	tc.ID = id
	return nil
}

// GetToolCallByAgentID looks up a tool call by the agent's own tool call id
// within a session.
func (s *Store) GetToolCallByAgentID(ctx context.Context, sessionID, agentToolCallID string) (*ToolCall, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, agent_tool_call_id, tool_name, tool_kind,
		tool_status, risk, tool_content, raw_input, raw_output, created_at, completed_at
		FROM tool_calls WHERE session_id = ? AND agent_tool_call_id = ?`, sessionID, agentToolCallID)
	tc, err := scanToolCall(row)
	if err != nil {
		return nil, wrapErr("get tool call", err)
	}
	return tc, nil
}

// GetToolCall looks up one tool call by its DB id (late-verdict guard,
// vibe-trial A9).
func (s *Store) GetToolCall(ctx context.Context, id int64) (*ToolCall, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, agent_tool_call_id, tool_name, tool_kind,
		tool_status, risk, tool_content, raw_input, raw_output, created_at, completed_at
		FROM tool_calls WHERE id = ?`, id)
	tc, err := scanToolCall(row)
	if err != nil {
		return nil, wrapErr("get tool call", err)
	}
	return tc, nil
}

func scanToolCall(row interface{ Scan(...any) error }) (*ToolCall, error) {
	var tc ToolCall
	var kind, content, rawIn, rawOut sql.NullString
	var completedAt sql.NullInt64
	err := row.Scan(&tc.ID, &tc.SessionID, &tc.AgentToolCallID, &tc.ToolName, &tc.ToolKind,
		&tc.ToolStatus, &tc.Risk, &content, &rawIn, &rawOut, &tc.CreatedAt, &completedAt)
	if err != nil {
		return nil, err
	}
	tc.ToolKind, tc.ToolContent, tc.RawInput, tc.RawOutput = kind.String, content.String, rawIn.String, rawOut.String
	tc.CompletedAt = completedAt.Int64
	return &tc, nil
}

// UpdateToolCallStatus transitions a tool call's lifecycle status, stamping
// completed_at when it reaches a terminal status.
func (s *Store) UpdateToolCallStatus(ctx context.Context, id int64, status string) error {
	var completedAt any
	switch status {
	case ToolCompleted, ToolFailed, ToolRejected:
		completedAt = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tool_calls SET tool_status = ?, completed_at = COALESCE(?, completed_at) WHERE id = ?`,
		status, completedAt, id)
	return wrapErr("update tool call status", err)
}

// UpdateToolCallResult records the final output of a completed tool call.
func (s *Store) UpdateToolCallResult(ctx context.Context, id int64, status, rawOutput string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tool_calls SET tool_status = ?, raw_output = ?, completed_at = ? WHERE id = ?`,
		status, rawOutput, time.Now().Unix(), id)
	return wrapErr("update tool call result", err)
}

// ListToolCalls returns every tool call of a session, oldest first.
func (s *Store) ListToolCalls(ctx context.Context, sessionID string) ([]*ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, agent_tool_call_id, tool_name, tool_kind,
		tool_status, risk, tool_content, raw_input, raw_output, created_at, completed_at
		FROM tool_calls WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, wrapErr("list tool calls", err)
	}
	defer rows.Close()
	var out []*ToolCall
	for rows.Next() {
		tc, err := scanToolCall(rows)
		if err != nil {
			return nil, wrapErr("list tool calls", err)
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// ------------------------------------------------------- permission requests

// InsertPermissionRequest records a pending approval prompt; the generated row
// id is stored in pr.ID.
func (s *Store) InsertPermissionRequest(ctx context.Context, pr *PermissionRequest) error {
	now := time.Now().Unix()
	if pr.CreatedAt == 0 {
		pr.CreatedAt = now
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO permission_requests
		(request_uid, session_id, tool_call_id, tool_name, risk, command_pattern, reason, outcome, option_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pr.RequestUID, pr.SessionID, nullIfZero(pr.ToolCallID), nullIfEmpty(pr.ToolName),
		nullIfEmpty(pr.Risk), nullIfEmpty(pr.CommandPattern), nullIfEmpty(pr.Reason),
		OutcomePending, nullIfEmpty(pr.OptionID), pr.CreatedAt)
	if err != nil {
		return wrapErr("insert permission request", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return wrapErr("insert permission request", err)
	}
	pr.ID, pr.Outcome = id, OutcomePending
	return nil
}

// ResolvePermissionRequest records the human decision on a pending request.
func (s *Store) ResolvePermissionRequest(ctx context.Context, id int64, outcome, optionID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE permission_requests SET outcome = ?, option_id = ?, decided_at = ? WHERE id = ? AND outcome = ?`,
		outcome, nullIfEmpty(optionID), time.Now().Unix(), id, OutcomePending)
	if err != nil {
		return wrapErr("resolve permission request", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrapErr("resolve permission request", err)
	}
	if n == 0 {
		// Either already resolved or never existed — distinguish for callers.
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM permission_requests WHERE id = ?`, id).Scan(&exists); err != nil {
			return wrapErr("resolve permission request", err)
		}
		if exists == 0 {
			return wrapErr("resolve permission request", ErrNotFound)
		}
		return fmt.Errorf("resolve permission request: request %d already resolved", id)
	}
	return nil
}

// GetPermissionRequest fetches one request by its row id.
func (s *Store) GetPermissionRequest(ctx context.Context, id int64) (*PermissionRequest, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, request_uid, session_id, tool_call_id, tool_name, risk,
		command_pattern, reason, outcome, option_id, created_at, decided_at
		FROM permission_requests WHERE id = ?`, id)
	pr, err := scanPermissionRequest(row)
	if err != nil {
		return nil, wrapErr("get permission request", err)
	}
	return pr, nil
}

// GetPermissionRequestByUID fetches one request by its public request id.
func (s *Store) GetPermissionRequestByUID(ctx context.Context, uid string) (*PermissionRequest, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, request_uid, session_id, tool_call_id, tool_name, risk,
		command_pattern, reason, outcome, option_id, created_at, decided_at
		FROM permission_requests WHERE request_uid = ?`, uid)
	pr, err := scanPermissionRequest(row)
	if err != nil {
		return nil, wrapErr("get permission request", err)
	}
	return pr, nil
}

// ListPermissionRequests returns requests filtered by outcome ("" = all),
// newest first.
func (s *Store) ListPermissionRequests(ctx context.Context, outcome string) ([]*PermissionRequest, error) {
	q := `SELECT id, request_uid, session_id, tool_call_id, tool_name, risk, command_pattern, reason,
		outcome, option_id, created_at, decided_at FROM permission_requests`
	args := []any{}
	if outcome != "" {
		q += ` WHERE outcome = ?`
		args = append(args, outcome)
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrapErr("list permission requests", err)
	}
	defer rows.Close()
	var out []*PermissionRequest
	for rows.Next() {
		pr, err := scanPermissionRequest(rows)
		if err != nil {
			return nil, wrapErr("list permission requests", err)
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

func scanPermissionRequest(row interface{ Scan(...any) error }) (*PermissionRequest, error) {
	var pr PermissionRequest
	var toolCallID sql.NullInt64
	var toolName, risk, pattern, reason, optionID sql.NullString
	var decidedAt sql.NullInt64
	err := row.Scan(&pr.ID, &pr.RequestUID, &pr.SessionID, &toolCallID, &toolName, &risk, &pattern,
		&reason, &pr.Outcome, &optionID, &pr.CreatedAt, &decidedAt)
	if err != nil {
		return nil, err
	}
	pr.ToolCallID, pr.ToolName, pr.Risk = toolCallID.Int64, toolName.String, risk.String
	pr.CommandPattern, pr.Reason, pr.OptionID = pattern.String, reason.String, optionID.String
	pr.DecidedAt = decidedAt.Int64
	return &pr, nil
}

// ------------------------------------------------------------ approval history

// InsertApproval appends one entry to the approval audit trail.
func (s *Store) InsertApproval(ctx context.Context, ah *ApprovalRecord) error {
	if ah.DecidedAt == 0 {
		ah.DecidedAt = time.Now().Unix()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO approval_history
		(permission_request_id, session_id, decision, decided_at, decision_by, notes) VALUES (?, ?, ?, ?, ?, ?)`,
		nullIfZero(ah.PermissionRequestID), ah.SessionID, ah.Decision, ah.DecidedAt, ah.DecisionBy, nullIfEmpty(ah.Notes))
	if err != nil {
		return wrapErr("insert approval", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return wrapErr("insert approval", err)
	}
	ah.ID = id
	return nil
}

// ListApprovals returns the most recent approval records (audit trail).
func (s *Store) ListApprovals(ctx context.Context, limit int) ([]*ApprovalRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, permission_request_id, decision, decided_at, decision_by, notes
		FROM approval_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, wrapErr("list approvals", err)
	}
	defer rows.Close()
	var out []*ApprovalRecord
	for rows.Next() {
		var ah ApprovalRecord
		var permReqID sql.NullInt64
		var notes sql.NullString
		if err := rows.Scan(&ah.ID, &permReqID, &ah.Decision, &ah.DecidedAt, &ah.DecisionBy, &notes); err != nil {
			return nil, wrapErr("list approvals", err)
		}
		ah.PermissionRequestID = permReqID.Int64
		ah.Notes = notes.String
		out = append(out, &ah)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- approval memory

// GetApprovalMemory returns the persisted verdict for (agentKind, tool,
// commandPattern), or (nil, nil) when no verdict is stored.
func (s *Store) GetApprovalMemory(ctx context.Context, agentKind, tool, pattern string) (*ApprovalMemory, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, agent_kind, tool, command_pattern, decision, scope, confirm_count,
		created_at, updated_at FROM approval_memory WHERE agent_kind = ? AND tool = ? AND command_pattern = ?`,
		agentKind, tool, pattern)
	var m ApprovalMemory
	err := row.Scan(&m.ID, &m.AgentKind, &m.Tool, &m.CommandPattern, &m.Decision, &m.Scope, &m.ConfirmCount,
		&m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapErr("get approval memory", err)
	}
	return &m, nil
}

// UpsertApprovalMemory writes (or overwrites) the verdict for a memory key.
func (s *Store) UpsertApprovalMemory(ctx context.Context, m *ApprovalMemory) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO approval_memory
		(agent_kind, tool, command_pattern, decision, scope, confirm_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_kind, tool, command_pattern) DO UPDATE SET
			decision = excluded.decision, scope = excluded.scope, confirm_count = excluded.confirm_count,
			updated_at = excluded.updated_at`,
		m.AgentKind, m.Tool, m.CommandPattern, m.Decision, m.Scope, m.ConfirmCount, now, now)
	return wrapErr("upsert approval memory", err)
}

// ListApprovalMemory returns all persisted verdicts.
func (s *Store) ListApprovalMemory(ctx context.Context) ([]*ApprovalMemory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent_kind, tool, command_pattern, decision, scope, confirm_count,
		created_at, updated_at FROM approval_memory ORDER BY updated_at DESC`)
	if err != nil {
		return nil, wrapErr("list approval memory", err)
	}
	defer rows.Close()
	var out []*ApprovalMemory
	for rows.Next() {
		var m ApprovalMemory
		if err := rows.Scan(&m.ID, &m.AgentKind, &m.Tool, &m.CommandPattern, &m.Decision, &m.Scope,
			&m.ConfirmCount, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, wrapErr("list approval memory", err)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// DeleteApprovalMemory revokes a persisted verdict (Hub UI 撤销 action).
func (s *Store) DeleteApprovalMemory(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM approval_memory WHERE id = ?`, id)
	if err != nil {
		return wrapErr("delete approval memory", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrapErr("delete approval memory", err)
	}
	if n == 0 {
		return wrapErr("delete approval memory", ErrNotFound)
	}
	return nil
}

// DeleteApprovalMemoryByKey removes the memory row matching a rule's key,
// so removing an always-rule also revokes its memory twin (vibe-trial B12).
func (s *Store) DeleteApprovalMemoryByKey(ctx context.Context, agentKind, tool, pattern string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM approval_memory WHERE agent_kind = ? AND tool = ? AND command_pattern = ?`,
		agentKind, tool, pattern)
	return wrapErr("delete approval memory by key", err)
}

// PurgeSessionsBefore deletes sessions last updated before the given unix
// time (decision G: 90-day retention). Foreign-key cascades clear the
// session's tool calls, permission requests, approvals and notifications;
// approval memory is global and survives. The session's task rows and
// session-scoped rules go with it (vibe-trial B2: they used to outlive the
// session as orphan rows the console could no longer attribute).
func (s *Store) PurgeSessionsBefore(ctx context.Context, beforeUnix int64) (int64, error) {
	var ids []string
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE updated_at < ?`, beforeUnix)
	if err != nil {
		return 0, wrapErr("purge sessions", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE session_id = ?`, id); err != nil {
			return 0, wrapErr("purge session tasks", err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM rules WHERE scope = 'session' AND session_id = ?`, id); err != nil {
			return 0, wrapErr("purge session rules", err)
		}
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE updated_at < ?`, beforeUnix)
	if err != nil {
		return 0, wrapErr("purge sessions", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, wrapErr("purge sessions", err)
	}
	return n, nil
}

// HasNotificationWithTailHash reports whether a notification with the same
// tail hash was already recorded within the given window (de-duplication).
func (s *Store) HasNotificationWithTailHash(ctx context.Context, tailHash string, within time.Duration) (bool, error) {
	since := time.Now().Add(-within).Unix()
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE tail_hash = ? AND sent_at > ?`, tailHash, since).Scan(&count)
	if err != nil {
		return false, wrapErr("check notification dedup", err)
	}
	return count > 0, nil
}

// InsertNotificationWithDedup records a notification unless an identical tail
// hash was already recorded recently.
func (s *Store) InsertNotificationWithDedup(ctx context.Context, n *Notification) (bool, error) {
	if n.TailHash != "" {
		dup, err := s.HasNotificationWithTailHash(ctx, n.TailHash, 24*time.Hour)
		if err != nil {
			return false, err
		}
		if dup {
			return false, nil
		}
	}
	if err := s.InsertNotification(ctx, n); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------- notifications

// InsertNotification appends an entry to the notification outbox.
func (s *Store) InsertNotification(ctx context.Context, n *Notification) error {
	if n.SentAt == 0 {
		n.SentAt = time.Now().Unix()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO notifications
		(session_id, notification_type, title, body, sent_at, read_at, tail_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullIfEmpty(n.SessionID), n.NotificationType, nullIfEmpty(n.Title), nullIfEmpty(n.Body),
		n.SentAt, nullIfZero(n.ReadAt), nullIfEmpty(n.TailHash))
	if err != nil {
		return wrapErr("insert notification", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return wrapErr("insert notification", err)
	}
	n.ID = id
	return nil
}

// ListNotifications returns notification history, newest first.
func (s *Store) ListNotifications(ctx context.Context, limit int) ([]*Notification, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, notification_type, title, body, sent_at, read_at, tail_hash
		FROM notifications ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, wrapErr("list notifications", err)
	}
	defer rows.Close()
	var out []*Notification
	for rows.Next() {
		var n Notification
		var sessionID, title, body, tailHash sql.NullString
		var readAt sql.NullInt64
		if err := rows.Scan(&n.ID, &sessionID, &n.NotificationType, &title, &body, &n.SentAt, &readAt, &tailHash); err != nil {
			return nil, wrapErr("list notifications", err)
		}
		n.SessionID, n.Title, n.Body, n.TailHash = sessionID.String, title.String, body.String, tailHash.String
		n.ReadAt = readAt.Int64
		out = append(out, &n)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------- helpers

// ExportSession assembles the full archive bundle of one session (decision G).
func (s *Store) ExportSession(ctx context.Context, id string) (*SessionExport, error) {
	sess, err := s.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	calls, err := s.ListToolCalls(ctx, id)
	if err != nil {
		return nil, err
	}
	reqs, err := s.ListPermissionRequests(ctx, "")
	if err != nil {
		return nil, err
	}
	var sessionReqs []*PermissionRequest
	for _, pr := range reqs {
		if pr.SessionID == id {
			sessionReqs = append(sessionReqs, pr)
		}
	}
	// Filter by the session_id column (migration 0007): system verdicts
	// carry permission_request_id IS NULL and the old INNER JOIN dropped
	// them from the archive entirely (vibe-trial B3).
	rows, err := s.db.QueryContext(ctx, `SELECT ah.id, ah.permission_request_id, ah.decision, ah.decided_at, ah.decision_by, ah.notes
		FROM approval_history ah WHERE ah.session_id = ?
		ORDER BY ah.id`, id)
	if err != nil {
		return nil, wrapErr("export session", err)
	}
	defer rows.Close()
	var approvals []*ApprovalRecord
	for rows.Next() {
		var ah ApprovalRecord
		var permReqID sql.NullInt64
		var notes sql.NullString
		if err := rows.Scan(&ah.ID, &permReqID, &ah.Decision, &ah.DecidedAt, &ah.DecisionBy, &notes); err != nil {
			return nil, wrapErr("export session", err)
		}
		ah.PermissionRequestID = permReqID.Int64
		ah.Notes = notes.String
		approvals = append(approvals, &ah)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr("export session", err)
	}
	// Archive completeness (vibe-trial B4): the bundle used to carry only
	// session/toolCalls/permissionRequests/approvals, losing the dispatch
	// task, its rules and the persisted event stream.
	var sessionTasks []*Task
	if tasks, err := s.ListTasks(ctx, ""); err == nil {
		for _, tk := range tasks {
			if tk.SessionID == id {
				sessionTasks = append(sessionTasks, tk)
			}
		}
	}
	allRules, _ := s.ListRules(ctx)
	var sessionRules []rules.Rule
	for _, r := range allRules {
		if r.SessionID == id {
			sessionRules = append(sessionRules, r)
		}
	}
	events, _ := s.ListUIEvents(ctx, 500, id)
	return &SessionExport{
		Session:            sess,
		ToolCalls:          calls,
		PermissionRequests: sessionReqs,
		Approvals:          approvals,
		Tasks:              sessionTasks,
		Rules:              sessionRules,
		Events:             events,
		ExportedAt:         time.Now().Unix(),
	}, nil
}

// InsertUIEvent persists one console event (vibe-trial B5).
func (s *Store) InsertUIEvent(ctx context.Context, e *UIEventRecord) error {
	e.Ts = time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `INSERT INTO ui_events (ts, type, agent_kind, session_id, payload)
		VALUES (?, ?, ?, ?, ?)`, e.Ts, e.Type, e.AgentKind, e.SessionID, string(e.Payload))
	if err != nil {
		return wrapErr("insert ui event", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		e.ID = id
	}
	return nil
}

// ListUIEvents returns the newest persisted console events, newest first
// (vibe-trial B5: /api/events now reads the database, so history survives
// daemon restarts). Empty sessionID lists across sessions.
func (s *Store) ListUIEvents(ctx context.Context, limit int, sessionID string) ([]*UIEventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT id, ts, type, agent_kind, session_id, payload FROM ui_events`
	args := []any{}
	if sessionID != "" {
		q += ` WHERE session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrapErr("list ui events", err)
	}
	defer rows.Close()
	var out []*UIEventRecord
	for rows.Next() {
		var e UIEventRecord
		var payload string
		if err := rows.Scan(&e.ID, &e.Ts, &e.Type, &e.AgentKind, &e.SessionID, &payload); err != nil {
			return nil, wrapErr("list ui events", err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// PurgeUIEventsBefore deletes console events older than the given unix time
// (same retention policy as sessions, vibe-trial B5).
func (s *Store) PurgeUIEventsBefore(ctx context.Context, beforeUnix int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_events WHERE ts < ?`, beforeUnix)
	return wrapErr("purge ui events", err)
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullIfEmpty(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// ---------------------------------------------------------------- tasks (F6)

// InsertTask records a queued task.
func (s *Store) InsertTask(ctx context.Context, t *Task) error {
	now := time.Now().Unix()
	if t.CreatedAt == 0 {
		t.CreatedAt = now
	}
	if t.MaxRetries == 0 {
		t.MaxRetries = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks
		(id, agent_kind, cwd, prompt, state, session_id, retries, max_retries, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.AgentKind, t.CWD, t.Prompt, t.State, t.SessionID, t.Retries, t.MaxRetries,
		t.Error, t.CreatedAt)
	return wrapErr("insert task", err)
}

// GetTask fetches one task by id.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, agent_kind, cwd, prompt, state, session_id,
		retries, max_retries, error, created_at, started_at, finished_at FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if err != nil {
		return nil, wrapErr("get task", err)
	}
	return t, nil
}

// ListTasks returns tasks filtered by state ("" = all), newest first.
func (s *Store) ListTasks(ctx context.Context, state string) ([]*Task, error) {
	q := `SELECT id, agent_kind, cwd, prompt, state, session_id, retries, max_retries, error,
		created_at, started_at, finished_at FROM tasks`
	args := []any{}
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrapErr("list tasks", err)
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, wrapErr("list tasks", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueuedTasks returns queued tasks in dispatch order (oldest first).
func (s *Store) QueuedTasks(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent_kind, cwd, prompt, state, session_id,
		retries, max_retries, error, created_at, started_at, finished_at
		FROM tasks WHERE state = ? ORDER BY created_at, rowid`, TaskQueued)
	if err != nil {
		return nil, wrapErr("queued tasks", err)
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, wrapErr("queued tasks", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTask persists the mutable fields of a task.
func (s *Store) UpdateTask(ctx context.Context, t *Task) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET state = ?, session_id = ?, retries = ?,
		error = ?, started_at = ?, finished_at = ? WHERE id = ?`,
		t.State, t.SessionID, t.Retries, t.Error, nullIfZero(t.StartedAt), nullIfZero(t.FinishedAt), t.ID)
	return wrapErr("update task", err)
}

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var sessionID, errText sql.NullString
	var startedAt, finishedAt sql.NullInt64
	if err := row.Scan(&t.ID, &t.AgentKind, &t.CWD, &t.Prompt, &t.State, &sessionID,
		&t.Retries, &t.MaxRetries, &errText, &t.CreatedAt, &startedAt, &finishedAt); err != nil {
		return nil, err
	}
	t.SessionID, t.Error = sessionID.String, errText.String
	t.StartedAt, t.FinishedAt = startedAt.Int64, finishedAt.Int64
	return &t, nil
}
