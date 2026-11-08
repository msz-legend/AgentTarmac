// qa/fake-agent.mjs — ACP fake agent for connector e2e (E2E-01 / phone E2E).
// Usage: node fake-agent.mjs [low|medium|high]
//   low/medium/high pick the tool command the agent asks permission for on
//   its first turn, so QA can trigger each risk tier from a live session.
import readline from 'node:readline';

const tier = (process.argv[2] || 'medium').toLowerCase();
const COMMANDS = {
  low: 'echo qa-fake-agent',
  medium: 'cargo build --release',
  high: 'rm -rf D:/tmp/qa-fake-target',
};
const command = COMMANDS[tier] || COMMANDS.medium;

const rl = readline.createInterface({ input: process.stdin, terminal: false });
const write = (o) => process.stdout.write(JSON.stringify(o) + '\n');
process.stderr.write(`[fake-agent] tier=${tier} command=${command}\n`);

let prompting = false;
rl.on('line', (line) => {
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (prompting && msg.id === 'agent-perm-1') {
    // Permission resolution from the Hub (via connector option mapping).
    prompting = false;
    const outcome = (msg.result && msg.result.outcome) || {};
    process.stderr.write(`[fake-agent] permission outcome=${JSON.stringify(outcome)}\n`);
    write({ jsonrpc: '2.0', method: 'session/update', params: { sessionId: 'fake-sess-1', status: 'running' } });
    write({ jsonrpc: '2.0', id: lastPromptId, result: { stopReason: 'end_turn' } });
    return;
  }
  switch (msg.method) {
    case 'initialize':
      write({ jsonrpc: '2.0', id: msg.id, result: { protocolVersion: 'acp/v1' } });
      break;
    case 'session/new':
      write({ jsonrpc: '2.0', id: msg.id, result: { sessionId: 'fake-sess-1' } });
      break;
    case 'session/prompt':
      prompting = true;
      lastPromptId = msg.id;
      write({
        jsonrpc: '2.0', id: 'agent-perm-1', method: 'session/request_permission',
        params: {
          sessionId: 'fake-sess-1',
          toolCall: { toolCallId: 'tc-perm-1', title: 'shell', kind: 'execute', rawInput: { command } },
          options: [
            { optionId: 'proceed_once', name: 'Allow once', kind: 'allow_once' },
            { optionId: 'cancel', name: 'Reject', kind: 'reject_once' },
          ],
        },
      });
      break;
    default:
      if (msg.id !== undefined) {
        write({ jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: 'fake-agent: unsupported ' + msg.method } });
      }
  }
});
let lastPromptId = null;
