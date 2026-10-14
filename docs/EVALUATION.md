# 评测与验证

## 评测与验证边界

### 历史首次文字评测：27/30，基线 24/30

本地 .cache/monitor-eval-holdout-v1.first.json 保存了一份完整的 MiniMax-M3 历史报告：

| 项目 | 首次历史结果 |
|---|---|
| 时间 | 2026-10-04 17:25:32（Asia/Shanghai；报告记录 UTC 09:25:32） |
| 数据集 | holdout-v1，30 条固定文字；另有 dev 10 条 |
| 数据集 SHA256 | c780f9e306a30c3263c882ece0b636ad3a0b98f1d799e413ebdc96d631076c66 |
| 模型通过 / 关键词基线通过 | 27/30（90%） / 24/30（80%） |
| 提醒 precision / recall / FPR | 0.875 / 0.7778 / 0.0476 |
| 模型 API 错误 | 0 |
| 单条调查 P95 耗时 | 21755 ms |
| 报告 input/output tokens | 37905 / 10301 |
| 工具调用数 | 97 |

3 个失败分别是 h11_stream_breaking_news、h15_custom_topic_start（未在执行预算内形成命中结论），以及 h21_chat_question_without_frame（无图时误报聊天命中）。当前代码增加了聊天区域的工具与提交约束，**不能因此直接把旧 27/30 说成当前测得结果**。

这是端到端调查适配器的文字样例评测，不是直播采集召回率、真实评论识别准确率、口播质量或用户端延迟。“通过”包含正确不提醒，precision/recall 是提醒行为指标；不代表全部文字解释正确。评测路径直接调用 Reviewer，不能覆盖所有 Service 提交约束。

原报告在被忽略的本地缓存里，未作为随仓库公开的可复现结果文件；本文记录的是可核实的历史摘要。冻结集既然已读取失败结果，之后再针对它修改提示词，就应将其视为已打开的回归集；独立泛化评估需要新的未见版本。

### Harness 与故障回归

- [loop_test.go](../internal/agent/loop_test.go)：白名单、预算、参数与重复调用、终止工具、超时。
- [retry_test.go](../internal/agent/retry_test.go)、[circuit_breaker_test.go](../internal/agent/circuit_breaker_test.go)：有限重试、取消、熔断与探测。
- [execution_test.go](../internal/monitor/execution_test.go)：格式纠正、协议消息回传、看图/聊天前置条件、停止取消。
- [pending_test.go](../internal/monitor/pending_test.go)：慢模型下采集继续、截图所有权、满队列重试、TTL/合并、改目标取消、跨窗口并发。
- [state_repository_test.go](../internal/monitor/state_repository_test.go)：状态迁移、ID冲突、并发仓储、重启停止；PostgreSQL 合同测试需要专用数据库环境，默认跳过。
- [eval.go](../internal/monitor/eval.go)：固定文字开发集/冻结集、基线、行为指标、延迟与 token。
- [load.go](../internal/monitor/load.go)：同步 Ingest 的合成信号 + mock 模型负载；不包含 Chrome/OCR/ASR/真实 API，不能证明异步队列吞吐或全链路容量。

### 吞吐和延迟：已经测到的部分

仓库里确实有吞吐/延迟相关代码，不需要把项目说成“完全没有性能测量”。但每个数字必须带上测量范围：

| 工具 | 测什么 | 报告字段 | 不代表什么 |
|---|---|---|---|
| `cmd/monitor-load` → `RunSyntheticLoad` | 8 个并发 worker 往 8 个内存 Run 调用同步 `Service.Ingest`；Reviewer 是固定等待 40ms 的 mock | `requests_per_second`、`p95_ingest_ms`、`elapsed_ms`、接受/错误数、背压、模型判断数、判断并发峰值 | 不测异步 `SubmitSignal` 排队等待、真实模型/API、Chrome、OCR、ASR、数据库或完整提醒链路 |
| `cmd/monitor-eval` → `RunSignalEvalCases` | 固定文字案例逐条调用实际配置的 Reviewer | 单条调查 P95、token、工具调用、precision/recall/FPR、每案例结果 | 不测实时视频采集，也不是用户看到网页提醒的端到端 P95 |
| `scripts/export-monitor.ps1` | 从现有 Run 导出人工验收数据 | 采到信号到后端提醒的延迟统计、保留调查的 token 汇总 | 不知道未采到的内容，也不测 SSE 到达浏览器的时刻 |

