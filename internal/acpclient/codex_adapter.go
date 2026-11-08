package acpclient

// AgentKindCodex is the bridge registry key for Codex sessions (decision A:
// Claude Code + Codex are the first supported agent kinds).
const AgentKindCodex = "codex"

// CodexAdapter is the Codex flavor of the Hub adapter. All behavior lives in
// BaseAdapter; this type pins the agent_kind and serves as the named handle
// for the Codex runtime.
type CodexAdapter = BaseAdapter

// NewCodexAdapter creates the Codex adapter. store must be an opened database
// (InitDB); sink receives outbound frames and may be nil.
func NewCodexAdapter(store *Store, sink Sink) (*CodexAdapter, error) {
	return NewBaseAdapter(AgentKindCodex, store, sink)
}

// CodexFrameVocabulary documents the Codex-specific mapping notes:
//
//   - Codex tool names are lowercase ("shell", "apply_patch", "update_plan");
//     "shell" is already in the terminal tool set and "apply_patch" in the
//     write-like set of ClassifyRisk.
//   - Risk classification and approval memory are Hub-side policy and are
//     intentionally shared across agent kinds; memory keys are namespaced by
//     agent_kind so a Codex verdict never auto-applies to Claude Code.
var _ = struct{}{}
