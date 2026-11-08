#!/usr/bin/env node
// AgentHub — Claude Code PreToolUse hook (PRD F3)
//
// Installed into Claude Code's hooks config. Claude Code runs this before
// every tool use; the hook forwards the call to the Hub and blocks until the
// Hub renders a verdict.
//
// Verdict handling:
//   allow          → JSON allow + exit 0
//   deny/timeout   → JSON deny  + exit 2   (fail closed)
//   hub unreachable→ JSON deny  + exit 2   (fail closed)
//
// Options (argv, optional):
//   --url <http://127.0.0.1:8799>   Hub address
//   --token <hex>                  Hub access token
//   --timeout-ms <n>               Max wait for a verdict (default 120000)
//
// Environment fallbacks: AGENTHUB_URL, AGENTHUB_TOKEN, AGENTHUB_TIMEOUT_MS.

import { readFileSync } from "node:fs";

const LOW_RISK = new Set([
  "Read", "Grep", "Glob", "LS", "TodoRead", "TodoWrite", "NotebookRead",
]);

function arg(name, fallback) {
  const i = process.argv.indexOf(name);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}

const DAEMON = arg("--url", process.env.AGENTHUB_URL || "http://127.0.0.1:8799").replace(/\/+$/, "");
const TOKEN = arg("--token", process.env.AGENTHUB_TOKEN || "");
const TIMEOUT_MS = Number(arg("--timeout-ms", process.env.AGENTHUB_TIMEOUT_MS || "120000"));

function readStdin() {
  try {
    return readFileSync(0, "utf8");
  } catch {
    return "";
  }
}

function allow(reason) {
  process.stdout.write(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "allow",
      permissionDecisionReason: reason || "AgentHub: allowed",
    },
  }));
  process.exit(0);
}

function deny(reason) {
  const msg = typeof reason === "string" && reason.length ? reason : "AgentHub: denied";
  process.stderr.write(msg + "\n");
  // Exit 2 is Claude Code's "block this tool call" signal; the JSON is a
  // belt-and-braces copy for builds that prefer hookSpecificOutput.
  try {
    process.stdout.write(JSON.stringify({
      hookSpecificOutput: {
        hookEventName: "PreToolUse",
        permissionDecision: "deny",
        permissionDecisionReason: msg,
      },
    }));
  } catch { /* stdout may be closed */ }
  process.exit(2);
}

// --- main -------------------------------------------------------------------

let input;
try {
  input = JSON.parse(readStdin());
} catch {
  // Malformed hook input must never break Claude Code.
  allow("AgentHub: malformed hook input");
}

const tool = input.tool_name || input.tool || "";
const toolInput = input.tool_input && typeof input.tool_input === "object" ? input.tool_input : {};

if (!tool) allow("AgentHub: no tool in hook input");
if (LOW_RISK.has(tool)) allow("AgentHub: low-risk tool");

const command = typeof toolInput.command === "string" ? toolInput.command : "";
const filePath = typeof toolInput.file_path === "string" ? toolInput.file_path : "";
const digest = JSON.stringify(toolInput);

const payload = {
  session_id: input.session_id || "",
  tool,
  command,
  file_path: filePath,
  args_digest: digest.length > 4000 ? digest.slice(0, 4000) : digest,
  cwd: input.cwd || "",
  permission_mode: input.permission_mode || "",
  tool_use_id: input.tool_use_id || "",
  agent_kind: "claude_code",
};

const ctrl = new AbortController();
const timer = setTimeout(() => ctrl.abort(), TIMEOUT_MS);

let res;
try {
  res = await fetch(`${DAEMON}/api/hook/intake`, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      ...(TOKEN ? { authorization: `Bearer ${TOKEN}` } : {}),
    },
    body: JSON.stringify(payload),
    signal: ctrl.signal,
  });
} catch (e) {
  clearTimeout(timer);
  deny(`AgentHub: hub unreachable (${DAEMON}): ${e.message}`);
} finally {
  clearTimeout(timer);
}

if (!res.ok) {
  const text = await res.text().catch(() => "");
  deny(`AgentHub: hub intake failed ${res.status} ${text}`);
}

let decision;
try {
  decision = await res.json();
} catch {
  deny("AgentHub: hub returned an unreadable verdict");
}

if (decision.outcome === "allowed_once") {
  const scope = decision.rule_scope && decision.rule_scope !== "none"
    ? ` (rule_scope=${decision.rule_scope})`
    : "";
  const via = decision.note ? ` — ${decision.note}` : "";
  allow(`AgentHub: allowed${scope}${via}`);
}

deny(`AgentHub: denied${decision.note ? ` — ${decision.note}` : ""}`);
