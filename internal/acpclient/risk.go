package acpclient

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Risk classification (decision C): the Hub monitors passively by default and
// actively intercepts high-risk operations. High-risk operations are never
// auto-approved, even when approval memory holds a previous "allow" verdict.

// highRiskCommandRe matches destructive or privilege-escalating shell commands
// on both Unix and Windows. Case-insensitive.
var highRiskCommandRe = regexp.MustCompile(`(?i)(` + strings.Join([]string{
	// Recursive / forced deletion
	`\brm\s+(-[a-z]*[rf][a-z]*\s+)+`,
	`\bdel\s+(/f|\s)*/?[fsq]`,
	`\brmdir\s+/s`,
	`remove-item\b[^|;&]*(-recurse|-force)`,
	// Disk / filesystem destruction
	`\bformat(\s+[a-z]:|\s/|$)`,
	`\bdiskpart\b`,
	`\bmkfs\b`,
	`\bdd\s+if=`,
	`\bcipher\s+/w`,
	// System power / process killing
	`\bshutdown\b`,
	`stop-computer\b`,
	`restart-computer\b`,
	`\btaskkill\s+/f`,
	`\bkill\s+-9\b`,
	`\bpkill\b`,
	// Registry / boot / services
	`\breg\s+(add|delete|import|restore|load)\b`,
	`\bregedit(\s|/|$)`,
	`\bbcdedit\b`,
	`\bset-namedsecurityinfo\b`,
	`\bicacls\b[^|;&]*(/grant|/remove|/reset)`,
	// Escalation / obfuscation
	`-encodedcommand\b`,
	`invoke-expression\b`,
	`\biex\s*\(`,
	`set-executionpolicy\b`,
	`\bchmod\s+777\b`,
	`\bchown\s+-r\b`,
	// Remote code execution via pipe
	`\|\s*(sudo\s+)?(ba)?sh\b`,
	`\|\s*(iex|invoke-expression|powershell\s+-)\b`,
	`invoke-webrequest\b[^|;&]*\|\s*iex\b`,
	`curl\b[^|;&]*\|\s*(ba)?sh\b`,
	`wget\b[^|;&]*\|\s*(ba)?sh\b`,
	// Git history / force operations
	`git\s+push\s+(--force|-f)\b`,
	`git\s+reset\s+--hard\b`,
	`git\s+clean\s+-[a-z]*f`,
	// Event log / scheduled tasks tampering
	`wevtutil\s+cl`,
	`vssadmin\s+delete`,
	`schtasks\s+/create\b`,
	`at\s+\d{1,2}:\d{2}\b`,
}, `|`) + `)`)

// readOnlySingle are single-token read-only commands; "cat file" etc. stays
// low risk as long as the command carries no | > & ; metacharacters.
var readOnlySingle = map[string]bool{
	"ls": true, "dir": true, "pwd": true, "cat": true, "type": true,
	"head": true, "tail": true, "wc": true, "stat": true, "whoami": true,
	"hostname": true, "which": true, "where": true, "echo": true, "date": true,
}

// readOnlyCommands are terminal invocations classified as low risk so that
// passive monitoring (decision C) does not prompt on harmless reads.
var readOnlyCommands = map[string]bool{
	"ls": true, "dir": true, "pwd": true, "cat": true, "type": true,
	"head": true, "tail": true, "wc": true, "file": true, "stat": true,
	"whoami": true, "hostname": true, "echo": true, "printenv": true,
	"which": true, "where": true, "date": true, "env": true,
	"git status": true, "git log": true, "git diff": true, "git show": true,
	"git branch": true, "git remote": true,
	"go version": true, "go list": true, "go vet": true, "go env": true,
	"node -v": true, "node --version": true, "npm ls": true, "npm list": true,
	"python --version": true, "python -v": true, "pip list": true,
	"dotnet --version": true, "cargo --version": true, "rustc --version": true,
}

// readLikeTools never mutate state; they are always low risk.
var readLikeTools = map[string]bool{
	"read": true, "view": true, "grep": true, "glob": true, "search": true,
	"find": true, "list": true, "ls": true, "think": true, "todo": true,
	"webfetch": true, "websearch": true,
}

