param([switch]$Evaluate, [ValidateSet("dev", "holdout-v1")][string]$Split = "dev")
$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot
$OpenClawPath = Join-Path $env:USERPROFILE ".openclaw\openclaw.json"
$OriginalAPIKey = $env:MINIMAX_API_KEY
$OriginalEndpoint = $env:LIVEGUARD_MONITOR_ENDPOINT
$OriginalAudio = $env:LIVEGUARD_TEST_AUDIO
$AudioPath = Join-Path $RepoRoot ".cache\minimax-asr-smoke.wav"

Push-Location $RepoRoot
try {
    New-Item -ItemType Directory -Force -Path (Join-Path $RepoRoot ".cache") | Out-Null
    # Use the user's confirmed local proxy port if the shell inherited a stale setting.
    foreach ($proxyName in @("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")) {
        $proxyValue = [Environment]::GetEnvironmentVariable($proxyName)
        if ($proxyValue -match '^http://127\.0\.0\.1:(9|7890)/?$') {
            [Environment]::SetEnvironmentVariable($proxyName, "http://127.0.0.1:7897")
        }
    }
    if (-not $env:MINIMAX_API_KEY) {
        if (-not (Test-Path -LiteralPath $OpenClawPath)) { throw "MiniMax API key is not in the environment or OpenClaw config." }
        $config = Get-Content -LiteralPath $OpenClawPath -Raw | ConvertFrom-Json
        $providerId = if ($env:LIVEGUARD_MINIMAX_PROVIDER_ID) { $env:LIVEGUARD_MINIMAX_PROVIDER_ID } else { "minimax-cn" }
        $providerProperty = $config.models.providers.PSObject.Properties[$providerId]
        $provider = if ($providerProperty) { $providerProperty.Value } else { $null }
        if (-not $provider -or -not $provider.apiKey) { throw "MiniMax provider '$providerId' has no configured API key." }
        $env:MINIMAX_API_KEY = [string]$provider.apiKey
        if (-not $env:LIVEGUARD_MONITOR_ENDPOINT) {
            $env:LIVEGUARD_MONITOR_ENDPOINT = ([string]$provider.baseUrl).TrimEnd('/') + "/v1/messages"
        }
    }

    if ($Evaluate) {
        $env:GOCACHE = Join-Path $RepoRoot ".cache\go-build"
        $reportName = if ($Split -eq "dev") { "monitor-eval-dev.json" } else { "monitor-eval-holdout-v1.json" }
        $evalArgs = @('run', './cmd/monitor-eval', '-split', $Split, '-out', ".cache\$reportName")
        Write-Host "Evaluating the '$Split' fixed-text cases with real MiniMax. API usage will be charged. Report: .cache\$reportName"
        & go @evalArgs
        if ($LASTEXITCODE -ne 0) { throw "Some evaluation cases failed; inspect .cache/monitor-eval.json." }
        return
    }

    Add-Type -AssemblyName System.Speech
    $synth = [System.Speech.Synthesis.SpeechSynthesizer]::new()
    try {
        $voice = $synth.GetInstalledVoices() | Where-Object { $_.VoiceInfo.Culture.Name -eq "zh-CN" } | Select-Object -First 1
        if (-not $voice) { throw "A Chinese Windows speech voice is required to create the short ASR smoke sample." }
        $synth.SelectVoice($voice.VoiceInfo.Name)
        $synth.SetOutputToWaveFile($AudioPath)
        $synth.Speak("语音识别测试。福袋马上开始，请留意直播间的抽奖活动。")
        $synth.SetOutputToNull()
    } finally {
        $synth.Dispose()
    }
    $env:LIVEGUARD_TEST_AUDIO = $AudioPath
    $env:GOCACHE = Join-Path $RepoRoot ".cache\go-build"

    Write-Host "Calling MiniMax ASR and MiniMax M3 image-tool flow. API usage will be charged to the configured account. The key will not be printed."
    $testOutput = & go test -tags=liveapi ./internal/monitor -run '^TestMiniMaxLive' -count=1 -v 2>&1
    $testExitCode = $LASTEXITCODE
    $testOutput | ForEach-Object { Write-Host $_ }
    if ($testExitCode -ne 0) { throw "Live MiniMax API smoke tests failed with exit code $testExitCode." }
} finally {
    $env:MINIMAX_API_KEY = $OriginalAPIKey
    $env:LIVEGUARD_MONITOR_ENDPOINT = $OriginalEndpoint
    $env:LIVEGUARD_TEST_AUDIO = $OriginalAudio
    Remove-Item -LiteralPath $AudioPath -Force -ErrorAction SilentlyContinue
    Pop-Location
}
