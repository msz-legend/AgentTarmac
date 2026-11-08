package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ManagedMarker identifies our hook command inside settings.json / hooks.json
// so uninstall only removes our own entry (E: 参考实现同语义).
const ManagedMarker = "hub-hook"

// install registers the hook in Claude Code's settings.json (PreToolUse) or
// Codex's hooks.json (PermissionRequest), merging with unrelated keys.
func install(o options) error {
	command := fmt.Sprintf("%q --url %q --token %q --timeout-ms %d",
		selfPath(), o.url, o.token, o.timeoutMS)
	if o.codex {
		return mergeJSON(codexHooksPath(), map[string]any{
			"PermissionRequest": []any{map[string]any{
				"matcher": "*",
				"hooks":   []any{map[string]any{"type": "command", "command": command, "timeout": int(o.timeoutMS/1000) + 30}},
			}},
		}, "PermissionRequest")
	}
	return mergeJSON(ccSettingsPath(), map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "",
				"hooks":   []any{map[string]any{"type": "command", "command": command}},
			}},
		},
	}, "hooks")
}

// uninstall removes entries whose command contains ManagedMarker.
func uninstall(o options) error {
	if o.codex {
		return purgeJSON(codexHooksPath(), "PermissionRequest")
	}
	return purgeJSON(ccSettingsPath(), "hooks")
}

func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ManagedMarker
	}
	return exe
}

func ccSettingsPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude", "settings.json")
	}
	return filepath.Join(".claude", "settings.json")
}

func codexHooksPath() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return filepath.Join(v, "hooks.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex", "hooks.json")
	}
	return filepath.Join(".codex", "hooks.json")
}

// mergeJSON deep-merges patch into the JSON document at path, creating it
// when missing. Arrays under top are REPLACED by the patch's arrays after
// filtering out our own managed entries (idempotent install).
func mergeJSON(path string, patch map[string]any, topKey string) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := purgeManaged(doc, topKey); err != nil {
		return err
	}
	for k, v := range patch {
		doc[k] = v
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// purgeJSON removes managed entries under topKey, deleting empty containers.
func purgeJSON(path, topKey string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if err := purgeManaged(doc, topKey); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// purgeManaged strips hook entries containing ManagedMarker from the
// document's topKey container.
func purgeManaged(doc map[string]any, topKey string) error {
	// CC: doc.hooks.PreToolUse = [group{matcher, hooks:[{command}]}]
	// Codex: doc.PermissionRequest = [group{matcher, hooks:[{command}]}]
	var container any
	if topKey == "hooks" {
		hooks, ok := doc["hooks"].(map[string]any)
		if !ok {
			return nil
		}
		container = hooks["PreToolUse"]
		defer func() { hooks["PreToolUse"] = container }()
	} else {
		container = doc[topKey]
		defer func() { doc[topKey] = container }()
	}
	groups, ok := container.([]any)
	if !ok {
		return nil
	}
	kept := make([]any, 0, len(groups))
	for _, g := range groups {
		group, _ := g.(map[string]any)
		hooks, _ := group["hooks"].([]any)
		inner := make([]any, 0, len(hooks))
		for _, h := range hooks {
			hook, _ := h.(map[string]any)
			cmd, _ := hook["command"].(string)
			if !containsMarker(cmd) {
				inner = append(inner, h)
			}
		}
		if len(inner) > 0 {
			group["hooks"] = inner
			kept = append(kept, group)
		}
	}
	container = kept
	return nil
}

func containsMarker(command string) bool {
	return len(command) >= len(ManagedMarker) && indexOf(command, ManagedMarker) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
