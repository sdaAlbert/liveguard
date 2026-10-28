# Running, Configuration, and Troubleshooting

[English](OPERATIONS.en.md) | [简体中文](OPERATIONS.md)

If you are new to the project, start with the [Project Guide](PROJECT-GUIDE.en.md) for the meanings of Run, observation, investigation, and alert. This page focuses on startup, configuration, troubleshooting, and local data.

## Runtime duration and API quota

**LiveGuard is a continuous monitoring service.** A Run uses `context.WithCancel`; it has no total-duration deadline, automatic stop timer, or ten-minute limit derived from `StartedAt`. While the model API is available, live input is valid, and the service remains running, LiveGuard can keep collecting signals. It rebuilds investigation context over time; it does not keep one model request open indefinitely.

- The user pressing Stop, the service exiting, or an explicit model API quota/balance exhaustion ends monitoring.
- Ordinary network errors, temporary rate limits, or one investigation timeout do not end the whole session. After bounded retries, monitoring continues; repeated provider failures can open a short circuit breaker.
- A login or CAPTCHA prompt pauses input and asks the user to intervene. Temporary screenshot, OCR, or state-write failures enter a recovery state and retry with backoff up to 30 seconds; successful recovery restores health.
- The shared model adapter recognizes explicit quota errors as `ErrModelQuotaExhausted`. The configured shared reviewer records the unavailable state, stops active windows using that configuration, cancels capture and queued work, releases the windows, and preserves the stop reason.
- The Run records `Status=failed`, `stop_reason=api_quota_exhausted`, and a quota-related `last_error`. Model settings report `quota_exhausted`, and the web page displays “API quota exhausted.” An ordinary HTTP 429 is not by itself proof of a depleted balance.
- After restoring quota, save the model settings again and create a new monitor. Saving clears the unavailable flag bound to that configuration; it does not resume an ended window and does not test the API key.
- LiveGuard does not poll for account balance or stop early based on estimated spend. It stops only when an actual model request returns a clear quota/balance-unavailable error. It also does not make empty requests just to consume quota when there are no new signals.

### What each time limit applies to

| Limit | Applies to | Ends the monitoring session? |
|---|---|---|
| Total session duration | Run | **No fixed limit** |
| Observation/queue expiry | By default, text expires after 2 minutes and a frame after 30 seconds; an explicit countdown, relative future time, or `valid_until` can override the default | Stops a stale investigation from starting; the Run and stored history remain |
| 90-second investigation / 12-second tool / 60-second HTTP request | One investigation, tool call, or model request | Stops or fails that execution, not the whole Run |
| 15-minute evaluation command limit | Offline `cmd/monitor-eval` run | Applies only to the evaluation command |

