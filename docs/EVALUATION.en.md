# Evaluation and Verification

LiveGuard has automated tests, runtime records, and synthetic load measurements. The fixed-text evaluation code and cases still exist, but the current results are not a reliable measure of model quality. Do not publish accuracy numbers until the evaluation decision path has been repaired and manually checked. Mock load results do not represent real models or real livestream performance.

[English](EVALUATION.en.md) | [简体中文](EVALUATION.md)

## Automated tests

Before submitting a code change, run:

```powershell
go test ./...
go vet ./...
go build ./...
node --check internal/web/static/monitor.js
```

The tests cover agent tool permissions and budgets, retries and timeouts, monitor-state changes, queues and idempotency, failure recovery, SQLite state and observation recovery, Pending recovery across pause/restart/resume, transaction rollback, browser-capture cancellation, OCR upload, and web APIs. SQLite recovery cases use a temporary database and mock Reviewer. They verify program rules, not real-model quality or real-livestream end-to-end recovery. JSON export format is also checked; JSON is no longer a runtime repository.

## Latency from real runtime records

The following figures were aggregated from LiveGuard runtime records on 2026-10-15 using MiniMax-M3. They come from uncontrolled usage with different inputs, networks, and targets. They show what the system has experienced; they cannot by themselves compare model versions.

| Stage | Samples | P50 | P95 | Notes |
|---|---:|---:|---:|---|
| Successful model HTTP requests | 319 | 3.4 s | 9.8 s | 48 failed request records were also present; one request is not a complete investigation. |
| Agent investigations | 53 with valid latency | 17.1 s | 35.0 s | Includes multiple model turns and tool steps within an investigation. |
| Observation to backend alert | 6 with complete timing | 80.5 s | 115.0 s | From observation time to alert creation by the backend; excludes UI rendering and the time until a user sees it. |

An investigation or alert takes longer than one model request because it can include queue wait, tool execution, and multiple judgment turns. There are only six valid alert-latency samples, so treat this as preliminary runtime evidence.

## Synthetic load

`cmd/monitor-load` uses a mock Reviewer with a fixed 40 ms delay. It measures the service admission and queue-processing paths. It does not call a model, browser, OCR, or database.

| Path | Input | Result |
|---|---:|---|
| Synchronous `Service.Ingest` | 160 requests, 3 runs | All accepted, 0 errors; P95 5–6 ms, about 1,043–1,181 requests/s. |
| Synchronous `Service.Ingest` | 800 requests, 3 runs | All accepted, 0 errors; P95 30–32 ms, about 411–427 requests/s. |
| Asynchronous `Service.SubmitSignal` | 160 requests, 3 runs | All queued and processed, 0 errors; about 84.9–86.6 requests/s. |
| Asynchronous `Service.SubmitSignal` | 800 requests, 3 runs | All queued and processed, 0 errors; about 32.9–40.6 requests/s; queue-wait P95 2.7–3.4 s. |

The load environment was Windows/amd64, Go 1.26.3, and 16 logical CPUs, with 8 monitor Runs and 8 submission workers. Reproduce with:

```powershell
go run ./cmd/monitor-load -requests 160
go run ./cmd/monitor-load -requests 800
go run ./cmd/monitor-load -mode async -requests 160
go run ./cmd/monitor-load -mode async -requests 800
```

These throughput figures apply only to the mock backend load. Real model throughput depends on model response time, the number of review turns per signal, and input rate. Existing runtime records are not a controlled end-to-end throughput test.

## Multi-window sustained run

In the latest 6-minute stability run, 8 headless Chrome test pages ran alongside screenshots and Tesseract OCR. A mock Reviewer with a 15-second delay simulated slow reviews. All 8 windows ended healthy; each captured 71–73 frames, and 20 frames were skipped due to backlog. This run did not reproduce a multi-window capture stall. The pages were synthetic and do not represent capture quality on a real livestream platform.

To reproduce, install Chrome and Tesseract locally:

```powershell
$env:LIVEGUARD_STABILITY_DURATION = "6m"
go test ./internal/monitor -run TestEightBrowserMonitorsSustained -count=1 -v
```

## Evaluation scope

- Runtime records provide model-request, investigation, and a small number of alert-latency observations, but the sample is uncontrolled and UI-render time is not recorded.
- We do not report a livestream missed-event rate. Measuring it reliably requires complete source material and human labels, so it is outside the current metrics.
