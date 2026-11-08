// acp_kinds.go pins the agent_kind registry keys for the ACP-native CLIs
// (PRD §7: OpenCode / Hermes = L1 via ACP). They ride the same BaseAdapter
// as claude_code/codex — approvals, sessions, memory and audit stay kind-
// scoped. The connector drives them with:
//
//	agenthub-connect -agent-kind opencode -- opencode acp
//	agenthub-connect -agent-kind hermes  -- hermes acp
package acpclient

// AgentKindOpenCode is the bridge registry key for OpenCode sessions
// (`opencode acp`).
const AgentKindOpenCode = "opencode"

// AgentKindHermes is the bridge registry key for Hermes Agent sessions
// (`hermes acp`). Hermes' ACP approvals map 1:1 onto the Hub's four-option
// gate (allow_once / allow_session / allow_always / deny, fail-closed on
// timeout — approvals.timeout default 300s).
const AgentKindHermes = "hermes"

// Session origin values (PRD F7/D14): attached = the agent process lives
// outside the Hub (connector/user CLI); managed = Hub-spawned (task queue).
const (
	OriginAttached = "attached"
	OriginManaged  = "managed"
)
