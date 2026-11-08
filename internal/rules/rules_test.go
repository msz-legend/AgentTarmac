package rules

import (
	"testing"
	"time"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true},
		{"*", "anything at all", true},
		{"rm *", "rm -rf /tmp/x", true},
		{"rm *", "rmdir /s x", false},
		{"git push*", "git push --force origin main", true},
		{"npm install ?", "npm install x", true},
		{"npm install ?", "npm install left-pad", false},
		{"cargo build", "cargo build", true},
		{"cargo build", "cargo build --release", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXbYd", false},
	}
	for _, tc := range cases {
		if got := MatchGlob(tc.pattern, tc.s); got != tc.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func mkRule(id, tool, pattern, scope, sessionID string) Rule {
	return Rule{ID: id, Tool: tool, ArgPattern: pattern, Scope: scope, Effect: EffectAllow, SessionID: sessionID, CreatedAt: time.Now()}
}

func TestMatchSpecificity(t *testing.T) {
	rs := []Rule{
		mkRule("r1", "Bash", "", "always", ""),
		mkRule("r2", "Bash", "rm *", "always", ""),
		mkRule("r3", "*", "cargo*", "session", "s1"),
	}

	// arg pattern beats bare tool.
	if r, err := MatchFirst(rs, "Bash", "rm -rf x", "s1"); err != nil || r.ID != "r2" {
		t.Fatalf("most specific = %+v err=%v", r, err)
	}
	// bare tool fallback.
	if r, err := MatchFirst(rs, "Bash", "npm install", "s1"); err != nil || r.ID != "r1" {
		t.Fatalf("bare tool = %+v err=%v", r, err)
	}
	// session scope filters other sessions.
	if _, err := MatchFirst(rs, "Grep", "anything", "s2"); err == nil {
		t.Fatal("session rule must not match another session")
	}
	if r, err := MatchFirst(rs, "Grep", "cargo build", "s1"); err != nil || r.ID != "r3" {
		t.Fatalf("session rule in own session = %+v err=%v", r, err)
	}
	// no match at all.
	if _, err := MatchFirst(rs, "WebFetch", "https://x", "s9"); err != ErrNoMatch {
		t.Fatalf("no-match error = %v", err)
	}
}

func TestMatchIgnoresDenyAndOtherEffects(t *testing.T) {
	rs := []Rule{{ID: "d1", Tool: "Bash", Scope: "always", Effect: "deny", CreatedAt: time.Now()}}
	if _, err := MatchFirst(rs, "Bash", "anything", "s"); err != ErrNoMatch {
		t.Fatalf("deny rules must not auto-allow in M1: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	tool, pat, err := ParseKey("Bash(rm *)")
	if err != nil || tool != "Bash" || pat != "rm *" {
		t.Fatalf("ParseKey = %q %q %v", tool, pat, err)
	}
	tool, pat, err = ParseKey("Write")
	if err != nil || tool != "Write" || pat != "" {
		t.Fatalf("ParseKey bare = %q %q %v", tool, pat, err)
	}
	if _, _, err := ParseKey("Bash(rm *"); err == nil {
		t.Fatal("unbalanced key must error")
	}
}
