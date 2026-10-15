# LiveGuard Documentation

[English](README.en.md) | [简体中文](README.md)

LiveGuard is a locally run livestream-monitoring agent. This index points to the project guides. Each guide has an English and a Simplified Chinese version.

## First-time reading path

1. [Project guide](PROJECT-GUIDE.en.md) — what the project does, how the folders are organized, and how a signal becomes an alert.
2. [Architecture](ARCHITECTURE.en.md) — module boundaries, data flow, tool permissions, and evidence rules.
3. [Agent Harness](AGENT-HARNESS.en.md) — how a bounded investigation runs, including state, async work, retries, idempotency, persistence, and evaluation limits.
4. [Operations](OPERATIONS.en.md) — starting the app, configuring models and storage, troubleshooting, and backing up local data.
5. [Evaluation and verification](EVALUATION.en.md) — automated coverage, real runtime latency records, synthetic load results, and what they do and do not prove.
6. [Improvement roadmap](ROADMAP.en.md) — known problems, candidate improvements, and evidence for deciding whether they worked.

From the repository root, read the [README](../README.md) for features and quick start, or the [contribution guide](../CONTRIBUTING.md) for the development workflow.

## Find a guide by question

| Question | Guide |
|---|---|
| Which Go files should I read first? | [Project guide](PROJECT-GUIDE.en.md) |
| How do goal planning, model tools, evidence, and alerts fit together? | [Architecture](ARCHITECTURE.en.md) |
| What is included in the Harness, and how does a model suggestion become an alert? | [Agent Harness](AGENT-HARNESS.en.md) |
| What happens when I pause, stop, change the goal, time out, or restart? | [Project guide](PROJECT-GUIDE.en.md), [Operations](OPERATIONS.en.md) |
| How do I configure MiniMax, OpenAI, DeepSeek, Tesseract, or ASR? | [Operations](OPERATIONS.en.md) |
| What has been tested or measured for throughput and latency? | [Evaluation and verification](EVALUATION.en.md) |
| What is worth improving, and how will we know it worked? | [Improvement roadmap](ROADMAP.en.md) |

## How to read claims in these guides

The guides distinguish three kinds of statements:

- **Implementation facts:** the corresponding Go code or configuration is linked.
- **Measurements:** apply only to the stated dataset, environment, and path. Historical numbers do not automatically describe the current code.
- **Known limitations:** capabilities that are not implemented or not measured are described as such.

Model output, queue admission, backend alert creation, UI updates, and a user seeing an alert are separate stages. When reading performance, reliability, or evaluation claims, check which stage is being measured.
