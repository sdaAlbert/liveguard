# Agent Harness: Runtime, Safety Boundaries, and Interview Guide

[English](AGENT-HARNESS.en.md) | [简体中文](AGENT-HARNESS.md)

This document explains LiveGuard's Agent Harness: how a model request becomes a bounded investigation, how tools are executed, and which checks must pass before an alert is saved. It is written for readers who are new to agent systems.

## 1. What “Harness” means in this project

An Agent Harness is the application code around a language model that makes its work usable and controlled. LiveGuard's Harness is not just a loop that sends a prompt and waits for an answer. It has several layers:

1. **Generic agent loop** — [`internal/agent/loop.go`](../internal/agent/loop.go) limits turns and tool calls, checks the tool allowlist, enforces deadlines, detects repeated calls, and returns tool results to the model.
2. **LiveGuard reviewer** — [`internal/monitor/agent_reviewer.go`](../internal/monitor/agent_reviewer.go) prepares the investigation context, model session, and domain tools, then converts the final model response into a decision.
3. **Asynchronous service and queue** — receives signals, applies backpressure and scheduling rules, and runs investigations independently from the HTTP request.
4. **Domain and persistence checks** — verifies that evidence, target version, signal age, run state, and duplicate-alert rules still allow a proposed alert to be saved.
5. **Runtime records and evaluation** — keeps selected tool-step summaries and request metrics, runs regression tests, and measures synthetic load.

A useful mental model: the model proposes what to do; the Harness decides which actions it may perform; the service decides whether the result is still valid; the repository stores only accepted state.

## 2. One investigation, step by step

1. A live capture, uploaded image, replay, or text signal creates an observation and submits a signal.
2. The service validates it, deduplicates by event ID, checks expiry and queue capacity, and schedules accepted work.
3. A worker creates a bounded investigation with a copy of the current goal version and a deadline.
4. The reviewer builds the model request from the goal, recent observations, permitted evidence, and tool descriptions.
5. The model may answer directly or request an allowlisted tool.
6. The loop validates and runs that tool under its own timeout, then returns a concise result to the next model turn.
7. The loop stops on a final answer, a budget limit, a timeout, cancellation, or an unrecoverable error.
8. The reviewer parses the result into a decision. The service checks current state again before saving.
9. An accepted alert becomes visible through the monitor snapshot; the browser receives an update hint and reloads that snapshot.

This is an asynchronous investigation, but the model response itself is not streamed token by token to the web page. The web uses HTTP/SSE for state-update hints.

## 3. The generic loop in `internal/agent`

The generic package does not know about livestreams, OCR coordinates, chat regions, or alert semantics. It handles reusable execution rules.

### 3.1 Turn and call budgets

A **turn** is one request to the model. A **tool call** is a request from the model to execute one named application function. The loop applies configured maximums so a model cannot keep calling itself forever. The monitor reviewer supplies the investigation policy; the loop enforces it.

The limits are intentionally independent: an investigation can stop because it used its turn budget even if it did not use every tool call, or because it used the call budget before reaching the turn limit.

### 3.2 Allowlisted tools and result return

The loop accepts only tool names registered by the caller. Unknown names are rejected instead of being dispatched dynamically. A tool handler returns a structured result, which is appended to the next model request so the model can reason from what the application actually did.

A model response is not permission to run arbitrary shell commands, read arbitrary files, control any browser, post a chat message, or make a purchase. The registered tool set is the real capability boundary; prompt wording alone is not a security control.

### 3.3 Timeouts, cancellation, and repeated calls

The loop uses an overall investigation deadline and a per-tool timeout. It also observes cancellation from the run, signal, and caller contexts. When a tool respects Go's context, cancellation ends its work. Go cannot forcibly stop arbitrary code that ignores its context.

Repeated-call detection prevents the same investigation from spending its budget on an identical tool request. This is a loop guard, not a replacement for domain idempotency: an event receipt or alert may still need its own duplicate checks.

### 3.4 Stopping and errors

The loop reports why it stopped, for example a final answer, turn or call budget, timeout, cancellation, or tool/model error. The caller can then distinguish a complete decision from an incomplete investigation. An incomplete investigation is not treated as a successful alert.

## 4. LiveGuard's domain layer

### 4.1 Reviewer: a new bounded session per investigation

The reviewer builds the request for one investigation. It supplies the current goal and version, relevant recent observations, evidence rules, and only the tools appropriate to the configured mode and model capability. It translates the generic loop's output into a LiveGuard decision.

The reviewer does not own the monitor lifecycle or final persistence. Those belong to the service and repository.

### 4.2 Tools are domain operations, not direct model powers

The tools expose narrow operations such as searching recent history, inspecting an image, looking up recent alerts, and proposing an alert. Each handler checks its prerequisites. For example, seeing matching OCR words is not by itself enough to claim that text appeared in the visible chat region: the visual check, OCR source, and image must refer to the same frame, and the location must meet the chat-region rule.

