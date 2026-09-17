# Contributing to LiveGuard

LiveGuard keeps a deliberately narrow scope: personal, goal-driven livestream monitoring plus policy-gated evidence tools. Contributions should strengthen the monitoring state model, signal pipeline, reliability, or explanation.

## Prerequisites

- Go version declared in `go.mod`
- Docker Desktop with a Compose v2 release that supports `docker compose up --wait`
- Google Chrome
- Node.js 22 or newer for the JavaScript syntax check
- Windows PowerShell for the documented one-command and manual workflows

The current workflows are verified on Windows. Linux and macOS users can translate the process environment assignments, but those paths are not yet documented as supported.

## Set up

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\demo.ps1
```

Run commands from the repository root. The default example forces `LIVEGUARD_LLM_MODE=deterministic`, so `.env.local` and an API key are optional. To exercise online model routing, copy `.env.example` to `.env.local`, add your own key, and change the mode to `auto` before starting the processes manually.

Stop the local demo with:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\demo.ps1 stop
```

The stop command keeps PostgreSQL and Redis volumes. Use `docker compose down -v` only when you intentionally want to delete the local demo data.

## Start processes manually

Start infrastructure first:

```powershell
docker compose up -d --wait
docker pull golang:1.26
```

In terminal A:

```powershell
$env:LIVEGUARD_SANDBOX_RPC_ADDR="127.0.0.1:9090"
$env:LIVEGUARD_OTEL_ENDPOINT="127.0.0.1:4317"
$env:LIVEGUARD_SANDBOX_IMAGE="golang:1.26"
go run ./cmd/sandbox-worker
```

In terminal B:

```powershell
$env:LIVEGUARD_INFRA_MODE="durable"
$env:LIVEGUARD_LLM_MODE="deterministic"
$env:LIVEGUARD_HEADLESS="true"
$env:LIVEGUARD_SANDBOX_RPC_TARGET="127.0.0.1:9090"
$env:LIVEGUARD_OTEL_ENDPOINT="127.0.0.1:4317"
go run ./cmd/liveguard
```

For online routing, load `OPENAI_API_KEY`, set `LIVEGUARD_LLM_MODE=auto`, and optionally set `LIVEGUARD_MODEL` in terminal B. The dashboard decision audit must show `openai:<model>` to count as an online result.

## Validate a change

```powershell
go test ./...
go vet ./...
node --check internal/web/static/app.js
node --check internal/web/static/monitor.js
```

For changes to Sandbox behavior, also run the four-case Sandbox Lab from the UI and verify that every temporary container is removed. For Agent routing changes, run the six-case Agent Eval and include expected/actual results in the pull request.

## Repository map

- `cmd/liveguard`: control plane and web server
- `cmd/sandbox-worker`: gRPC process with Docker access
- `internal/agent`: planning, tool routing, policy, evals, and circuit breaker
- `internal/browser`: Chrome evidence collection
- `internal/monitor`: personal monitor state, goal-aware signal investigation, persistence, and replay source
- `internal/sandbox`: PostgreSQL Outbox, Redis Streams, execution lifecycle, and policies
- `internal/sandboxrpc`: gRPC client and server adapters
- `internal/web`: HTTP API and embedded dashboard
- `api/sandbox/v1`: protobuf contract
- `docs`: architecture notes and public verification images

Generated protobuf stubs are checked in. They currently record `protoc` 5.27.3, `protoc-gen-go` 1.36.11, and `protoc-gen-go-grpc` 1.6.2. After installing those tools, regenerate both outputs from the repository root:

```powershell
protoc --go_out=. --go_opt=module=liveguard --go-grpc_out=. --go-grpc_opt=module=liveguard api/sandbox/v1/sandbox.proto
```

Include the `.proto` and generated Go changes in the same pull request.

## Pull requests

Keep pull requests focused. Describe the concrete failure or user need, the resulting behavior, and how it was verified. Avoid adding infrastructure solely to lengthen the technology list; new dependencies should solve an observed limitation and include a removal or rollback path.

Never commit API keys, browser profiles, screenshots containing private data, local state, or Docker credentials.
