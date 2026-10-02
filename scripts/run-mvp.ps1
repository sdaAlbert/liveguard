$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot
$OriginalMiniMaxAPIKey = $env:MINIMAX_API_KEY
Push-Location $RepoRoot
try {
    $env:GOCACHE = Join-Path $RepoRoot ".cache\go-build"
    $env:LIVEGUARD_INFRA_MODE = "local"
    if (-not $env:LIVEGUARD_OTEL_ENDPOINT) { $env:LIVEGUARD_OTEL_ENDPOINT = "off" }
    if (-not $env:LIVEGUARD_MONITOR_LLM_MODE) { $env:LIVEGUARD_MONITOR_LLM_MODE = "auto" }

    # Repair only known stale local proxy ports; preserve any other user proxy configuration.
    foreach ($proxyName in @("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")) {
        $proxyValue = [Environment]::GetEnvironmentVariable($proxyName)
        if ($proxyValue -match '^http://127\.0\.0\.1:(9|7890)/?$') {
            [Environment]::SetEnvironmentVariable($proxyName, "http://127.0.0.1:7897")
        }
    }

    if ($env:LIVEGUARD_MONITOR_PROVIDER -ne "openai") {
        $openClawPath = Join-Path $env:USERPROFILE ".openclaw\openclaw.json"
        $providerId = if ($env:LIVEGUARD_MINIMAX_PROVIDER_ID) { $env:LIVEGUARD_MINIMAX_PROVIDER_ID } else { "minimax-cn" }
        if ((-not $env:MINIMAX_API_KEY -or -not $env:LIVEGUARD_MONITOR_PROVIDER) -and (Test-Path -LiteralPath $openClawPath)) {
            $openClawConfig = Get-Content -LiteralPath $openClawPath -Raw | ConvertFrom-Json
            $providerProperty = $openClawConfig.models.providers.PSObject.Properties[$providerId]
            $miniMaxProvider = if ($providerProperty) { $providerProperty.Value } else { $null }
            if ($miniMaxProvider -and $miniMaxProvider.api -eq "anthropic-messages" -and $miniMaxProvider.apiKey) {
                if (-not $env:MINIMAX_API_KEY) { $env:MINIMAX_API_KEY = [string]$miniMaxProvider.apiKey }
                if (-not $env:LIVEGUARD_MONITOR_PROVIDER) { $env:LIVEGUARD_MONITOR_PROVIDER = "minimax" }
                if (-not $env:LIVEGUARD_MONITOR_MODEL) { $env:LIVEGUARD_MONITOR_MODEL = "MiniMax-M3" }
                if (-not $env:LIVEGUARD_MONITOR_ENDPOINT) {
                    $env:LIVEGUARD_MONITOR_ENDPOINT = ([string]$miniMaxProvider.baseUrl).TrimEnd('/') + "/v1/messages"
                }
            }
        }
    }

    if (-not $env:LIVEGUARD_ADDR) { $env:LIVEGUARD_ADDR = "127.0.0.1:8080" }
    if (-not $env:LIVEGUARD_TESSERACT_BIN) {
        $defaultTesseract = "C:\Program Files\Tesseract-OCR\tesseract.exe"
        if (Test-Path $defaultTesseract) { $env:LIVEGUARD_TESSERACT_BIN = $defaultTesseract }
    }
    if (-not $env:LIVEGUARD_OCR_LANGUAGE) { $env:LIVEGUARD_OCR_LANGUAGE = "chi_sim+eng" }

    New-Item -ItemType Directory -Force -Path (Join-Path $RepoRoot ".cache") | Out-Null
    Write-Host "Starting LiveGuard MVP at http://$env:LIVEGUARD_ADDR"
    Write-Host "This mode uses local memory; PostgreSQL is optional."
    if ($env:LIVEGUARD_MONITOR_PROVIDER -eq "minimax") {
        Write-Host "MiniMax $env:LIVEGUARD_MONITOR_MODEL plans each goal; review cadence is selected per monitor and clamped by server policy."
    } else {
        Write-Host "Local goal planning is active; set LIVEGUARD_MONITOR_PROVIDER=minimax to enable M3 planning and proactive review."
    }
    Write-Host "Press Ctrl+C to stop."
    go run .\cmd\liveguard
} finally {
    $env:MINIMAX_API_KEY = $OriginalMiniMaxAPIKey
    Pop-Location
}
