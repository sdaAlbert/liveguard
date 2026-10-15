# Contributing to LiveGuard

LiveGuard focuses on one flow: personal livestream monitoring with a goal-aware Agent and evidence-backed alerts. Contributions should improve capture, signal handling, Agent decisions, tool safety, or user-visible reliability.

## Before you start

1. Read the [README](README.md) for the project scope and startup instructions.
2. Use the [documentation index](docs/README.md) to find the relevant architecture, harness, operations, or evaluation guide.
3. Check the [improvement roadmap](docs/ROADMAP.md) for known gaps and candidate work.

Roadmap items marked **Candidate** are ideas to discuss, not approved work or a delivery promise. Before starting a large change, agree on the problem, scope, and evidence that will show the change helped. Update the roadmap status when the work is accepted, started, completed, or deferred.

## When to write an RFC or ADR

- Write an **RFC** when a change has multiple plausible designs, affects several modules, or needs feedback before implementation. Include the problem, options, trade-offs, open questions, and a way to evaluate the decision.
- Write an **ADR** when the team has made a durable technical decision and future contributors will need to understand why it was chosen.
- For a small, reversible change with a clear solution, a focused issue or pull request is enough. Do not create process documents just to make a change look larger.

Link the RFC, ADR, issue, or pull request from the related roadmap item so contributors can follow the discussion and see the outcome in one place.

## Local workflow

The supported quick-start path uses Windows PowerShell, Go, Google Chrome, and Tesseract. From the repository root:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\run-mvp.ps1
```

The default local mode stores monitor state in JSON files under `runtime/`; it does not require Docker or a database. Set `LIVEGUARD_INFRA_MODE=memory` to keep state in process memory only. That data is lost when the process exits. Optional integrations, such as OTLP tracing, require an externally managed service.

If MiniMax is configured in local OpenClaw settings, the launcher can reuse that provider. Never commit API keys, browser profiles, private screenshots, runtime state, or local evaluation output.

## Implementation and verification

- Keep business rules in the existing monitor flow; add infrastructure only when the problem and benefit are clear.
- Preserve the boundaries documented in [ARCHITECTURE.md](docs/ARCHITECTURE.md) and [AGENT-HARNESS.md](docs/AGENT-HARNESS.md). In particular, the model proposes decisions while Go enforces tool permissions, budgets, evidence checks, and state changes.
- Add or update focused tests for behavior and failure cases. For a Go change, run `go test ./...` when the local environment permits it, and report any package that could not run.
- For model quality, queue throughput, or latency claims, follow [EVALUATION.md](docs/EVALUATION.md). State the dataset, mock or real dependencies, workload, and what the measurement does not cover.

## Pull request checklist

- Explain the user or engineering problem and link the related roadmap item or issue.
- Describe the change in plain language and call out relevant boundary or failure behavior.
- Report tests, evaluations, or measurements that were run; do not describe unmeasured behavior as verified.
- Update the roadmap status and add a link to the merged pull request and completion evidence when the work addresses a roadmap item.
- Update user and developer documentation when behavior, setup, configuration, or recovery steps change.

## Project map

- `cmd/`: executable entry points, including the web service and evaluation tools.
- `internal/agent`: the shared, bounded model/tool execution loop.
- `internal/monitor`: monitor lifecycle, evidence, tools, alerts, persistence, and evaluation.
- `internal/browser`: dedicated Chrome session and frame capture.
- `internal/web`: monitor UI and HTTP API.
- `docs/`: architecture, project guide, operations, evaluation, harness details, and the improvement roadmap.

Redis, Kafka, external schedulers, batch inspections, and the old Sandbox Lab are outside the current scope. Propose them only when a demonstrated problem in the monitor flow requires them.
