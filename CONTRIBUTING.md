# Contributing

LiveGuard focuses on one flow: personal livestream monitoring with a goal-aware Agent and evidence-backed alerts. Changes should improve capture, signal handling, Agent decisions, tool safety, or user-visible reliability.

## Local workflow

Requires Go, Windows PowerShell, Google Chrome, and Tesseract. From the repository root:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\run-mvp.ps1
```

The local monitor uses memory by default and does not require Docker. PostgreSQL persistence and OTLP tracing are optional integrations for externally managed services; they are not part of the default workflow.

If MiniMax is configured in local OpenClaw settings, the launcher can reuse that provider. Do not commit API keys, browser profiles, private screenshots, or local runtime data.

## Project map

- `internal/agent`: shared, bounded ReAct execution loop.
- `internal/monitor`: monitor lifecycle, evidence, tools, alerts, and evaluation.
- `internal/browser`: dedicated Chrome session and frame capture.
- `internal/web`: monitor UI and HTTP API.

Redis, Kafka, task schedulers, batch inspections, and the old Sandbox Lab are outside the current scope. New infrastructure should address a demonstrated need in the monitor flow.
