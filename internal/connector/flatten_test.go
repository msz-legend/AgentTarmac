package connector

import (
	"encoding/json"
	"testing"
)

func TestFlattenPromptText(t *testing.T) {
	// ACP content-block array → plain text.
	in := json.RawMessage(`{"sessionId":"s1","prompt":[
		{"type":"text","text":"line one"},
		{"type":"image","data":"..."},
		{"type":"text","text":"line two"}]}`)
	out := flattenPromptText(in)
	var m struct {
		SessionID string `json:"sessionId"`
		Prompt    string `json:"prompt"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m.SessionID != "s1" || m.Prompt != "line one\nline two" {
		t.Fatalf("flatten wrong: %s", out)
	}

	// Already-plain prompt passes through unchanged.
	plain := json.RawMessage(`{"sessionId":"s1","prompt":"hello"}`)
	if out := flattenPromptText(plain); string(out) != string(plain) {
		t.Fatalf("plain prompt mangled: %s", out)
	}
}

func TestMergeJSONKey(t *testing.T) {
	// Absent key gets the default.
	out := mergeJSONKey(json.RawMessage(`{"cwd":"D:/x"}`), "mcpServers", []any{})
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["cwd"] != "D:/x" {
		t.Fatalf("cwd lost: %s", out)
	}
	if arr, ok := m["mcpServers"].([]any); !ok || len(arr) != 0 {
		t.Fatalf("mcpServers default missing: %s", out)
	}

	// Explicit Hub value wins.
	out = mergeJSONKey(json.RawMessage(`{"mcpServers":[{"name":"x"}]}`), "mcpServers", []any{})
	_ = json.Unmarshal(out, &m)
	if arr, ok := m["mcpServers"].([]any); !ok || len(arr) != 1 {
		t.Fatalf("explicit mcpServers overwritten: %s", out)
	}
}
