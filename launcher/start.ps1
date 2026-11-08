#!/usr/bin/env pwsh
# AgentTarmac desktop launcher — idempotent. Double-click the desktop shortcut
# and this makes sure the daemon, the two ACP connectors and the browser console
# are all up. Safe to run repeatedly: it never starts a duplicate process.
$ErrorActionPreference = 'Continue'
$Root = 'D:\DevTools\AgentHub'
$Bin  = Join-Path $Root 'bin'
$Data = Join-Path $Root 'data'
$Logs = Join-Path $Root 'launcher\logs'
$TokenFile = Join-Path $Data 'token'
$Port = 8799

New-Item -ItemType Directory -Force -Path $Logs | Out-Null
$LogFile = Join-Path $Logs 'start.log'
function Write-Log([string]$m) {
  try { Add-Content -LiteralPath $LogFile -Value ("[{0}] {1}" -f (Get-Date -Format 's'), $m) -Encoding UTF8 } catch {}
}
function Test-Port([int]$p) {
  $c = $null
  try { $c = New-Object System.Net.Sockets.TcpClient; $c.Connect('127.0.0.1', $p); return $true }
  catch { return $false }
  finally { if ($c) { $c.Dispose() } }
}

Write-Log "=== launch pid=$PID ==="

# 1. Daemon on 127.0.0.1:8799 (production instance, data/ holds its token).
if (-not (Test-Port $Port)) {
  $daemonArgs = @(
    '-addr', '127.0.0.1:8799',
    '-data', $Data,
    '-retention-days', '90',
    '-hook-path', (Join-Path $Bin 'hub-hook.exe'),
    '-task-concurrency', '2'
  )
  try {
    Start-Process -FilePath (Join-Path $Bin 'agenthubd.exe') `
      -WorkingDirectory $Root -ArgumentList $daemonArgs -WindowStyle Hidden | Out-Null
    Write-Log 'daemon: started'
  } catch { Write-Log "daemon: START FAILED $($_.Exception.Message)" }
} else { Write-Log 'daemon: already listening' }

$healthy = $false
for ($i = 0; $i -lt 40; $i++) {
  try {
    $h = Invoke-RestMethod -Uri 'http://127.0.0.1:8799/healthz' -TimeoutSec 2
    if ($h.status -eq 'ok') { $healthy = $true; break }
  } catch {}
  Start-Sleep -Milliseconds 500
}
if (-not $healthy) { Write-Log 'daemon: healthz TIMEOUT — aborting'; exit 1 }
Write-Log "daemon: healthz ok kinds=$($h.agentKinds -join ',')"

if (-not (Test-Path -LiteralPath $TokenFile)) { Write-Log 'token file missing — aborting'; exit 1 }
$Token = (Get-Content -Raw -LiteralPath $TokenFile).Trim()

# 2. Persistent ACP connectors for the two attach-only agent kinds.
# Claude Code and Codex are hook-driven (PreToolUse / PermissionRequest) and
# need no long-lived process; OpenCode and Hermes speak ACP and must be bridged.
# opencode CLI needs its "acp" subcommand; hermes-acp.exe IS the ACP server
# and rejects a positional "acp" (verified: "unrecognized arguments: acp").
$specs = @(
  @{ Kind = 'opencode'; Exe = 'D:\DevTools\npm-global\node_modules\opencode-ai\bin\opencode.exe'; Args = @('acp'); Msys = $false },
  @{ Kind = 'hermes';   Exe = (Join-Path $env:LOCALAPPDATA 'hermes\bin\hermes-acp.exe'); Args = @(); Msys = $true  }
)
foreach ($s in $specs) {
  $marker = "-agent-kind $($s.Kind)"
  $existing = @(Get-CimInstance Win32_Process -Filter "name='agenthub-connect.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -like "*$marker*" -and $_.CommandLine -like '*-daemon http://127.0.0.1:8799*' })
  if ($existing.Count -gt 0) {
    Write-Log "$($s.Kind): connector already running (pid $($existing[0].ProcessId))"
    continue
  }
  if (-not (Test-Path -LiteralPath $s.Exe)) {
    Write-Log "$($s.Kind): SKIP — agent exe missing: $($s.Exe)"
    continue
  }
  $connArgs = @(
    '-daemon', 'http://127.0.0.1:8799',
    '-token', $Token,
    '-agent-kind', $s.Kind,
    '--', $s.Exe
  ) + $s.Args
  try {
    if ($s.Msys) { $env:MSYS_NO_PATHCONV = '1' }
    Start-Process -FilePath (Join-Path $Bin 'agenthub-connect.exe') `
      -WorkingDirectory $Root -ArgumentList $connArgs -WindowStyle Hidden `
      -RedirectStandardError (Join-Path $Logs "$($s.Kind)-connect.err.log") `
      -RedirectStandardOutput (Join-Path $Logs "$($s.Kind)-connect.out.log") | Out-Null
    Write-Log "$($s.Kind): connector started"
  } catch { Write-Log "$($s.Kind): START FAILED $($_.Exception.Message)" }
}

# 3. Open the console as a clean app window (Edge --app). The server injects
# the bearer token into the page (window.__TARMAC_BOOT__), so no login step.
$edges = @(
  'C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe',
  'C:\Program Files\Microsoft\Edge\Application\msedge.exe'
)
$edge = $edges | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
try {
  if ($edge) {
    $profile = Join-Path $Root 'launcher\edge-profile'
    New-Item -ItemType Directory -Force -Path $profile | Out-Null
    Start-Process -FilePath $edge -ArgumentList @(
      "--app=http://127.0.0.1:8799/",
      "--user-data-dir=$profile",
      '--window-size=1440,900',
      '--no-first-run',
      '--no-default-browser-check'
    ) | Out-Null
    Write-Log 'browser: Edge app window opened'
  } else {
    Start-Process 'http://127.0.0.1:8799/' | Out-Null
    Write-Log 'browser: default browser opened (Edge not found)'
  }
} catch { Write-Log "browser: FAILED $($_.Exception.Message)" }

Write-Log '=== launch done ==='
