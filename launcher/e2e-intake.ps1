param([string]$Kind = 'claude_code')
$ErrorActionPreference='Stop'
# Background half of the four-kind approval sweep: fire ONE blocking hook
# intake tagged with this agent_kind and print the daemon's verdict.
$tok = (Get-Content -Raw -LiteralPath 'D:\DevTools\AgentHub\data\token').Trim()
$H = @{ Authorization = "Bearer $tok"; 'Content-Type' = 'application/json' }
$body = @{
  session_id  = "desktop-sweep-$Kind"
  tool        = 'Bash'
  tool_name   = 'Bash'
  command     = 'git push --force origin main'   # high-risk per risk.go
  args_digest = '{"command":"force-push"}'
  cwd         = 'D:\DevTools\AgentHub'
  agent_kind  = $Kind
} | ConvertTo-Json -Compress
"INTAKE[$Kind] starting pid=$PID"
try {
  $v = Invoke-RestMethod 'http://127.0.0.1:8799/api/hook/intake' -Method Post -Headers $H -Body $body -TimeoutSec 120
  "INTAKE[$Kind] VERDICT: " + ($v | ConvertTo-Json -Compress)
} catch {
  "INTAKE[$Kind] ERR: $($_.Exception.Message)"
}
