package acpclient

import "fmt"

// Permission decision vocabulary (decision B):
//
//	allow_once    — approve this single execution
//	allow_session — approve for the rest of this session (memory is in-process)
//	allow_always  — persist to approval memory; requires a second confirmation
//	reject_once   — deny this single execution
//	reject_always — persist a deny verdict; requires a second confirmation
//
// The DSH-verified ACP shape (allow-once / reject-once only) is a subset of
// this vocabulary; the adapter always sends the full option list to the Hub UI
// and maps foreign option ids onto the closest supported decision.

// Decision identifiers.
const (
	DecisionAllowOnce    = "allow_once"
	DecisionAllowSession = "allow_session"
	DecisionAllowAlways  = "allow_always"
	DecisionRejectOnce   = "reject_once"
	DecisionRejectAlways = "reject_always"
)

// PermissionOption is one choice offered to the human for a permission request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Label    string `json:"label"`
	Kind     string `json:"kind"` // "allow" or "reject"
}

// PermissionOptions lists the options presented for a permission request.
func PermissionOptions(risk RiskLevel) []PermissionOption {
	opts := []PermissionOption{
		{OptionID: DecisionAllowOnce, Label: "仅本次允许", Kind: "allow"},
		{OptionID: DecisionAllowSession, Label: "本会话内允许", Kind: "allow"},
	}
	if risk != RiskHigh {
		// Permanent verdicts are offered for non-high-risk operations only;
		// high-risk operations are always re-confirmed (decision C).
		opts = append(opts,
			PermissionOption{OptionID: DecisionAllowAlways, Label: "永久允许（需二次确认）", Kind: "allow"},
		)
	}
	opts = append(opts,
		PermissionOption{OptionID: DecisionRejectOnce, Label: "仅本次拒绝", Kind: "reject"},
		PermissionOption{OptionID: DecisionRejectAlways, Label: "永久拒绝（需二次确认）", Kind: "reject"},
	)
	return opts
}

// PermissionDecision is a human verdict on a permission request, submitted via
// the daemon API.
type PermissionDecision struct {
	Decision  string `json:"decision"` // one of the Decision* constants
	Confirm   bool   `json:"confirm"`  // second confirmation for *_always
	Note      string `json:"note,omitempty"`
	DecidedBy string `json:"decidedBy,omitempty"` // defaults to "user"
}

// NormalizeDecision canonicalizes a decision id, accepting the hyphenated ACP
// spellings (allow-once) and bare allow/reject.
func NormalizeDecision(s string) (string, error) {
	switch s {
	case DecisionAllowOnce, "allow-once", "allow":
		return DecisionAllowOnce, nil
	case DecisionAllowSession, "allow-session", "session":
		return DecisionAllowSession, nil
	case DecisionAllowAlways, "allow-always", "always":
		return DecisionAllowAlways, nil
	case DecisionRejectOnce, "reject-once", "reject", "deny":
		return DecisionRejectOnce, nil
	case DecisionRejectAlways, "reject-always":
		return DecisionRejectAlways, nil
	default:
		return "", fmt.Errorf("unknown decision %q", s)
	}
}

// PermissionResolution is the result of applying a decision to a request.
type PermissionResolution struct {
	RequestID            string `json:"requestId"`
	Outcome              string `json:"outcome"` // approved / rejected
	Applied              bool   `json:"applied"`
	Decision             string `json:"decision,omitempty"` // normalized decision id (allow_once …)
	RequiresConfirmation bool   `json:"requiresConfirmation,omitempty"`
	Note                 string `json:"note,omitempty"`
}

// Spec outcome vocabulary (EVENT_MODEL v0.2 §2 — frozen, no allow-always:
// "always" is Hub-side policy carried by rule_scope).
const (
	OutcomeAllowedOnce = "allowed_once"
	OutcomeRejectedEv  = "rejected"
	OutcomeCancelled   = "cancelled"
	OutcomeUnavailable = "unavailable"
)

// Spec decided_by vocabulary.
const (
	DecidedByHub     = "hub"
	DecidedByPolicy  = "policy"
	DecidedByTimeout = "timeout"
	DecidedByNative  = "native_gui"
)

// Rule scopes (EVENT_MODEL v0.2 C3).
const (
	ScopeNone    = "none"
	ScopeSession = "session"
	ScopeAlways  = "always"
)

// Decision is the verdict a waiting hook intake receives (spec §3 mapping).
type Decision struct {
	Outcome   string `json:"outcome"`    // allowed_once | rejected | cancelled | unavailable
	RuleScope string `json:"rule_scope"` // none | session | always
	Note      string `json:"note,omitempty"`
	DecidedBy string `json:"decided_by"` // hub | policy | timeout | native_gui
}

// AllowOnceDecision is option ① (放行一次).
func AllowOnceDecision() Decision {
	return Decision{Outcome: OutcomeAllowedOnce, RuleScope: ScopeNone, DecidedBy: DecidedByHub}
}

// AllowWithScopeDecision is option ② / ③ (rule_scope=session|always).
func AllowWithScopeDecision(scope string) Decision {
	return Decision{Outcome: OutcomeAllowedOnce, RuleScope: scope, DecidedBy: DecidedByHub}
}

// RejectDecision is option ④ (拒绝, note carries the 复验 content).
func RejectDecision(note string) Decision {
	return Decision{Outcome: OutcomeRejectedEv, RuleScope: ScopeNone, DecidedBy: DecidedByHub, Note: note}
}

// TimedOutDecision fails closed after the intake deadline.
func TimedOutDecision() Decision {
	return Decision{Outcome: OutcomeUnavailable, RuleScope: ScopeNone, DecidedBy: DecidedByTimeout}
}
