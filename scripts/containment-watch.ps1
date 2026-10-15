param(
  [string]$CoreUrl = "http://localhost:8083",
  [string]$EdgeUrl = "http://10.222.111.245:8081",
  [int]$DurationMin = 120,
  [int]$PollSec = 5,
  [string]$Report = "$env:TEMP\opencode\containment-watch-report.txt"
)

$ErrorActionPreference = "Continue"
$stop = (Get-Date).AddMinutes($DurationMin)
$seen = @{}          # dedupe of log lines
$heldSeen = @{}      # bin_id -> held (for held-set diff)
$flagSeen = @{}      # payload_code -> active (flag diff)
$script:fail = 0

function Log([string]$msg) {
  $line = "[{0}] {1}" -f (Get-Date -Format "HH:mm:ss"), $msg
  Write-Host $line
  Add-Content -LiteralPath $Report -Value $line
}

function Login($url) {
  $sess = New-Object Microsoft.PowerShell.Commands.WebRequestSession
  try {
    Invoke-WebRequest -Uri "$url/login" -Method Post -Body @{ username = "admin"; password = "admin" } `
      -WebSession $sess -UseBasicParsing -TimeoutSec 15 | Out-Null
    return $sess
  } catch {
    Log "LOGIN FAILED for ${url}: $($_.Exception.Message)"
    return $null
  }
}

function Get-JSON($sess, $url) {
  try {
    $r = Invoke-WebRequest -Uri $url -WebSession $sess -UseBasicParsing -TimeoutSec 15
    return ($r.Content | ConvertFrom-Json)
  } catch { return $null }
}

function Get-DiagLines($sess, $url, $subsystem) {
  try {
    $r = Invoke-WebRequest -Uri "$url/diagnostics?subsystem=$subsystem" -WebSession $sess -UseBasicParsing -TimeoutSec 15
    $t = $r.Content -replace '&#34;', '"' -replace '&#39;', "'" -replace '&lt;', '<' -replace '&gt;', '>' -replace '&amp;', '&'
    return [regex]::Matches($t, 'containment:[^<]{0,220}') | ForEach-Object { $_.Value } | Select-Object -Unique
  } catch { return @() }
}

function Check-HeldBin($coreSess, $binId, $holdBy, $holdAt) {
  $d = Get-JSON $coreSess "$CoreUrl/api/bins/detail?id=$binId"
  if (-not $d) { Log "bin $binId : detail read FAILED (skipping leak check)"; return }
  $label = $d.bin.label; $payload = $d.bin.payload_code; $node = $d.bin.node_name
  Log ("bin {0} '{1}' HELD at {2} (payload {3}, by {4})" -f $binId, $label, $node, $payload, $holdBy)
  if (-not $d.audit) { Log "bin $binId : no audit rows"; return }
  $leaks = @()
  foreach ($a in $d.audit) {
    if ($a.action -in @("claimed", "reserved") -and $a.created_at -gt $holdAt) {
      $leaks += "  LEAK? bin $binId audit: $($a.action) '$($a.new_value)' at $($a.created_at) (after hold $($holdAt))"
    }
  }
  if ($leaks.Count) { $script:fail += $leaks.Count; $leaks | ForEach-Object { Log $_ } }
  else { Log "bin $binId : no claims after the hold - leak check PASS" }
}

if (Test-Path $Report) { Remove-Item $Report -Force }
Log "containment watcher: core=$CoreUrl edge=$EdgeUrl poll=${PollSec}s duration=${DurationMin}min"
Log "report: $Report"

$core = Login $CoreUrl
$edge = Login $EdgeUrl
if (-not $core) { Log "cannot continue without core"; exit 1 }
Log "logins ok"

while ((Get-Date) -lt $stop) {
  # 1) containment log lines, dispatch + engine subsystems
  foreach ($sub in @("dispatch", "engine")) {
    foreach ($l in (Get-DiagLines $core $sub)) {
      $key = "$sub|$l"
      if (-not $seen.ContainsKey($key)) {
        $seen[$key] = $true
        $marker = if ($l -match "hold marker stamped") { "  << FIX-1 EVIDENCE" }
          elseif ($l -match "diverted") { "  << DIVERT" }
          elseif ($l -match "WARN") { "  << WARN (fail-open path - note this)" }
          else { "" }
        Log "[$sub] $l$marker"
      }
    }
  }

  # 2) containment state: flag diff + held-bins diff
  $st = Get-JSON $core "$CoreUrl/api/containment"
  if ($st) {
    foreach ($c in $st.containment) {
      $was = $flagSeen[$c.payload_code]
      if ($null -eq $was -or $was -ne $c.active) {
        $state = if ($c.active) { "ACTIVATED (reason: $($c.reason), by $($c.activated_by))" } else { "DEACTIVATED (by $($c.deactivated_by))" }
        Log "flag $($c.payload_code): $state"
        $flagSeen[$c.payload_code] = $c.active
      }
    }
    foreach ($b in $st.held_bins) {
      $was = $heldSeen[$b.bin_id]
      if (-not $was) {
        $heldSeen[$b.bin_id] = $true
        Check-HeldBin $core $b.bin_id $b.hold_by $b.hold_at
      }
    }
    foreach ($k in @($heldSeen.Keys)) {
      $still = $st.held_bins | Where-Object { $_.bin_id -eq $k }
      if (-not $still) { Log "bin $k : hold marker CLEARED (released/unheld)"; $heldSeen.Remove($k) }
    }
  }

  Start-Sleep -Seconds $PollSec
}

Log "=== WATCH WINDOW ENDED. leak-check failures: $script:fail ==="
