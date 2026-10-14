param([string]$MonitorId, [string]$BaseUrl = "http://127.0.0.1:8080")
$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$reportDir = Join-Path $repoRoot ".cache"
New-Item -ItemType Directory -Force -Path $reportDir | Out-Null
$monitors = Invoke-RestMethod "$BaseUrl/api/monitors"
if ($MonitorId) { $monitors = @($monitors | Where-Object { $_.id -eq $MonitorId }) }
if (-not $monitors.Count) { throw "No matching monitor. Start a monitor first." }
$runs = @($monitors | ForEach-Object { Invoke-RestMethod "$BaseUrl/api/monitors/$($_.id)" })
$observations = @($runs | ForEach-Object {
    $run = $_
    $alerts = @($run.alerts | Where-Object { $null -ne $_ })
    $latencies = @($alerts | ForEach-Object { [long]$_.signal_to_alert_ms } | Sort-Object)
    $inputTokens = 0L; $outputTokens = 0L
    foreach ($investigation in $run.investigations) {
        if ($investigation.review) {
            $inputTokens += [long]$investigation.review.input_tokens
            $outputTokens += [long]$investigation.review.output_tokens
        }
    }
    $p95 = if ($latencies.Count) { $latencies[[int][Math]::Ceiling(.95 * $latencies.Count) - 1] } else { $null }
    [ordered]@{
        id = $run.id; url = $run.target_url; goal = $run.goal; goal_version = $run.goal_version
        source_mode = $run.source_mode; status = $run.status
        signal_count = if ($run.events) { @($run.events).Count } else { 0 }
        alert_count = $alerts.Count
        input_tokens = $inputTokens; output_tokens = $outputTokens
        signal_to_backend_alert_p95_ms = $p95
        latest_capture_interval_ms = $run.rooms[0].capture_interval_ms
        history_truncated = $run.history_truncated
        sampling = $run.sampling
    }
})
$report = [ordered]@{
    recorded_at = [DateTimeOffset]::UtcNow.ToString("o")
    measurement = "Saved evidence to backend alert; browser display time is not measured. Correctness must be labeled by the user. Token sums cover retained investigations only."
    summary = $observations; runs = $runs
}
$file = Join-Path $reportDir ("monitor-acceptance-" + (Get-Date -Format "yyyyMMdd-HHmmss") + ".json")
$report | ConvertTo-Json -Depth 32 | Set-Content -LiteralPath $file -Encoding UTF8
Write-Host "Saved local report: $file"
Write-Host "Contains livestream URLs and recognized text; .cache is excluded from Git."
