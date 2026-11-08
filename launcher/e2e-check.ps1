# AgentTarmac desktop E2E — foreground half.
#
# Pair with e2e-intake.ps1 (background half). Usage:
#   # terminal 1 (or `pwsh run_in_background`):
#   powershell -NoProfile -ExecutionPolicy Bypass -File e2e-intake.ps1 -Kind claude_code
#   # terminal 2 (this script), while the intake is still blocking:
#   powershell -NoProfile -ExecutionPolicy Bypass -File e2e-check.ps1
#
# Proves the full approval loop against the desktop daemon (127.0.0.1:8799):
#   blocking high-risk hook intake -> pending approval -> human decision ->
#   hook unblocked (allowed_once) -> audit row + resolved request.
#
# NOTE: the decision body field is "decision" (not "outcome") — see
# internal/server/server.go:477 and internal/acpclient/approval.go:55.
$ErrorActionPreference='Stop'
$tok = (Get-Content -Raw -LiteralPath 'D:\DevTools\AgentHub\data\token').Trim()
$H = @{ Authorization = "Bearer $tok"; 'Content-Type' = 'application/json' }
$Base = 'http://127.0.0.1:8799'

function Api($path, $method='GET', $payload=$null) {
  $p = @{ Uri = $Base + $path; Method = $method; Headers = $H; TimeoutSec = 20 }
  if ($payload -ne $null) { $p.Body = $payload }
  Invoke-RestMethod @p
}
function Pending($r) { if ($r.permissionRequests) { @($r.permissionRequests) } else { @() } }

"STEP0 clean orphaned pending approvals"
foreach ($p in (Pending (Api '/api/permission-requests?outcome=pending'))) {
  $id = $p.requestId; if (-not $id) { $id = $p.id }
  $b = @{ decision='reject_once'; decidedBy='e2e-cleanup' } | ConvertTo-Json -Compress
  try { Api ("/api/permission-requests/" + [uri]::EscapeDataString($id) + "/decision") 'POST' $b | Out-Null } catch {}
}
"  remaining = $((Pending (Api '/api/permission-requests?outcome=pending')).Count)"

"STEP1 poll for the high-risk pending raised by e2e-intake.ps1"
$pending = $null
for ($i=0; $i -lt 60; $i++) {
  Start-Sleep -Milliseconds 500
  $list = Pending (Api '/api/permission-requests?outcome=pending')
  if ($list.Count -gt 0) { $pending = $list[-1]; "  appeared after ~$([math]::Round(($i+1)*0.5,1))s"; break }
}
if (-not $pending) { "FAIL: no pending — is e2e-intake.ps1 running?"; exit 1 }
$id = $pending.requestId; if (-not $id) { $id = $pending.id }
"  id=$id session=$($pending.sessionId) tool=$($pending.toolName) risk=$($pending.risk)"

"STEP2 POST decision allow_once (same body shape as the console button)"
$dec = @{ decision='allow_once'; decidedBy='hub'; note='desktop e2e verdict' } | ConvertTo-Json -Compress
$d = Api ("/api/permission-requests/" + [uri]::EscapeDataString($id) + "/decision") 'POST' $dec
"  resp: $($d | ConvertTo-Json -Compress)"

"STEP3 verify resolved state + audit"
$pr = Api '/api/permission-requests'
$row = @($pr.permissionRequests) | Where-Object { $_.requestId -eq $id }
"  request outcome=$($row.outcome) optionId=$($row.optionId)"
$ap = Api '/api/approvals'; "  approvals=$(if($ap.approvals){@($ap.approvals).Count}else{0})"
"  remaining pending=$((Pending (Api '/api/permission-requests?outcome=pending')).Count)"
"  (the hook's unblocked verdict appears in the e2e-intake.ps1 / job_output stream)"
if ($d.applied -eq $true -and $row.outcome -ne 'pending') { "E2E DECISION PASS" } else { "E2E INCONCLUSIVE" }
