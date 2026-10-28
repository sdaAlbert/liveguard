[CmdletBinding()]
param(
  [string]$MetricsUrl = "http://127.0.0.1:8080/api/metrics",
  [string]$ProcessName = "liveguard",
  [int]$ProcessId = 0,
  [ValidateRange(1, 3600)]
  [int]$IntervalSeconds = 5,
  [ValidateRange(0, 1440)]
  [int]$DurationMinutes = 0,
  [string]$OutputDirectory = ""
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
  $stamp = Get-Date -Format "yyyyMMdd-HHmmss"
  $repoRoot = Split-Path -Parent $PSScriptRoot
  $OutputDirectory = Join-Path $repoRoot "artifacts/runtime-monitoring/$stamp"
}
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null

$resourceCsv = Join-Path $OutputDirectory "resource-samples.csv"
$queueCsv = Join-Path $OutputDirectory "queue-samples.csv"
$snapshotJsonl = Join-Path $OutputDirectory "metrics-snapshots.jsonl"

function Add-CsvRow {
  param([string]$Path, [object]$Row)
  if (Test-Path -LiteralPath $Path) {
    $Row | Export-Csv -LiteralPath $Path -NoTypeInformation -Append -Encoding UTF8
  } else {
    $Row | Export-Csv -LiteralPath $Path -NoTypeInformation -Encoding UTF8
  }
}

function Get-LiveGuardProcesses {
  if ($ProcessId -gt 0) {
    return @(Get-Process -Id $ProcessId -ErrorAction SilentlyContinue)
  }
  return @(Get-Process -Name $ProcessName -ErrorAction SilentlyContinue | Sort-Object Id)
}

Write-Host "LiveGuard runtime sampler started."
Write-Host "Metrics: $MetricsUrl"
Write-Host "Output:  $OutputDirectory"
Write-Host "Interval: ${IntervalSeconds}s. Press Ctrl+C to stop."
Write-Host "The process name is '$ProcessName'; pass -ProcessId to select a specific process."
$stopAt = if ($DurationMinutes -gt 0) { (Get-Date).AddMinutes($DurationMinutes) } else { $null }
if ($stopAt) { Write-Host "Sampling will stop at $($stopAt.ToString('o'))." }

while (-not $stopAt -or (Get-Date) -lt $stopAt) {
  $sampleTime = [DateTimeOffset]::Now.ToString("o")
  $metrics = $null
  $metricsError = ""
  try {
    $metrics = Invoke-RestMethod -Uri $MetricsUrl -Method Get -TimeoutSec 5
    $json = $metrics | ConvertTo-Json -Depth 12 -Compress
    [System.IO.File]::AppendAllText($snapshotJsonl, $json + [Environment]::NewLine, [System.Text.UTF8Encoding]::new($false))
  } catch {
    $metricsError = $_.Exception.Message
  }

  $processes = Get-LiveGuardProcesses
  $workingSet = $null
  $privateBytes = $null
  $cpuSeconds = $null
  $osThreads = $null
  $handles = $null
  $processIds = ""
  if ($processes.Count -gt 0) {
    $workingSet = [int64](($processes | Measure-Object -Property WorkingSet64 -Sum).Sum)
    $privateBytes = [int64](($processes | Measure-Object -Property PrivateMemorySize64 -Sum).Sum)
    $cpuSeconds = [double](($processes | Measure-Object -Property CPU -Sum).Sum)
    $handles = [int](($processes | Measure-Object -Property Handles -Sum).Sum)
    $threadTotal = 0
    foreach ($process in $processes) { $threadTotal += $process.Threads.Count }
    $osThreads = $threadTotal
    $processIds = (@($processes | ForEach-Object { $_.Id }) -join ";")
  }

  $go = if ($metrics) { $metrics.go_runtime } else { $null }
  $queues = if ($metrics -and $metrics.queues) { @($metrics.queues) } else { @() }
  $reviewing = @($queues | Where-Object { $_.reviewing }).Count
  $calls = if ($metrics) { $metrics.model_calls } else { $null }
  $resourceRow = [pscustomobject]@{
    sampled_at = $sampleTime
    metrics_ok = [bool]$metrics
    metrics_error = $metricsError
    process_ids = $processIds
    process_count = $processes.Count
    process_working_set_bytes = $workingSet
    process_private_bytes = $privateBytes
    process_cpu_seconds = $cpuSeconds
    process_os_threads = $osThreads
    process_handles = $handles
    go_goroutines = if ($go) { $go.goroutines } else { $null }
    go_heap_alloc_bytes = if ($go) { $go.heap_alloc_bytes } else { $null }
    go_heap_inuse_bytes = if ($go) { $go.heap_inuse_bytes } else { $null }
    go_heap_sys_bytes = if ($go) { $go.heap_sys_bytes } else { $null }
    go_sys_bytes = if ($go) { $go.sys_bytes } else { $null }
    go_total_alloc_bytes = if ($go) { $go.total_alloc_bytes } else { $null }
    go_num_gc = if ($go) { $go.num_gc } else { $null }
    queue_waiting = if ($metrics) { $metrics.queue_waiting } else { $null }
    queue_oldest_wait_ms = if ($metrics) { $metrics.queue_oldest_wait_ms } else { $null }
    queues_reviewing = if ($metrics) { $reviewing } else { $null }
    model_calls_24h = if ($calls) { $calls.calls } else { $null }
    model_failures_24h = if ($calls) { $calls.failures } else { $null }
    input_tokens_24h = if ($calls) { $calls.input_tokens } else { $null }
    output_tokens_24h = if ($calls) { $calls.output_tokens } else { $null }
    estimated_cost_24h = if ($calls) { $calls.estimated_cost } else { $null }
    alert_samples_24h = if ($metrics) { $metrics.alert_samples } else { $null }
    avg_received_to_alert_ms = if ($metrics) { $metrics.average_received_to_alert_ms } else { $null }
  }
  Add-CsvRow -Path $resourceCsv -Row $resourceRow

  foreach ($queue in $queues) {
    Add-CsvRow -Path $queueCsv -Row ([pscustomobject]@{
      sampled_at = $sampleTime
      run_id = $queue.run_id
      name = $queue.name
      waiting = $queue.waiting
      oldest_wait_ms = $queue.oldest_wait_ms
      reviewing = $queue.reviewing
    })
  }

  $goRoutinesText = if ($go) { $go.goroutines } else { "n/a" }
  $heapMB = if ($go) { [math]::Round([double]$go.heap_alloc_bytes / 1MB, 1) } else { "n/a" }
  $queueCount = if ($metrics) { $metrics.queue_waiting } else { "n/a" }
  Write-Host "$sampleTime  go-routines=$goRoutinesText heap=${heapMB}MB queue=$queueCount process-count=$($processes.Count)"
  if ($metricsError) { Write-Warning "Metrics endpoint unavailable: $metricsError" }
  if ($processes.Count -eq 0) { Write-Warning "No process named '$ProcessName' (PID $ProcessId) found." }
  Start-Sleep -Seconds $IntervalSeconds
}
