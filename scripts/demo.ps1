[CmdletBinding()]
param(
    [ValidateSet("start", "stop", "status")]
    [string]$Action = "start",
    [switch]$OpenBrowser
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot
$DemoRoot = Join-Path $RepoRoot "runtime\demo"
$BinRoot = Join-Path $DemoRoot "bin"
$PidFile = Join-Path $DemoRoot "processes.json"
$WorkerExe = Join-Path $BinRoot "liveguard-sandbox-worker.exe"
$ControlExe = Join-Path $BinRoot "liveguard-control.exe"
$Dashboard = "http://127.0.0.1:8080"

function Get-DemoState {
    if (-not (Test-Path $PidFile)) {
        return $null
    }
    try {
        return Get-Content $PidFile -Raw | ConvertFrom-Json
    } catch {
        return $null
    }
}

function Test-TrackedProcess {
    param($Entry)
    if ($null -eq $Entry) {
        return $false
    }
    $process = Get-Process -Id $Entry.pid -ErrorAction SilentlyContinue
    if ($null -eq $process) {
        return $false
    }
    try {
        return [IO.Path]::GetFullPath($process.Path) -eq [IO.Path]::GetFullPath($Entry.path)
    } catch {
        return $false
    }
}

function Stop-DemoProcesses {
    $state = Get-DemoState
    if ($null -ne $state) {
        foreach ($entry in @($state.processes)) {
            if (Test-TrackedProcess $entry) {
                Stop-Process -Id $entry.pid -Force
                Write-Host "Stopped $($entry.name) (PID $($entry.pid))."
            }
        }
    }
    if (Test-Path $PidFile) {
        Remove-Item -LiteralPath $PidFile -Force
    }
}

function Wait-ForDashboard {
    param([int]$Seconds = 20)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $status = Invoke-RestMethod -Uri "$Dashboard/api/infra/status" -TimeoutSec 2
            if ($status.postgres -eq "healthy" -and $status.redis -eq "healthy" -and $status.worker -eq "healthy") {
                return $true
            }
        } catch {
            Start-Sleep -Milliseconds 400
        }
    }
    return $false
}

function Show-Status {
    $state = Get-DemoState
    if ($null -eq $state) {
        Write-Host "LiveGuard demo is not tracked as running."
        return
    }
    foreach ($entry in @($state.processes)) {
        $status = if (Test-TrackedProcess $entry) { "running" } else { "stopped" }
        Write-Host "$($entry.name): $status (PID $($entry.pid))"
    }
    try {
        $infra = Invoke-RestMethod -Uri "$Dashboard/api/infra/status" -TimeoutSec 2
        Write-Host "postgres=$($infra.postgres) redis=$($infra.redis) worker=$($infra.worker) outbox=$($infra.outbox) pending=$($infra.pending) dlq=$($infra.dead_letter)"
    } catch {
        Write-Host "Dashboard API is unavailable."
    }
}

