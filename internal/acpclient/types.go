package acpclient

import (
	"encoding/json"

	"github.com/agenttarmac/agenttarmac/internal/rules"
)

// Domain models shared by the SQLite store and the ACP adapters.
// All timestamps are unix seconds, matching the schema defaults in
// migrate_init.sql.

// Session statuses (session/update state machine, see adapterStateTransitions).
const (
	StatusActive           = "active"
	StatusRunning          = "running"
	StatusAwaitingApproval = "awaiting_approval"
	StatusAwaitingInput    = "awaiting_input"
	StatusCompleted        = "completed"
	StatusFailed           = "failed"
	StatusClosed           = "closed"
)

// Risk levels (decision C: passive monitoring for low risk, active
// interception for high risk).
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

// AgentKindClaudeCode is declared in claudecode_adapter.go alongside its
// adapter; AgentKindCodex in codex_adapter.go.

// Session is a hosted Claude Code session.
type Session struct {
	ID              string `json:"id"`
	AgentKind       string `json:"agentKind"`
	CWD             string `json:"cwd"`
	Model           string `json:"model"`
	Provider        string `json:"provider"`
	Status          string `json:"status"`
	Turns           int    `json:"turns"`
	InputTokens     int64  `json:"inputTokens"`
	OutputTokens    int64  `json:"outputTokens"`
	LastError       string `json:"lastError,omitempty"`
	IsTerminal      bool   `json:"isTerminal"`
	IsClosed        bool   `json:"isClosed"`
	Origin          string `json:"origin"`          // managed | attached (PRD F7/D14)
	ParentSessionID string `json:"parentSessionId"` // fork 溯源 (PRD F7/C5)
	Title           string `json:"title"`           // 首条 prompt 摘要 (PRD F7)
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	AgentSessionID  string `json:"agentSessionId"` // R8: real session id the agent generates (ACP session/new); native resume targets this
}

// ToolCall is one observed tool invocation inside a session.
type ToolCall struct {
	ID              int64  `json:"id"`
	SessionID       string `json:"sessionId"`
	AgentToolCallID string `json:"toolCallId"`
	ToolName        string `json:"name"`
	ToolKind        string `json:"kind,omitempty"`
	ToolStatus      string `json:"status"`
	Risk            string `json:"risk"`
	ToolContent     string `json:"content,omitempty"`
	RawInput        string `json:"rawInput,omitempty"`
	RawOutput       string `json:"rawOutput,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	CompletedAt     int64  `json:"completedAt,omitempty"`
}

// Tool call lifecycle statuses.
const (
	ToolPending   = "pending"
	ToolApproved  = "approved"
	ToolRejected  = "rejected"
	ToolCompleted = "completed"
	ToolFailed    = "failed"
)

// PermissionRequest is an approval prompt raised by an adapter, resolved by a
// human decision through the daemon API (decisions B and C).
type PermissionRequest struct {
	ID             int64  `json:"id"`
	RequestUID     string `json:"requestId"`
	SessionID      string `json:"sessionId"`
	ToolCallID     int64  `json:"toolCallId,omitempty"`
	ToolName       string `json:"toolName,omitempty"`
	Risk           string `json:"risk,omitempty"`
	CommandPattern string `json:"commandPattern,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Outcome        string `json:"outcome"`
	OptionID       string `json:"optionId,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
	DecidedAt      int64  `json:"decidedAt,omitempty"`
}

// Permission request outcomes.
const (
	OutcomePending  = "pending"
	OutcomeApproved = "approved"
	OutcomeRejected = "rejected"
	OutcomeExpired  = "expired"
)

// ApprovalRecord is one entry of the approval audit trail.
type ApprovalRecord struct {
	ID                  int64  `json:"id"`
	PermissionRequestID int64  `json:"permissionRequestId"`
	SessionID           string `json:"sessionId,omitempty"`
	Decision            string `json:"decision"` // "allow" or "deny"
	DecidedAt           int64  `json:"decidedAt"`
	DecisionBy          string `json:"decisionBy"` // "user" or "system"
	Notes               string `json:"notes,omitempty"`
}

// ApprovalMemory is a persisted verdict keyed by (agent_kind, tool,
// command_pattern) — decision B's approval memory.
type ApprovalMemory struct {
	ID             int64  `json:"id"`
	AgentKind      string `json:"agentKind"`
	Tool           string `json:"tool"`
	CommandPattern string `json:"commandPattern"`
	Decision       string `json:"decision"` // "allow" or "deny"
	Scope          string `json:"scope"`    // "always"
	ConfirmCount   int    `json:"confirmCount"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// UIEventRecord is one persisted console event (vibe-trial B5: the event
// stream used to be an in-memory ring that restarted empty).
type UIEventRecord struct {
	ID        int64           `json:"id"`
	Ts        int64           `json:"ts"`
	Type      string          `json:"type"`
	AgentKind string          `json:"agentKind,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// Notification is one entry of the desktop/feishu notification outbox.
type Notification struct {
	ID               int64  `json:"id"`
	SessionID        string `json:"sessionId,omitempty"`
	NotificationType string `json:"notificationType"` // "desktop" or "feishu"
	Title            string `json:"title,omitempty"`
	Body             string `json:"body,omitempty"`
	SentAt           int64  `json:"sentAt"`
	ReadAt           int64  `json:"readAt,omitempty"`
	TailHash         string `json:"tailHash,omitempty"`
}

// SessionExport is the archive bundle for one session (decision G: 支持导出).
// The tasks/rules/events sections close the vibe-trial B4 gap (exports used
// to omit everything that happened outside the adapter).
type SessionExport struct {
	Session            *Session             `json:"session"`
	ToolCalls          []*ToolCall          `json:"toolCalls"`
	PermissionRequests []*PermissionRequest `json:"permissionRequests"`
	Approvals          []*ApprovalRecord    `json:"approvals"`
	Tasks              []*Task              `json:"tasks,omitempty"`
	Rules              []rules.Rule         `json:"rules,omitempty"`
	Events             []*UIEventRecord     `json:"events,omitempty"`
	ExportedAt         int64                `json:"exportedAt"`
}

// Task is one F6 dispatch unit (PRD D13: 任务 = 用户意图单元；队列+看板).
type Task struct {
	ID         string `json:"id"`
	AgentKind  string `json:"agentKind"`
	CWD        string `json:"cwd"`
	Prompt     string `json:"prompt"`
	State      string `json:"state"`
	SessionID  string `json:"sessionId,omitempty"`
	Retries    int    `json:"retries"`
	MaxRetries int    `json:"maxRetries"`
	Error      string `json:"error,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// Task states (EVENT_MODEL v0.2 task.action vocabulary).
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskFinished  = "finished"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
)