Code entry points: [Run creation and capture recovery](../internal/monitor/service.go), [quota termination](../internal/monitor/quota.go), and [provider error classification](../internal/monitor/model_errors.go). Quota and rate limiting are distinguished using provider error metadata. Live text such as “balance is low” cannot trigger quota shutdown. See the provider documentation for [OpenAI error codes](https://developers.openai.com/api/docs/guides/error-codes), [MiniMax error codes](https://platform.minimax.io/docs/api-reference/errorcode), and [DeepSeek error codes](https://api-docs.deepseek.com/quick_start/error_codes).

## Local data and long-running use

By default, LiveGuard stores monitoring state in the local `runtime/liveguard.sqlite` database. It does not connect to an external database.

| Data | Default location | Behavior after exit/restart |
|---|---|---|
| Goal/Plan/Sampling and bounded event, investigation, and alert snapshots | `runs` table in `runtime/liveguard.sqlite` | Restored on next start; a Run that was active is paused so the user can resume it |
| Full observation history | `observations` table in the same database | Committed in the same transaction as the Run update and queried by page |
| Pending signals, queue order, and waiting image bytes | `pending_signals` table in the same database | Still-valid signals for the current goal are restored after pause or process restart and investigated again after resume |
| Service logs | `runtime/liveguard.log`, rotated at 10 MiB to `.1`; change with `LIVEGUARD_LOG_PATH` | Kept locally; container/service deployments need their own collection or backup |
| Active model request, model turn and tool position, in-flight cancellation, review slots, throttling times | In memory in Service and the Agent runtime | Not restored; the request is cancelled and the persisted signal is investigated again from its saved evidence |
| Screenshot inspected during an investigation | System temp directory as `liveguard-agent-frame-*.png` | `save_frame_evidence` keeps the image only during the investigation; it is deleted at normal completion, not archived. Cleanup after forced termination has not been specifically verified |
| Chrome login profile | `runtime/live-profile/` | Browser profile remains local; this does not resume a monitor or queue |
| API key | In local Go process memory or a user-provided environment variable | UI settings are not written to the repository or browser storage; configure again after restart or supply an environment variable |
| Whether an alert was read | Browser `localStorage` | Affects only web display, not backend event idempotency |
| Manually exported/evaluation reports | `.cache/` | Git-ignored; not an automatically maintained service database |

SQLite is the only persistent runtime repository; `LIVEGUARD_SQLITE_PATH` changes its location. Run snapshots, observation records, and admission of new queued signals use SQLite transactions. Each Run can hold up to 8 pending signals, each staged image is limited to 4 MiB, and all pending images together are limited to 64 MiB. After restart, queued or processing signals that have not expired and still match the current GoalVersion return to the queue; expired signals and signals for older goals are discarded. An active model request cannot resume at its previous turn. Recovery starts a new investigation from the persisted evidence, so an external model call may be repeated if the process stopped between that call and local commit. This provides at-least-once processing, not exactly-once. Existing JSON/JSONL files are not imported automatically, and LiveGuard no longer opens them as runtime storage; inspect old files with a text or JSON tool if needed.

Create a JSON copy for inspection and archiving with `go run ./cmd/monitor-store-export -db runtime/liveguard.sqlite -out runtime/json-backup-2026-10-27`. The output directory must be new and contains `liveguard.json` plus `liveguard.json.observations.jsonl`, exported from one SQLite snapshot. The export does not include the Pending queue, is not a complete backup, and cannot be imported back into LiveGuard. SQLite is the only persistent runtime repository; explicitly setting `LIVEGUARD_INFRA_MODE=memory` selects temporary in-memory storage, so Runs and observations disappear when the process exits. For a complete SQLite backup, stop the service and copy `runtime/liveguard.sqlite` with any existing `-wal` / `-shm` sidecars. Investigation screenshots are not kept as historical archives; image bytes for queued work are staged in SQLite and removed when the queue item finishes.

OCR deduplicates text before appending new lines to the observation log and queue. Agent review has its own throttle, so it does not call the model on every OCR tick. Both regular capture images and screenshots selected during an investigation use temporary files and are deleted after normal processing. Cleanup after forced termination has not been specifically verified. Related OCR text and text conclusions are stored in monitor records.

The Run snapshot has a limit on the number of events; the complete observation history is stored separately in the append-only log. There is no automatic retention policy for that log, so disk usage grows over time. Back it up or clean it up yourself.

At startup, `RecoverInterrupted` changes leftover `running` Runs to `paused`, stores the `process_interrupted` reason, and reclaims valid Pending items. Resuming rebuilds capture and the queue, keeps history in the same Run, and investigates recovered signals again. The current tool call, model turn, and browser execution position are not restored.

### Bounded history does not mean all resources are bounded

| Per-Run limit | Value |
|---|---:|
| Events | 2,000 |
| Investigations / Alerts | 500 each |
| Evidence per investigation / per alert | 100 each |
| Steps per investigation | 64 |
| Event receipts | 2,048 and at most 24 hours |

After trimming, `HistoryTruncated=true`. Counts such as Events and Alerts in the summary mostly describe records still retained, not lifetime totals. Exported token totals cover only retained investigations and do not include all planning, ASR, or invisible failed-request costs.

The `MemoryRepository` Run map does not limit or expire the number of stopped sessions, so repeatedly creating and stopping Runs grows memory use. In file mode, each update deep-copies a Run as JSON and atomically rewrites the snapshot; frequent updates add CPU and disk work. Per-Run limits are not evidence of unlimited service capacity.

## Failures and recovery

`RunStatus`, `SourceHealth`, and `Investigation.Status` are three separate dimensions. A Run can be `running`, `paused`, `stopped`, `failed`, or `completed`. `paused` means the user paused the capture/investigation runtime. A Run can still be `running` while `SourceHealth=capture_failed`, meaning the session was not stopped but capture is not working. Check health and `LastError`; do not rely on `running` alone.

| Scenario | Current behavior | Current limitation |
|---|---|---|
| Login or CAPTCHA prompt | Sets `needs_login` and closes the input queue; after handling the page, pause and resume so the source is checked again | Does not log in or bypass verification automatically; text detection can misclassify |
| Screenshot/OCR failure | Screenshot and OCR have separate counters. A delayed state retries with exponential backoff up to 30 seconds and recovers automatically; failed state writes have their own logs and retries | Login/verification still needs a person. An old goal result cannot revive an old runtime. OCR backlog replaces a waiting frame but does not cancel the frame currently being recognized, and is not counted as source failure |
| OCR text is too long | Internal OCR splits up to 4,000 characters into multiple citeable observations while preserving recognized text and source | External input still validates the per-item length; splitting does not improve OCR accuracy |
| No text or speech | `ErrNoText` can enter the no-text visual-check path; `ErrNoSpeech` is a normal no-transcript result | A successful screenshot does not prove that the stream is playing or that comments are changing |
| Model rate limit/network failure | Temporary rate-limit or network problems use bounded retries and a circuit breaker; failed investigations are recorded, but new signals continue. Explicit quota/balance errors end active monitoring under that configuration and show a reason | A model request may have one bounded retry; a provider may charge for both requests. Unknown errors are not guessed to be quota exhaustion |
| Invalid model format | Shared Agent requests at most one format correction and spends the original turn budget; a second invalid result stops the investigation | No unlimited retries and no fallback to keyword alerts |
| Tool error/timeout/budget exhausted | Records steps, success state, duration, and stop reason; an incomplete investigation does not submit a normal alert | Context timeout depends on the adapter honoring cancellation; Go cannot kill arbitrary code that ignores context |
| Queue full/expired investigation | Pending shows dropped, expired, or merged counts and reasons; SQLite stores unfinished items, and expiry removes the queue item but keeps the observation; rejected work may be retried with the original ID | A receipt means accepted/recorded, not completed; completed or failed per-event state is not a permanent job ledger |
| Goal change/pause/stop | Goal changes increment `GoalVersion`; pause cancels old work but keeps the Run; stop ends the Run; commit checks prevent late results from taking effect | Resume rebuilds runtime; browser audio requires fresh user authorization |
| Process exit/restart | Active Runs become paused on next start and still-valid SQLite Pending signals are requeued; shutdown source is recorded | After Resume, signals are investigated again; the original model turn and browser action are not resumed. Memory mode loses state |
| Browser disconnect | Accepted queued work uses the monitor context and is not canceled with the HTTP client; SSE is an update hint, and the page reloads a snapshot | SSE is not a durable message bus or proof of external delivery |

The model circuit breaker defaults to two consecutive request failures and a 30-second cooldown across the three configured providers. After the cooldown, only one recovery probe is allowed. Investigation deadline and signal expiry are not model errors, so they do not open the breaker. A single model request may wait up to 60 seconds; an entire Agent investigation may run up to 90 seconds; the signal's own expiry can end it sooner.

The shared `DoModelRequest` retries replayable requests at most once for selected transient network failures and HTTP 408, 429, 500, 502, 503, or 504. It usually waits 250 ms. It does not wait automatically for a `Retry-After` longer than 2 seconds. It does not retry 401/403. The provider may charge for both attempts.

Model request logs include `review_id` (trigger signal ID), `attempt_id` (unique per investigation attempt), turn, HTTP status, retry count, request duration, available context budget, and timeout source. Agent trace logs also record slot/cooldown waiting, model-turn start, tool start/completion, and investigation completion. Tool logs exclude raw arguments, OCR text, and model prompts. Planning requests use the same transport policy. Format correction and transport retry are counted separately.

When reading request logs, `context_budget_ms` is the remaining **milliseconds** until that request's context deadline, not a token budget; `client_timeout_ms` is the HTTP client timeout. `input_tokens` and `output_tokens` describe one model request. A log such as `unexpected livestream review completed/incomplete` summarizes token use across the investigation's recorded turns.

To follow current Agent activity in PowerShell, run `Select-String -Path .\runtime\liveguard.log -Pattern 'attempt_id=' | Select-Object -Last 100`. Group by `attempt_id`: `agent review waiting for slot` means the work is waiting for global capacity/cooldown; `agent review slot acquired` means it can start; `agent model turn started` marks an outgoing model request; `model request completed/failed` marks its response; `agent tool started/completed` marks Go tool execution; then check `agent review finished` and `unexpected livestream review completed/incomplete/failed`. A start entry with no end entry means that stage is still running or has not logged its result. The existing process will not emit the new entries; restart onto the updated build to enable them.

`review_id` identifies the triggering signal, not a unique investigation attempt. A repeated investigation of the same signal restarts at `turn=1`, so group by `attempt_id`. Interpret `context canceled` together with `LiveGuard shutdown requested`, pause/stop records, and `timeout_source`. If it immediately follows process interruption, it usually means runtime cancellation, not token overflow. `model response truncated at token limit` means one response hit its output limit; `turn_budget` or `call_budget` means the investigation loop used up its turn or call allowance.

There is currently no hard token budget per Run or per day. The per-response output limit and investigation turn limit are described in [Agent Harness](AGENT-HARNESS.en.md).

Capture recovery distinguishes retryable `delayed` from a truly `paused` state. Queue admission blocks `needs_login` and `capture_failed`; a successful observation restores input health. A full queue or rejected image budget is not considered a capture failure. Explicit quota errors have a user-visible stop reason. Unresolved problems and possible improvements are tracked in the [Roadmap](ROADMAP.en.md).

## Running locally and using the API

### Quick start

The supported startup script targets Windows. Requirements: Windows 10/11, Go 1.25, Google Chrome, and Tesseract. For Chinese livestreams, install Tesseract's `chi_sim` language data. Processing browser audio also requires FFmpeg and MiniMax configuration. Full macOS/Linux setup and real-browser acceptance instructions are not yet available.

~~~powershell
git clone https://github.com/sdaAlbert/liveguard.git
cd liveguard
go version
$env:LIVEGUARD_INFRA_MODE = "sqlite"
powershell -ExecutionPolicy Bypass -File ./scripts/run-mvp.ps1
~~~

The startup script prints the active SQLite path, defaulting to `runtime/liveguard.sqlite`; the log also includes `SQLite monitor state persisted to ...`. To check Pending recovery, create a real livestream monitor and let a new OCR signal enter the queue while the Agent is investigating. Press Ctrl+C within two minutes of that signal, wait for shutdown, restart with the same command, and click Resume for that Run. The Run should appear paused first; after Resume, the Agent should investigate the interrupted signal again. Frame signals expire after 30 seconds, so restart sooner for those. The current model turn does not resume; a new investigation record/model request is the evidence of replay. Keep the same SQLite path across both starts.

Open http://127.0.0.1:8080, open the dedicated Chrome window and sign in, then create a livestream monitor. In “Model Settings,” configure provider, API key, model, and optional endpoint. Saving does not send a model request. The key is sent only to the local Go service; it is not stored in browser storage or the repository. Leaving the key empty reuses the current process key for the same provider. After restart, enter it again or provide it through the environment.

Model settings affect future investigations. A target plan is regenerated when creating or changing the goal, and its capability projection is refreshed for the selected model. MiniMax-M3, GPT-6 Luna, GPT-6 Sol, and DeepSeek Flash are configured as supporting tool calls and image input by default. For custom models, declare image understanding according to actual capability; the service does not send a probe request to verify that declaration.

OpenAI GPT-6 Luna and GPT-6 Sol use `reasoning_effort: none` through Chat Completions. All three providers use the same planning and investigation strategy; protocol adapters translate requests to provider-specific formats. Choosing “Local Rules” disables model planning and judgment; it does not disable separately configured MiniMax ASR. Setting a model key in the UI does not configure ASR, which is mainly configured at startup.

Before startup, you can set `MINIMAX_API_KEY`, `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`, and `LIVEGUARD_MONITOR_PROVIDER`. The script defaults to `auto` and tries to reuse local OpenClaw MiniMax configuration. Direct Go startup defaults to `deterministic`; to use environment-configured models, also set `LIVEGUARD_MONITOR_LLM_MODE=auto`.

If Tesseract is not installed at its default path:

~~~powershell
$env:LIVEGUARD_TESSERACT_BIN = "C:\path\to\tesseract.exe"
$env:LIVEGUARD_OCR_LANGUAGE = "chi_sim+eng"
~~~

A proxy is optional. Set `HTTPS_PROXY` for your own environment if needed. The program and startup script respect existing proxy settings; they do not rewrite a user's port to a hard-coded machine-specific value.

### Common configuration

| Setting | Purpose |
|---|---|
| `LIVEGUARD_ADDR` | Defaults to `127.0.0.1:8080` |
| `LIVEGUARD_MONITOR_LLM_MODE` | Direct Go startup defaults to `deterministic`; `auto` enables configured providers |
| `LIVEGUARD_MONITOR_PROVIDER / MODEL / ENDPOINT` | Provider, model, and endpoint; repository defaults are not a claim about the provider's latest recommendation |
| `LIVEGUARD_MONITOR_SUPPORTS_VISION` | Explicit declaration of custom-model image support; no probe request is sent |
| `LIVEGUARD_MONITOR_REVIEW_INTERVAL` | Minimum global interval between investigation starts; defaults to 15 seconds |
| `LIVEGUARD_LIVE_CAPTURE_INTERVAL` | Optional global capture interval floor; without it, local OCR target interval is 1 second |
| `LIVEGUARD_LIVE_CAPTURE_WARMUP` | New-page warm-up; defaults to 7 seconds |
| `LIVEGUARD_TESSERACT_BIN / OCR_LANGUAGE` | Tesseract path and OCR languages |
| `LIVEGUARD_ASR_MODE / MODEL / ENDPOINT / FFMPEG_BIN` | Startup ASR configuration; `off` disables it. Audio sharing still requires user authorization |
| `LIVEGUARD_ALLOWED_HOSTS` | Optional comma-separated allowlist for dedicated-browser hosts; unset means any valid HTTP(S) target is accepted. Configure it if other users can reach the service |
| `LIVEGUARD_INFRA_MODE` | `sqlite` (default, persistent local store) or `memory` (temporary in-process store; lost on exit) |
| `LIVEGUARD_SQLITE_PATH` | SQLite database path; defaults to `runtime/liveguard.sqlite` |
| `LIVEGUARD_FRAME_ARCHIVE_DIR` | Directory to read previously archived screenshots; defaults to Git-ignored `runtime/frames`. Current investigations do not write new screenshots there |
| `LIVEGUARD_LOG_PATH` | Persistent service log; defaults to `runtime/liveguard.log`, with one 10 MiB rotated file |
| `LIVEGUARD_OTEL_ENDPOINT` | Optional OTLP exporter; full Agent investigation spans are not yet instrumented |
| `LIVEGUARD_MODEL_PRICING` | Optional JSON model-ID price table for estimated cost per million input/output tokens |

The control-room metrics panel reads `/api/metrics` during monitor refresh. Queue size and oldest wait come from live queues. Model calls, failure categories, provider-reported token usage, and transport retry counts are stored in SQLite. Alert latency is the average for locally retained alerts from the last 24 hours. User cancellations are reported separately and excluded from the model failure rate. Cost is estimated only when token usage and a local price entry are available; no price is inferred when either is missing. For example:

~~~powershell
$env:LIVEGUARD_MODEL_PRICING = '{"model-id":{"input_per_million":1.0,"output_per_million":2.0}}'
~~~

Model records contain correlation IDs, model, duration, outcome category, retries, and token counts. They do not contain prompts or full tool exchanges, so these metrics are not replayable traces. Alert latency is limited by retained Run history. With in-memory storage, model-call records are lost when the process exits.

To keep a local resource time series while testing, run the sampler in a separate PowerShell window:

~~~powershell
.\scripts\capture-runtime.ps1 -IntervalSeconds 5
~~~

Stop it with `Ctrl+C`, or add `-DurationMinutes 30` to stop automatically after a 30-minute run. It polls `/api/metrics` every five seconds by default and samples matching `liveguard` Windows processes. Use `-ProcessId` to select a PID or `-MetricsUrl` to change the service address. Output is written under `artifacts/runtime-monitoring/<timestamp>/` as a resource summary CSV, per-monitor queue CSV, and raw metrics JSONL. The metrics API provides point-in-time Go goroutine, heap, and GC counts; PowerShell also records process working set, private memory, OS threads, handles, and cumulative CPU time. This is local sampling, not an alerting service. API outages or scrape errors are recorded in the resource CSV.

Agent investigations with a configured model also record a replayable loop trace: normalized model turns, tool calls, and tool results. SQLite keeps the latest 20 traces per Run. Each tool result is limited to 48 KiB; screenshot content stores only its MIME type, length, and SHA-256 digest, never image bytes. Traces may contain livestream text and tool arguments, so treat them as sensitive local data. Replay runs the Agent loop against recorded turns and tool results; it does not contact the provider, call original tool handlers, or submit alerts. It checks the captured loop path, not a new inference from the original prompt or a reconstruction of the external environment.

See [.env.example](../.env.example) for examples. Never put secrets in public documentation.
### HTTP API

| Endpoint | Purpose |
|---|---|
| `POST /api/monitors` | Create with `target_url`, `goal`, and `source_mode`; returns 202 |
| `GET /api/monitors` | List monitor summaries |
| `GET /api/monitors/{id}` | Read details, evidence, investigations, alerts, and Pending |
| `PUT /api/monitors/{id}/goal` | Change the goal and increment `GoalVersion` |
| `POST /api/monitors/{id}/instruction` | Save a general instruction for the Agent; while paused, submission resumes monitoring; text beginning with “Goal:” changes the goal |
| `POST /api/monitors/{id}/pause` | Pause capture and investigation while retaining the monitor card and history |
| `POST /api/monitors/{id}/resume` | Resume the same monitor; returns 429 if all runtime slots are occupied |
| `POST /api/monitors/{id}/stop` | Stop monitoring |
| `POST /api/monitors/{id}/signals` | Submit text signal; supports ID-based idempotency |
| `POST /api/monitors/{id}/ocr` | Submit an image for OCR using multipart form data |
| `POST /api/monitors/{id}/audio` | Transcribe and submit an audio segment the user authorized |
| `GET /api/events` | SSE state-update hints; read the snapshot for current details |
| `GET/POST /api/browser/session` | Inspect/open the dedicated browser window; does not prove successful login |
| `GET/PUT /api/settings/model` | Read/update local model settings |
| `GET /api/metrics` | Read-only queue, model-call, and alert-latency metrics |
| `GET /api/monitors/{id}/traces` | List investigation trace summaries |
| `POST /api/monitors/{id}/traces/{trace_id}/replay` | Replay one investigation loop without external side effects |

Supported `source_mode` values are `browser_ocr` (real screenshot), `ocr_upload` (manual text or upload), and `demo_replay` (synthetic demo). The Go Service default is `demo_replay`; a successful demo does not prove real capture works.

## Project structure and terms

| Directory | Responsibility |
|---|---|
| `cmd/liveguard` | Local service entry point, configuration, repository, and capture-adapter wiring |
| `cmd/monitor-eval`, `cmd/monitor-load` | Real text-model evaluation and synthetic backend load |
| `internal/agent` | Generic bounded loop, HTTP client, retry, and circuit breaker |
| `internal/monitor` | Lifecycle, domain types, queue, perception, planning, semantic investigation, observation search, repository, and evaluation |
| `internal/browser` | Dedicated Chrome session and CDP screenshots |
| `internal/web` | Web page, HTTP API, and SSE |
| `internal/telemetry` | Optional OTLP initialization |
| `scripts` | Startup, explicitly paid verification, and acceptance export |
| `docs/ROADMAP.md` | Known issues, candidate improvements, and acceptance evidence |

A Monitor Run is a continuous monitoring session. A Signal is one observation. Evidence is information with a source and time that supports or contradicts a claim. An Investigation is a bounded attempt to reason about a signal. Pending is work that has not finished. Memory is a bounded explanation of relevant history. An Alert is a backend record. Describe receipt, processing, alert creation, browser read state, and external delivery as separate events.

## Security boundaries

LiveGuard is a local reference project. By default it listens only on the local machine and has no user authentication or TLS; it is not hardened for public internet deployment. The browser uses a separate profile directory; do not share that profile or private screenshots.

Site restrictions do not yet fully cover redirects, DNS rebinding, or SSRF. Treat website-provided signals as untrusted data, not system instructions. Server-side tool allowlists control what the model can invoke.

The repository ignores `.env.local`, `runtime/`, `artifacts/`, `state/`, and `.cache/`. API keys stay in local environment or process memory; do not print or commit them. Report security issues through GitHub's private vulnerability reporting. Do not publish credentials, private URLs, or user data.
