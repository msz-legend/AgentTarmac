#!/usr/bin/env node
// AgentHub — Codex PermissionRequest hook (PRD F3)
//
// Installed into ~/.codex/hooks.json. Codex runs this in the approval path,
// before its own guardian/user approval UI. The hook forwards the request to
// the Hub and blocks until the Hub renders a verdict.
//
// Codex output contract (stdout JSON):
//   allow            → decision.behavior = "allow",  exit 0
//   deny / timeout   → decision.behavior = "deny",   exit 0   (fail closed)
//   hub unreachable  → decision.behavior = "deny",   exit 0   (fail closed)
//
// Options (argv, optional):
//   --url <http://127.0.0.1:8799>   Hub address
//   --token <hex>                  Hub access token
//   --timeout-ms <n>               Max wait for a verdict (default 120000)
//
// Environment fallbacks: AGENTHUB_URL, AGENTHUB_TOKEN, AGENTHUB_TIMEOUT_MS.

import { readFileSync } from "node:fs";

// Codex tool names that never need an approval round-trip. Anything not listed
// here is forwarded to the Hub.
//
// Canonical names come from codex-rs/core/src/tools/hook_names.rs:
//   Bash (shell), apply_patch (aliases Write/Edit), spawn_agent (alias Agent)
// plus the read-only tools Codex exposes.
const LOW_RISK = new Set([
  "read_file", "list_dir", "search", "update_plan", "view_image",
  // Claude Code-style names, in case a matcher alias reaches us.
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

function verdict(behavior, message) {
  const decision = { behavior };
  if (message) decision.message = message;
  process.stdout.write(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: "PermissionRequest",
      decision,
    },
  }));
  process.exit(0);
}

function allow(reason) {
  verdict("allow", reason || "AgentHub: allowed");
}

function deny(reason) {
  const msg = typeof reason === "string" && reason.length ? reason : "AgentHub: denied";
  process.stderr.write(msg + "\n");
  verdict("deny", msg);
}

// --- main -------------------------------------------------------------------

let input;
try {
  input = JSON.parse(readStdin());
} catch {
  // Malformed hook input must never break Codex.
  allow("AgentHub: malformed hook input");
}

const tool = input.tool_name || "";
const toolInput = input.tool_input && typeof input.tool_input === "object" ? input.tool_input : {};

if (!tool) allow("AgentHub: no tool in hook input");
if (LOW_RISK.has(tool)) allow("AgentHub: low-risk tool");

const command = typeof toolInput.command === "string" ? toolInput.command : "";

// apply_patch carries its target in tool_input.path (or changes[].path).
let filePath = typeof toolInput.path === "string" ? toolInput.path : "";
if (!filePath && typeof toolInput.file_path === "string") filePath = toolInput.file_path;
if (!filePath && Array.isArray(toolInput.changes) && toolInput.changes[0] &&
    typeof toolInput.changes[0].path === "string") {
  filePath = toolInput.changes[0].path;
}

const digest = JSON.stringify(toolInput);

const payload = {
  session_id: input.session_id || "",
  tool,
  tool_name: tool,
  command,
  file_path: filePath,
  args_digest: digest.length > 4000 ? digest.slice(0, 4000) : digest,
  cwd: input.cwd || "",
  permission_mode: input.permission_mode || "",
  tool_use_id: input.turn_id || "",
  agent_kind: "codex",
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
