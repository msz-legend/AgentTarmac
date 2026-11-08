// Package acpclient implements the AgentHub runtime adapters for ACP v2 draft
// agents (Claude Code and Codex first, per decision A), the SQLite persistence
// layer and the approval-memory pipeline (decision B).
package acpclient

// AgentKindClaudeCode is the bridge registry key for Claude Code sessions.
const AgentKindClaudeCode = "claude_code"

// ClaudeCodeAdapter is the Claude Code flavor of the Hub adapter (decision A).
// All behavior lives in BaseAdapter; this type pins the agent_kind and serves
// as the named handle for the Claude Code runtime.
type ClaudeCodeAdapter = BaseAdapter

// NewClaudeCodeAdapter creates the Claude Code adapter. store must be an
// opened database (InitDB); sink receives outbound frames and may be nil.
func NewClaudeCodeAdapter(store *Store, sink Sink) (*ClaudeCodeAdapter, error) {
	return NewBaseAdapter(AgentKindClaudeCode, store, sink)
}

// ClaudeCodeFrameVocabulary documents the frame mapping for the Claude Code
// runtime (see BaseAdapter for the full vocabulary):
//
//	Claude Code tool names (Read/Grep/Glob/Bash/Write/Edit/…) are classified by
//	ClassifyRisk; terminal commands extract their pattern from the "command"
//	field of rawInput.
var _ = struct{}{}
