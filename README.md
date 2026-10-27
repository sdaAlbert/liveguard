# LiveGuard

[English](README.md) | [简体中文](README.zh-CN.md)

[![CI](https://github.com/sdaAlbert/liveguard/actions/workflows/ci.yml/badge.svg)](https://github.com/sdaAlbert/liveguard/actions/workflows/ci.yml)

LiveGuard is a locally run livestream monitoring agent for people who want alerts without watching several livestream screens continuously. Add a livestream URL and describe what to watch for. LiveGuard collects visible text, can transcribe audio you explicitly share, and keeps evidence-backed alerts in a local monitoring room.

It supports livestream pages from Douyin, Bilibili, Kuaishou, and similar platforms through the same browser, OCR, and agent workflow. Bilibili is the main development and multi-room test platform. Platform login, anti-abuse controls, and concurrent-viewing limits still apply; LiveGuard does not bypass them.

## What it does

- Captures a dedicated Chrome session and reads visible livestream text with local OCR.
- Optionally transcribes browser audio that you explicitly share; the current ASR integration uses MiniMax.
- Uses an agent to investigate signals, search recorded observations, and inspect a frame when needed.
- Stores monitor state, observations, and recoverable pending signals in local SQLite. JSON is available for inspection and archiving through an export command.
- The web interface can switch between English and Simplified Chinese using the language button in the monitoring room; the selection is remembered in the browser. Stream names, monitoring goals, evidence, and agent-generated findings remain in their original language.

## Screenshots

The monitoring room summarizes active monitors and pending work.

![LiveGuard monitoring room with multiple active monitors](docs/images/monitor-room.png)

The detail page shows the agent's current status, evidence, and investigation history.

![LiveGuard monitor detail page showing evidence and agent review](docs/images/investigation-detail.png)

## Quick start

Requirements: Windows 10/11, Go 1.25, Google Chrome, and Tesseract OCR. For Chinese text recognition, install the `chi_sim` language data.

```powershell
git clone https://github.com/sdaAlbert/liveguard.git
cd liveguard
powershell -ExecutionPolicy Bypass -File ./scripts/run-mvp.ps1
```

Open <http://127.0.0.1:8080>, sign in to the livestream platform in the dedicated Chrome window, and create a monitor. To use an LLM, open **Model settings** and configure a provider and API key. Saving settings does not send a probe request.

Use the **EN / 中文** button in the top-right corner of the monitoring room to change the interface language. The choice is saved in the current browser.

## Project boundaries

LiveGuard only analyzes signals it captures; sampling intervals, visible page content, and OCR quality affect coverage. It stores alerts in the app and does not send them to WeChat, SMS, or other external channels. Eight active monitors is a scheduling limit, not a promise that every computer can OCR eight streams every second. The supported quick-start flow currently targets Windows.

## Documentation

- [Documentation index](docs/README.en.md): reading paths and topic index.
- [Project guide](docs/PROJECT-GUIDE.en.md): folders, terminology, request flow, state, and test map.
- [Architecture and agent behavior](docs/ARCHITECTURE.en.md): evidence, tools, scheduling, context, and persistence design.
- [Operations and troubleshooting](docs/OPERATIONS.en.md): startup, configuration, local data, limits, and recovery.
- [Evaluation and verification](docs/EVALUATION.en.md): automated tests, runtime records, synthetic load, and evidence limits.
- [Agent Harness guide](docs/AGENT-HARNESS.en.md) · [Improvement roadmap](docs/ROADMAP.en.md) · [Contributing](CONTRIBUTING.md)
- [简体中文文档导航](docs/README.md)

## Update history

- **2026-10 — SQLite persistence:** Replaced runtime JSON snapshot and JSONL storage plus the in-memory pending queue with SQLite. This brings monitor state, observations, and accepted pending signals under one transactional store, so valid queued work can be recovered after restart. The duplicate JSON runtime repository was removed; JSON export remains for inspection and archiving and does not include pending work.

## Security and privacy

The server listens on localhost by default and is not intended to be exposed directly to the public internet. API keys stay in the Go process memory or configured environment variables. Do not commit secrets, login data, or private screenshots. See the [operations guide](docs/OPERATIONS.en.md) for details.

## License

[MIT](LICENSE)