Push-Location $RepoRoot
try {
    switch ($Action) {
        "status" {
            Show-Status
            return
        }
        "stop" {
            Stop-DemoProcesses
            docker compose stop
            Write-Host "LiveGuard demo stopped. Persistent Docker volumes were kept."
            return
        }
    }

    $existing = Get-DemoState
    if ($null -ne $existing -and (@($existing.processes) | Where-Object { Test-TrackedProcess $_ }).Count -eq 2) {
        Write-Host "LiveGuard demo is already running at $Dashboard"
        if ($OpenBrowser) {
            Start-Process $Dashboard
        }
        return
    }

    Stop-DemoProcesses
    New-Item -ItemType Directory -Force -Path $BinRoot | Out-Null

    $composeHelp = docker compose up --help 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0 -or $composeHelp -notmatch '--wait') {
        throw "Docker Compose v2 with 'docker compose up --wait' support is required"
    }

    Write-Host "[1/4] Starting PostgreSQL, Redis, OpenTelemetry Collector, and Jaeger..."
    docker compose up -d --wait
    if ($LASTEXITCODE -ne 0) {
        throw "docker compose failed"
    }

    docker image inspect golang:1.26 *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "Pulling the Sandbox image once..."
        docker pull golang:1.26
        if ($LASTEXITCODE -ne 0) {
            throw "failed to pull golang:1.26"
        }
    }

    Write-Host "[2/4] Building Go binaries..."
    go build -o $WorkerExe ./cmd/sandbox-worker
    if ($LASTEXITCODE -ne 0) {
        throw "failed to build sandbox worker"
    }
    go build -o $ControlExe ./cmd/liveguard
    if ($LASTEXITCODE -ne 0) {
        throw "failed to build control plane"
    }

    $keys = @(
        "LIVEGUARD_INFRA_MODE", "LIVEGUARD_LLM_MODE", "LIVEGUARD_HEADLESS",
        "LIVEGUARD_ADDR", "LIVEGUARD_OTEL_ENDPOINT", "LIVEGUARD_SANDBOX_RPC_ADDR",
        "LIVEGUARD_SANDBOX_RPC_TARGET", "LIVEGUARD_SANDBOX_IMAGE"
    )
    $previous = @{}
    foreach ($key in $keys) {
        $previous[$key] = [Environment]::GetEnvironmentVariable($key, "Process")
    }

    try {
        $env:LIVEGUARD_INFRA_MODE = "durable"
        $env:LIVEGUARD_LLM_MODE = "deterministic"
        $env:LIVEGUARD_HEADLESS = "true"
        $env:LIVEGUARD_ADDR = "127.0.0.1:8080"
        $env:LIVEGUARD_OTEL_ENDPOINT = "127.0.0.1:4317"
        $env:LIVEGUARD_SANDBOX_RPC_ADDR = "127.0.0.1:9090"
        $env:LIVEGUARD_SANDBOX_RPC_TARGET = "127.0.0.1:9090"
        $env:LIVEGUARD_SANDBOX_IMAGE = "golang:1.26"

        Write-Host "[3/4] Starting the gRPC Sandbox Worker..."
        $worker = Start-Process -FilePath $WorkerExe -WorkingDirectory $RepoRoot -WindowStyle Hidden -PassThru `
            -RedirectStandardOutput (Join-Path $DemoRoot "worker.out.log") `
            -RedirectStandardError (Join-Path $DemoRoot "worker.err.log")
        Start-Sleep -Seconds 1
        if ($worker.HasExited) {
            throw "sandbox worker exited; inspect runtime/demo/worker.err.log"
        }

        Write-Host "[4/4] Starting the deterministic control plane..."
        $control = Start-Process -FilePath $ControlExe -WorkingDirectory $RepoRoot -WindowStyle Hidden -PassThru `
            -RedirectStandardOutput (Join-Path $DemoRoot "control.out.log") `
            -RedirectStandardError (Join-Path $DemoRoot "control.err.log")

        [pscustomobject]@{
            started_at = (Get-Date).ToUniversalTime().ToString("o")
            mode = "deterministic"
            processes = @(
                [pscustomobject]@{ name = "sandbox-worker"; pid = $worker.Id; path = $WorkerExe },
                [pscustomobject]@{ name = "control-plane"; pid = $control.Id; path = $ControlExe }
            )
        } | ConvertTo-Json -Depth 4 | Set-Content -Path $PidFile -Encoding UTF8
    } finally {
        foreach ($key in $keys) {
            [Environment]::SetEnvironmentVariable($key, $previous[$key], "Process")
        }
    }

    if (-not (Wait-ForDashboard)) {
        Stop-DemoProcesses
        throw "LiveGuard did not become healthy; inspect runtime/demo/*.err.log"
    }

    Write-Host ""
    Write-Host "LiveGuard is ready: $Dashboard"
    Write-Host "Jaeger: http://127.0.0.1:16686"
    Write-Host "Mode: deterministic (no API key or network model call required)"
    Write-Host "Stop: powershell -ExecutionPolicy Bypass -File .\scripts\demo.ps1 stop"
    if ($OpenBrowser) {
        Start-Process $Dashboard
    }
} finally {
    Pop-Location
}
