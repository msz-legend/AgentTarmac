// Package rules implements the AgentHub policy rule engine.
//
// Rule key format (design/EVENT_MODEL_v0.2.md §3.1, PRD Q-F4 decision —
// M1 must ship this, aligned with Claude Code's `Bash(rm *)` scoping):
//
//	<tool>                  e.g. "Bash"            — applies to every Bash call
//	<tool>(<arg_pattern>)   e.g. "Bash(rm *)"      — applies when the main arg matches
//
// Matching semantics:
//   - The most specific matching rule wins (exact tool beats wildcard, arg
//     pattern beats bare tool).
//   - scope=session rules only apply inside the session they were created for
//     and die with it; scope=always rules are machine-persistent (D7 single
//     user, so rules never cross users).
//   - arg_pattern is a LITERAL glob: '*' matches any run of characters
//     (including '/'), '?' matches exactly one. It does NOT parse shell
//     quoting, variables, pipes or command substitution — e.g. "Bash(rm *)"
//     will NOT match "Bash(x=$(rm -rf /); echo $x)". Documented M1 limit.
package rules

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// EffectAllow is the only effect in M1; deny rules arrive with the M2
// escalation (PRD D16 / EVENT_MODEL §3.1).
const EffectAllow = "allow"

// ErrNoMatch is returned by Match when no allow rule applies.
var ErrNoMatch = errors.New("rules: no matching allow rule")

// Rule is one policy entry. Field names mirror design/EVENT_MODEL_v0.2.md
// §3.1; AgentKind is an additive audit/display field (matching stays on
// tool + arg_pattern per the frozen model).
type Rule struct {
	ID          string    `json:"id"`
	Tool        string    `json:"tool"`                  // exact tool name, or "*" for any tool
	ArgPattern  string    `json:"arg_pattern,omitempty"` // literal glob; empty = any argument
	Scope       string    `json:"scope"`                 // session | always
	Effect      string    `json:"effect"`                // allow (M1)
	SessionID   string    `json:"session_id,omitempty"`  // required when Scope == "session"
	CreatedFrom string    `json:"created_from"`          // request_id that produced this rule
	CreatedAt   time.Time `json:"created_at"`
	AgentKind   string    `json:"agent_kind,omitempty"` // audit display (not matched)
}

// Engine is a concurrency-safe rule set (in-memory projection; the store
// keeps the durable copy).
type Engine struct {
	mu    sync.RWMutex
	rules []Rule
}

// NewEngine creates an empty engine.
func NewEngine() *Engine { return &Engine{} }

// Add appends a rule. Scope/SessionID pairing is validated by callers.
func (e *Engine) Add(r Rule) {
	e.mu.Lock()
	e.rules = append(e.rules, r)
	e.mu.Unlock()
}

// List returns a copy of the rule set.
func (e *Engine) List() []Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// Remove drops the rule with the given id and reports whether it existed.
func (e *Engine) Remove(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.rules {
		if r.ID == id {
			e.rules = append(e.rules[:i], e.rules[i+1:]...)
			return true
		}
	}
	return false
}

// Match returns the most specific allow rule for (tool, arg, sessionID).
//
// Specificity: exact tool beats "*"; a matched arg pattern beats a bare tool.
// Session-scoped rules are filtered out unless SessionID equals their own.
func (e *Engine) Match(tool, arg, sessionID string) (Rule, error) {
	return MatchFirst(e.List(), tool, arg, sessionID)
}

// MatchFirst applies the most-specific-wins matching over an arbitrary rule
// slice (the store-backed path uses this to match fresh rows every gate).
func MatchFirst(rs []Rule, tool, arg, sessionID string) (Rule, error) {
	best := -1
	var out Rule
	found := false

	for _, r := range rs {
		if r.Effect != EffectAllow {
			continue
		}
		if r.Scope == "session" && r.SessionID != sessionID {
			continue
		}

		toolScore := 0
		switch {
		case r.Tool == tool:
			toolScore = 2
		case r.Tool == "*":
			toolScore = 1
		default:
			continue
		}

		patScore := 0
		if r.ArgPattern != "" {
			if !MatchGlob(r.ArgPattern, arg) {
				continue
			}
			patScore = 1
		}

		if score := toolScore*2 + patScore; score > best {
			best = score
			out = r
			found = true
		}
	}
	if !found {
		return Rule{}, ErrNoMatch
	}
	return out, nil
}

// MatchGlob is a literal glob where '*' matches any run of characters
// (including '/') and '?' matches exactly one character.
// It intentionally does not parse shell semantics.
func MatchGlob(pattern, s string) bool {
	pi, si := 0, 0
	star := -1
	match := 0

	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			match = si
			pi++
		case star != -1:
			pi = star + 1
			match++
			si = match
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// ParseKey splits a "<tool>" or "<tool>(<arg_pattern>)" key into its parts.
// It is used when the UI submits a rule key typed by the user.
func ParseKey(key string) (tool, pattern string, err error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", errors.New("rules: empty key")
	}
	open := strings.IndexByte(key, '(')
	if open < 0 {
		return key, "", nil
	}
	if !strings.HasSuffix(key, ")") {
		return "", "", errors.New("rules: unbalanced parenthesis in key")
	}
	tool = strings.TrimSpace(key[:open])
	pattern = key[open+1 : len(key)-1]
	if tool == "" {
		return "", "", errors.New("rules: empty tool in key")
	}
	return tool, pattern, nil
}