合成负载命令：

~~~powershell
go run ./cmd/monitor-load -requests 160 -out .cache/monitor-load.json
~~~

当前 harness 每次固定 8 个 Run、8 个 worker、mock 判断延迟 40ms，最多接受 10,000 条请求，整个命令有 1 分钟 context deadline。`p95_ingest_ms` 是一次同步 `Ingest` 调用的耗时分位数；`requests_per_second` 是接受数除以整个提交过程的耗时。`max_in_flight` 检查同一时间正在运行的 mock 判断峰值。它不是异步队列的吞吐，也不应把 mock 延迟写成模型 P95。

### 还要不要补压测？

如果项目说明只陈述“有合成同步负载测量”，已有 harness 足以支持这个有限说法；若要证明简历/面试中提到的**异步**、排队和端到端延迟，就应补测异步路径。最有价值的下一步是在 `SubmitSignal` 的真实有界队列上持续提交，等待后台处理收敛，再报告：

- `SubmitSignal` 接收延迟 P50/P95/P99 与每秒接收量；
- 从 `ObservedAt` 到开始调查、调查完成、提醒提交的等待时间分布；
- 接受、重复、合并、背压拒绝、过期、调查失败各自的数量；
- 每 Run 队列峰值、全局模型调查并发峰值和运行时长；
- 多档输入速率与 mock 延迟下的稳定性，及运行硬件、Go 版本和负载参数。

需要先决定是在同一进程用 mock 测队列/调度，还是连 PostgreSQL 与真实采集做端到端压测。前者便宜、可重复，适合回答队列是否遵守容量和并发限制；后者测到更多真实瓶颈，但需要隔离外部服务并控制模型费用。完成这组补测后，才可以把异步排队时延称为测量结果。真实直播的漏采率还需要对照原始直播录像或完整聊天记录，单纯压 `SubmitSignal` 无法测量。

固定文字评测已记录模型单条调查 P95；历史摘要见上文。不要把一次历史模型调用的 P95 与合成负载 `Ingest` P95 拼成一个“端到端 P95”。

普通验证：

~~~powershell
go test ./...
go vet ./...
go build ./...
node --check internal/web/static/monitor.js
# Windows 需可用的 C 编译器，才能进行 race 检查：
go test -race ./internal/agent ./internal/monitor ./internal/web
~~~

显式付费验证：

~~~powershell
# 真实目标规划与合成图片工具；不含真实直播采集/ASR
powershell -ExecutionPolicy Bypass -File ./scripts/verify-minimax-live.ps1 -AgentOnly
# 先开发集，再回归已打开的 holdout-v1
powershell -ExecutionPolicy Bypass -File ./scripts/verify-minimax-live.ps1 -Evaluate -Split dev
powershell -ExecutionPolicy Bypass -File ./scripts/verify-minimax-live.ps1 -Evaluate -Split holdout-v1
# 合成信号负载
go run ./cmd/monitor-load
# 对正在运行的会话导出人工验收资料
powershell -ExecutionPolicy Bypass -File ./scripts/export-monitor.ps1
~~~

真实验收应独立记录：评论首见到采到的时间、漏掉的评论/短暂活动、OCR区域及错字、入队等待、模型耗时、后端提醒时间和网页展示时间。export-monitor 当前只统计保留调查的 token 与采到信号至后端提醒的 P95，不测网页到达，也无法统计根本没采到的评论。需要人工对照或可核验的完整源记录。