// terminalToolNames are tools that execute shell commands.
var terminalToolNames = map[string]bool{
	"bash": true, "shell": true, "terminal": true, "exec": true,
	"execute": true, "command": true, "cmd": true, "powershell": true,
	"pwsh": true, "run_command": true, "runcommand": true,
}

// writeLikeTools mutate files or state; medium risk unless the command itself
// is high risk.
var writeLikeTools = map[string]bool{
	"edit": true, "write": true, "create": true, "multiedit": true,
	"notebookedit": true, "move": true, "copy": true, "mkdir": true,
	"touch": true, "delete": true, "rename": true,
	// Codex / OpenAI-style names
	"apply_patch": true, "applypatch": true, "str_replace_editor": true,
	"str_replace_based_edit_tool": true,
}

// RiskLevel is the classification of a tool call.
type RiskLevel string

// ClassifyRisk returns the risk level of a tool call. toolKind follows the ACP
// tool kinds (read, edit, delete, move, search, execute, think, fetch, other);
// rawInput may be nil.
func ClassifyRisk(toolName, toolKind string, rawInput json.RawMessage) RiskLevel {
	name := strings.ToLower(strings.TrimSpace(toolName))
	kind := strings.ToLower(strings.TrimSpace(toolKind))

	if kind == "read" || kind == "search" || kind == "think" || kind == "fetch" {
		return RiskLow
	}
	if readLikeTools[name] {
		return RiskLow
	}
	if kind == "delete" {
		return RiskHigh
	}

	if terminalToolNames[name] || kind == "execute" {
		cmd := extractCommand(rawInput)
		if cmd != "" {
			if highRiskCommandRe.MatchString(cmd) {
				return RiskHigh
			}
			// Passive monitoring only for clean read-only invocations: no
			// redirection, pipe or chaining metacharacters.
			if !strings.ContainsAny(cmd, "|>&;") {
				fields := strings.Fields(cmd)
				first2 := strings.Join(fields, " ")
				if len(fields) > 2 {
					first2 = strings.Join(fields[:2], " ")
				}
				lower := strings.ToLower(cmd)
				lowerFirst2 := strings.ToLower(first2)
				lowerFirst1 := strings.ToLower(fields[0])
				if readOnlyCommands[lowerFirst2] || readOnlyCommands[lower] || readOnlySingle[lowerFirst1] {
					return RiskLow
				}
			}
		}
		return RiskMedium
	}

	if writeLikeTools[name] || kind == "edit" || kind == "move" {
		return RiskMedium
	}
	return RiskMedium
}

// extractCommand pulls the shell command out of a tool call input payload.
// Recognizes the common "command"/"cmd" fields.
func extractCommand(rawInput json.RawMessage) string {
	if len(rawInput) == 0 {
		return ""
	}
	var payload struct {
		Command string `json:"command"`
		Cmd     string `json:"cmd"`
	}
	if err := json.Unmarshal(rawInput, &payload); err != nil {
		return ""
	}
	if payload.Command != "" {
		return payload.Command
	}
	return payload.Cmd
}

// CommandPattern derives the approval-memory key component for a tool call
// (decision B: memory key is (agent_kind, tool, command_pattern)).
//
//   - terminal tools: first two whitespace-separated tokens of the command,
//     lowercased for case-insensitive matching (e.g. "git push", "npm install")
//   - file tools: the file_path field verbatim
//   - everything else: "*"
func CommandPattern(toolName string, rawInput json.RawMessage) string {
	name := strings.ToLower(strings.TrimSpace(toolName))
	if terminalToolNames[name] {
		cmd := extractCommand(rawInput)
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			return "*"
		}
		if len(fields) > 2 {
			fields = fields[:2]
		}
		for i := range fields {
			fields[i] = strings.ToLower(fields[i])
		}
		return strings.Join(fields, " ")
	}

	if len(rawInput) != 0 {
		var payload struct {
			FilePath string `json:"file_path"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(rawInput, &payload); err == nil {
			if payload.FilePath != "" {
				return payload.FilePath
			}
			if payload.Path != "" && (writeLikeTools[name] || readLikeTools[name]) {
				return payload.Path
			}
		}
	}
	return "*"
}
