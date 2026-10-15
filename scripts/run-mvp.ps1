$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot
$OriginalMiniMaxAPIKey = $env:MINIMAX_API_KEY
Push-Location $RepoRoot
try {
    $env:GOCACHE = Join-Path $RepoRoot ".cache\go-build"
    if (-not $env:LIVEGUARD_INFRA_MODE) { $env:LIVEGUARD_INFRA_MODE = "local" }
    if ($env:LIVEGUARD_INFRA_MODE -ne "local" -and $env:LIVEGUARD_INFRA_MODE -ne "memory") {
        throw "LIVEGUARD_INFRA_MODE must be 'local' or 'memory'."
    }
    if (-not $env:LIVEGUARD_OTEL_ENDPOINT) { $env:LIVEGUARD_OTEL_ENDPOINT = "off" }
    if (-not $env:LIVEGUARD_MONITOR_LLM_MODE) { $env:LIVEGUARD_MONITOR_LLM_MODE = "auto" }

    if (-not $env:LIVEGUARD_MONITOR_PROVIDER -or $env:LIVEGUARD_MONITOR_PROVIDER -eq "minimax") {
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
    if ($env:LIVEGUARD_INFRA_MODE -eq "memory") {
        Write-Host "This mode keeps monitor history in memory only; it will be lost when the process exits."
    } else {
        $statePath = if ($env:LIVEGUARD_LOCAL_STATE_PATH) { $env:LIVEGUARD_LOCAL_STATE_PATH } else { "runtime/liveguard.json" }
        Write-Host "This mode saves monitor history and Agent decisions locally: $statePath (observations: $statePath.observations.jsonl)"
    }
    if ($env:LIVEGUARD_MONITOR_PROVIDER -eq "minimax" -and $env:MINIMAX_API_KEY) {
        Write-Host "MiniMax $env:LIVEGUARD_MONITOR_MODEL is configured for goal planning and signal review."
    } elseif ($env:LIVEGUARD_MONITOR_PROVIDER -eq "openai" -and $env:OPENAI_API_KEY) {
        Write-Host "OpenAI is configured for goal planning and signal review using Chat Completions."
    } elseif ($env:LIVEGUARD_MONITOR_PROVIDER -eq "deepseek" -and $env:DEEPSEEK_API_KEY) {
        Write-Host "DeepSeek is configured for goal planning and signal review using Chat Completions."
    } else {
        Write-Host "Local rules are active. Add a model API in the Model settings window after startup."
    }
    Write-Host "Press Ctrl+C to stop."
    go run .\cmd\liveguard
} finally {
    $env:MINIMAX_API_KEY = $OriginalMiniMaxAPIKey
    Pop-Location
}