The tool descriptions explain these rules to the model, but Go handlers enforce the rules again. The model can propose a conclusion; it cannot bypass the server-side handler.

### 4.3 Service and repository: turn a suggestion into validated state

An alert tool call is a proposal. The service rechecks the current run and goal version, signal validity, evidence quality and freshness, and duplicate-alert rules. The repository persists only a decision that passes those checks. See [Architecture: evidence and alert submission](ARCHITECTURE.en.md#evidence-rules-and-alert-submission).

## 5. State and lifecycle

A single `Run` can contain many `SignalEvent` records, `Investigation` attempts, and accepted `Alert` records. `Pending` describes in-process queued or executing work; a pending investigation failing does not automatically stop the whole run.

The detailed fields, storage limits, observation log, and image-retention rules are described in [Architecture: cross-investigation context and persistence](ARCHITECTURE.en.md#cross-investigation-context-and-persistence).

## 6. Async work, deadlines, retries, and idempotency

### 6.1 Receiving a signal is separate from finishing it

When a model is enabled, `SubmitSignal` validates and accepts the signal, then hands work to the background queue and returns. Disconnecting the HTTP caller does not cancel accepted work because the queue uses the monitor's context. An expired signal will not start a new investigation. Pausing, stopping, or changing the goal cancels or invalidates old-version work.

For example, a signal can be queued against goal version 3, then the user changes the goal to version 4. The service removes queued version-3 work and cancels any running version-3 investigation. If the model returns later, the final check rejects its stale result.

### 6.2 Three different kinds of retry

| Kind | Current behavior | Why it is separate |
|---|---|---|
| Model HTTP transport retry | At most one retry for replayable requests after selected temporary HTTP statuses or transient network errors; observes cancellation and the overall deadline, and may honor a short `Retry-After` | The same model request may be resent when no reliable response arrived |
| Model-format correction | The reviewer can ask for one corrected structured response; it consumes the original model-turn budget | This asks the model to fix its output format; it is not a network retry |
| Business tool or alert retry | The Harness does not automatically replay it; a later model turn may ask again, but call budgets, domain checks, and idempotency still apply | Searching and proposing an alert must not be treated like a freely replayable HTTP read |

An OpenAI-compatible client request has a 60-second timeout by default. A loop investigation has a 90-second default, and one tool has a 12-second default. The signal deadline, run cancellation, or caller context may end the work earlier. The first deadline to expire wins. Request logs include investigation ID, turn, and timeout source to help tell these cases apart.

### 6.3 Circuit breaker

The model provider circuit breaker opens after two consecutive model failures by default and remains open for 30 seconds. During that period, new investigations fail quickly instead of repeatedly calling a known unhealthy provider. After the cooldown, one request may probe recovery. Success resets the failure state; failure keeps protection in place.

Cancellation and signal expiry are not provider-health failures and do not count toward the breaker. A breaker limits futile requests; it is not a provider-recovery mechanism.

### 6.4 Idempotency and duplicate suppression

Each signal has an event ID and content fingerprint. A run stores a bounded set of receipt records. The same ID with the same content is recognized as a duplicate; the same ID with different content is a conflict. If the queue is full, the service removes the receipt for work that was never enqueued, so the producer can retry the same ID after backpressure clears.

This is idempotency on the local signal-acceptance path. It does not promise global exactly-once execution across machines or external systems.

Alerts have a separate duplicate check. The model can look up recent alerts as context, and the service checks room, event type, and summary before persistence. Two tool calls do not automatically create two alerts.

The current simple rule treats alerts as duplicates when, in the same room within two minutes, their summaries match after whitespace and punctuation are removed, or the new signal text exactly matches OCR/ASR text cited by the old alert. This is not a general semantic-similarity model; different wording with the same meaning may not be merged.

One edge case needs precise wording: a signal that has expired by the time it reaches the queue may still be recorded, counted as expired, and have its temporary image released while `SubmitSignal` returns success. That means the acceptance/recording path handled it, not that it entered the queue or completed an investigation. Callers must interpret Pending state rather than treating HTTP acceptance as a guaranteed alert.

## 7. Context, images, and runtime records

- The first model turn can include up to 6 recent observations, with up to 600 characters per observation. History search allows up to 6 pages, 12 records per page, and 400 characters of source text per record.
- A single investigation's history-search results are capped at 12,000 Unicode characters. If that budget is reached, only whole records that fit are returned and the model is told not to keep paging.
- The 12,000-character history budget does **not** include the system prompt, tool definitions, images, assistant responses, or other context. It is not a global token limit for model input.
- Each model response is capped at 4,096 output tokens. Actual input and output usage is recorded in review metrics and model logs.
- During an investigation, `Observation` temporarily carries tool calls, tool outputs, and timing for the next turn. Persisted `AgentStep` records the tool name, purpose, result summary, success flag, and duration. Review metrics record model, token usage, total duration, turn count, stop reason, format corrections, and transport retries.
- Persisted records do not guarantee the full model prompt, every raw tool argument, or every raw tool response. There is no one-click replay of a complete investigation. Say “tool-step summaries and runtime metrics are recorded,” not “every part of the Agent trace is stored.”
- Image use is bounded by image size, investigation scope, and temporary-file lifetime. Whether image understanding is available depends on actual model configuration and archived evidence. OCR text alone is not a visual conclusion.

“Async task” does not mean “streaming model tokens.” Investigations run in a background queue; the browser receives state updates over HTTP/SSE. SSE sends state-change hints, not each token as the model generates it.

## 8. What tests and measurements prove

### 8.1 Fault-regression tests

The repository has automated tests for Harness boundaries, not only a happy-path conversation:

| Test file | Main behavior covered |
|---|---|
| [`loop_test.go`](../internal/agent/loop_test.go) | Tool allowlist, call budget, repeated calls, terminal tools, timeout, format-correction budget |
| [`retry_test.go`](../internal/agent/retry_test.go), [`circuit_breaker_test.go`](../internal/agent/circuit_breaker_test.go) | Temporary HTTP retry, cancellation, circuit-breaker behavior, single half-open recovery probe |
| [`execution_test.go`](../internal/monitor/execution_test.go) | Domain tool prerequisites, tool-result return, image/chat evidence rules, stop cancellation |
| [`pending_test.go`](../internal/monitor/pending_test.go) | Async acceptance, backpressure, same-ID retry, expiry/merge, goal-change cancellation, concurrency bounds |
| [`state_repository_test.go`](../internal/monitor/state_repository_test.go), [`service_test.go`](../internal/monitor/service_test.go) | State transitions, concurrent updates, investigation-result commit, service path |

These tests show whether code follows its boundary rules. They do not replace evaluation of real model quality.

### 8.2 Fixed-text evaluation is not currently a quality claim

The code still contains fixed-text cases and an evaluation entry point, but the latest real-model run reported zero precision. The result-classification path needs repair and every example needs manual review. Therefore the project does not publish a model-accuracy number or treat an old report as current performance.

Before restoring quality metrics, record the code version, model ID, case-set version, and actual alert result for each case. Then verify the denominators used for precision and recall. See [Evaluation](EVALUATION.en.md).

### 8.3 Existing throughput and latency measurements have a limited scope

The project previously measured `Ingest` and `SubmitSignal` using synthetic signals, an in-memory repository, and a mock reviewer with a fixed 40 ms wait. The documented 160/800-signal results include queue wait, backpressure retries, and local concurrency limits. They demonstrate that a simulated service path was measured; they do not represent a real model or end-to-end livestream performance.

Real Chrome capture, OCR, ASR, model API, and time until an alert appears on the web page must be measured separately. See [Evaluation](EVALUATION.en.md).

## 9. One-minute interview explanation

> By Harness I do not mean only a model loop. `internal/agent/loop.go` is the generic core: it applies turn and tool budgets, an allowlist, repeated-call detection, per-tool and overall deadlines, and returns tool results to the next model turn. `internal/monitor/agent_reviewer.go` connects that core to LiveGuard by building a separate session and context for each investigation, registering history-search, image-inspection, duplicate-check, and alert-proposal tools, then translating the result into a decision. The outer service handles asynchronous queuing and concurrency. Before saving, Go checks the goal version, evidence, expiry, and duplicate-alert rules. Transport retry, format correction, and business-tool retry are separate: transport retries at most once, format correction happens at most once, and the Harness does not automatically replay an alert. Runtime records include step summaries, token usage, duration, and stop reason. Validation includes fault-regression tests, mock-load measurement, and runtime timing records. The fixed-text evaluation path still needs repair, and mock load is not real livestream performance.
### Common interview follow-up questions

**“Why doesn't `loop.go` do everything?”**

> It is a generic loop and should not know about livestream evidence, chat-region coordinates, or duplicate-alert rules. The loop enforces execution budgets; the Monitor layer registers the domain tools and business checks. This keeps the loop reusable while allowing the final persistence path to recheck domain rules.

**“Can the model send an alert directly?”**

> No. The model can call the allowlisted `create_alert` tool to propose one. The tool handler and service validate evidence; the service also checks that the run is still active, the goal version is current, the signal has not expired, and the alert is not a duplicate. The repository performs the final save.

**“How do you avoid duplicate work when async tasks are retried?”**

> Signal acceptance uses event ID and a content fingerprint. If the queue is full, the receipt for work that was never enqueued is removed so the producer can retry with the same ID. Alert creation has its own duplicate check. This covers local acceptance and alert rules; it is not cross-machine exactly-once delivery.

**“Is your reported P95 an end-to-end P95?”**

> No single end-to-end number is currently reported. There are synchronous Ingest and asynchronous SubmitSignal queue/processing measurements with a mock reviewer, plus historical P95 from fixed-text model investigations. The full path from a live event through browser capture, OCR, model, alert persistence, and web display needs a separate measurement.
