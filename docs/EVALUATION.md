# 评测与验证

## 评测与验证边界

### 日志、调查轨迹与 Trace 的位置

| 数据 | 默认位置 | 内容与边界 |
|---|---|---|
| 服务运行日志 | `runtime/liveguard.log`，相对于启动 LiveGuard 时的工作目录；可用 `LIVEGUARD_LOG_PATH` 改写 | 启动/关闭、采集和模型请求日志；最大 10 MiB，滚动保留一个 `liveguard.log.1`。模型请求记录含 `review_id`、轮次、token、耗时和错误，可用来串联一次调查的请求。 |
| Agent 调查轨迹 | `runtime/liveguard.json` 的各 Run `investigations` 字段；状态文件可由 `LIVEGUARD_LOCAL_STATE_PATH` 改写 | 保存调查状态、结论、证据引用、工具步骤摘要、token/耗时和停止原因；不是完整 prompt、原始工具参数/返回或可重放对话。Run 快照有保留上限。 |
| 完整观测流水 | 默认 `runtime/liveguard.json.observations.jsonl` | 追加保存 OCR、ASR 和画面观测；不等于 Agent 工具调用轨迹。 |
| OpenTelemetry 分布式 Trace | 默认不导出，也没有本地 Trace 文件 | 配置 `LIVEGUARD_OTEL_ENDPOINT` 后通过 OTLP/gRPC 发往该 Collector；Trace 实际保存位置由外部 Collector 决定。当前覆盖范围有限，不能替代调查轨迹和模型请求日志。 |

程序日志和调查轨迹是两类记录：排查某次模型请求的 HTTP 状态、轮次或超时，查 `runtime/liveguard.log` 并按 `review_id` 关联；回看 Agent 的工具步骤和最终状态，查 `runtime/liveguard.json`；检查 OCR/ASR 实际采到的内容，查 `.observations.jsonl`。本机测试运行还可能把 Go 测试输出另存为该次测试目录里的 `soak.log`，它不是服务默认日志。

### 最近一次验证与负载：2026-10-15

此前的同步负载结果对应 commit `9fdf9d5e06b78e1af582a09af810a46285249afb`。本节新增的异步负载、协作闭环测试和门禁结果，是在该 commit 的当前工作区修改上测得；工作区当时为 dirty，不能把这些结果描述为该 commit 的已提交结果。Go `go1.26.3 windows/amd64`，16 个逻辑 CPU；硬件型号未记录。

| 验证项 | 结果 |
|---|---|
| `go test ./...` | 通过，退出码 0 |
| `go vet ./...` | 通过，退出码 0 |
| `go build ./...` | 通过，退出码 0 |
| `node --check internal/web/static/monitor.js` | 通过，退出码 0 |
| 协作闭环服务级测试 | 通过：Agent 求助、用户回答、同一调查复核并继续运行；使用 mock Reviewer，不验证真实模型是否会在正确时机提问 |
| OCR HTTP multipart 路径 | 既有 `TestMonitorOCRMultipartFlowsIntoAgent` 通过：静态 OCR stub 识别文本后进入 Agent；不验证真实 Tesseract 图片识别质量 |
| Tesseract 合成图片 + OCR HTTP | 通过 1 次：本机 Tesseract `chi_sim+eng` 精确识别 3 行测试文字，并经实际 `/api/monitors/{id}/ocr` multipart 路径写入 OCR 观测；单次感知耗时 390 ms，未调用模型 |
| 同步合成负载 | 160 条、800 条各运行 3 次；6 次均全部接收、错误数为 0，模型判断并发峰值为 4 |
| 160 条：同步 Ingest P95 / 吞吐 | 5–6 ms / 1,043–1,181 条每秒 |
| 800 条：同步 Ingest P95 / 吞吐 | 30–32 ms / 411–427 条每秒 |
| 异步 `SubmitSignal` 负载 | 160、800 条各运行 3 次；全部入队并处理，0 错误；真实队列容量和全局判断并发受限均被覆盖。详细数字见下表 |
| 付费模型评测 | 未运行；未使用 holdout 调参 |

同步负载使用 8 个 worker、8 个内存 Run 和固定等待 40 ms 的 mock Reviewer，只测同步 `Service.Ingest`。异步结果在下方单独报告。负载不代表真实模型耗时或采集到提醒的端到端延迟。Tesseract 使用本机生成的合成 PNG，在临时服务中通过 OCR 上传接口验证，没有启用模型或真实直播。图片和六份异步原始 JSON 保存在本机忽略目录 `.cache/test-2026-10-15/`，没有提交到仓库。

### 异步队列合成负载：`SubmitSignal`

在内存 Repository 上创建 8 个 Run，以 8 个提交 worker 并发调用生产路径 `Service.SubmitSignal`。每条信号使用独立文本，避免 OCR 数值修订去重；信号有效期设为 30 秒，以覆盖短时效信号的快速复核路径。Reviewer 是固定等待 40 ms 的 mock，不访问网络或真实模型。队列满时记录 `ErrPendingFull`，并用同一事件 ID 重试，直到接收。全局 Reviewer 并发上限仍由服务本身控制。

每个规模 3 次的结果范围如下；吞吐按提交开始到全部队列排空计算，P95 为各轮结果范围。`review_queue_p95` 由提交调用开始计至 Reviewer 开始，`submit_to_review_done_p95` 计至 mock Reviewer 返回，不包含后续数据库持久化或浏览器显示。

| 信号数 | 入队/处理 | 背压重试 | 完整处理吞吐 | Submit P95 | 排队至 Reviewer 开始 P95 | Reviewer 耗时 P95 | 提交至 Reviewer 返回 P95 | 峰值并发 / 每 Run 等待队列 |
|---:|---|---:|---:|---:|---:|---:|---:|---|
| 160 | 160/160，0 错误（3/3 轮） | 590–716 | 84.9–86.6 条/秒 | 9–17 ms | 812–844 ms | 40 ms | 852–885 ms | 4 / 8 |
| 800 | 800/800，0 错误（3/3 轮） | 1,802–2,273 | 32.9–40.6 条/秒 | 74–101 ms | 2,687–3,361 ms | 40 ms | 2,728–3,401 ms | 4 / 8 |

这些是 8 个 Run 同时积压且 40 ms mock Reviewer 的本机合成数据，不能据此声称真实模型或真实直播能达到相同速度。提交入队后仍需等待前序任务；800 条时效信号的队列等待 P95 约为 2.7–3.4 秒，说明有限并发与队列确实会形成等待。未测普通信号按目标计划节奏限频后的端到端提醒，也未测真实模型、Chrome、Tesseract、数据库写入和网页到达时间。

### 真实运行测试：8 路持续采集与高密度 OCR，2026-10-15

这次测试针对“同时开多个直播间，运行数分钟后采集异常”的问题，检查多窗口采集和 OCR 负载下服务能否持续工作。基线为 commit `9fdf9d5e06b78e1af582a09af810a46285249afb`，测试运行时工作区为 dirty，因此结果属于当时的工作区代码，不能单独归因于该 commit。环境为 Windows/amd64、Go `go1.26.3`；使用 8 个本地动态 HTML 测试页、真实 headless Chrome 截图、本机 Tesseract `chi_sim+eng` 和每次等待 15 秒的模拟 Reviewer；没有真实平台登录或付费模型请求。测试持续 360.6 秒。

| 指标 | 结果 |
|---|---|
| 监控窗口 | 8 路全部保持 `running`，结束时 8/8 采集健康状态为 `healthy` |
| 截图尝试 | 每路 71–73 次 |
| 实际采样间隔 | 约 4.0–6.0 秒；高密度 OCR 下从计划的 1 秒自适应放慢 |
| 感知队列跳过 | 8 路合计 20 帧；系统以丢弃积压感知帧换取采集继续前进，因此不代表每帧都完成 OCR/Agent 复核 |
| Agent 调度 | 同一监控没有重叠调查；全局 Reviewer 峰值并发为 4 |
| 采集异常 | 本轮未复现“约 5 分钟后所有窗口采集异常” |

这次验证观察到的缓解行为是：密集 OCR 使有效截图间隔自动放慢到约 5 秒，并允许感知队列跳过来不及处理的帧；8 路截图仍持续推进，采集状态没有转为失败。它说明该本机合成场景没有复现此前问题，不等于已证明 8 路真实直播平台、真实登录状态或 MiniMax 端到端稳定，也不测评论漏采率或提醒正确率。

原始报告位于本机忽略目录 `.cache/stability-2026-10-15-8rooms-dense/report.json`；同目录还有 `soak.log`、`state.json`、`state.json.observations.jsonl` 和测试用 `chrome-profile/`。这些文件未提交到 Git；`report.json` 含完整合成观测，约 6.2 MiB。复跑入口是 [`TestEightBrowserMonitorsSustained`](../internal/monitor/stability_soak_test.go)，需设置 `LIVEGUARD_STABILITY_DURATION=6m` 和 `LIVEGUARD_STABILITY_OUTPUT` 指向该输出目录，再执行：

~~~powershell
$env:LIVEGUARD_STABILITY_DURATION = "6m"
$env:LIVEGUARD_STABILITY_OUTPUT = Join-Path (Get-Location) ".cache/stability-2026-10-15-8rooms-dense"
go test ./internal/monitor -run TestEightBrowserMonitorsSustained -count=1 -v
~~~

### 历史文字评测：2026-10-04

旧报告记录 MiniMax-M3 在 30 条固定文字案例上通过 27/30，关键词基线通过 24/30；提醒 precision / recall / FPR 为 0.875 / 0.7778 / 0.0476，单条调查 P95 为 21,755 ms。它是当时 Reviewer 的文字样例评测，不是当前代码的重新评测，也不能和上面的同步负载数字直接比较。完整报告位于本机忽略文件 `.cache/monitor-eval-holdout-v1.first.json`；该 holdout 已被读取，属于已打开的数据集，不能解释为未见独立评测。

### Harness 与故障回归

各层分别负责什么、哪些限制属于通用循环、哪些由 Monitor/Service 承担，以及一次调查的完整调用路径，见[Agent Harness 详解](AGENT-HARNESS.md)。

- [loop_test.go](../internal/agent/loop_test.go)：白名单、预算、参数与重复调用、终止工具、超时。
- [retry_test.go](../internal/agent/retry_test.go)、[circuit_breaker_test.go](../internal/agent/circuit_breaker_test.go)：有限重试、取消、熔断与探测。
- [execution_test.go](../internal/monitor/execution_test.go)：格式纠正、协议消息回传、看图/聊天前置条件、停止取消。
- [pending_test.go](../internal/monitor/pending_test.go)：慢模型下采集继续、截图所有权、满队列重试、TTL/合并、改目标取消、跨窗口并发。
- [state_repository_test.go](../internal/monitor/state_repository_test.go)：状态迁移、ID冲突、并发仓储和本地文件重启恢复。
- [eval.go](../internal/monitor/eval.go)：固定文字开发集/冻结集、基线、行为指标、延迟与 token。
- [load.go](../internal/monitor/load.go)：同步 `Ingest` 与异步 `SubmitSignal` 的合成信号 + mock 模型负载；不包含 Chrome/OCR/ASR/真实 API。

### 吞吐和延迟：已经测到的部分

仓库里确实有吞吐/延迟相关代码，不需要把项目说成“完全没有性能测量”。但每个数字必须带上测量范围：

| 工具 | 测什么 | 报告字段 |
|---|---|---|
| `cmd/monitor-load` → `RunSyntheticLoad` | 8 个并发 worker 往 8 个内存 Run 调用同步 `Service.Ingest`；Reviewer 是固定等待 40ms 的 mock | `requests_per_second`、`p95_ingest_ms`、`elapsed_ms`、接受/错误数、背压、模型判断数、判断并发峰值 |
| `cmd/monitor-load -mode async` → `RunAsyncSyntheticLoad` | 8 个并发 worker 通过 `Service.SubmitSignal` 向 8 个内存 Run 入队；30 秒有效期让信号走时效快速复核，mock Reviewer 固定等待 40ms；满队列时按同 ID 重试 | 入队/处理数、背压重试、完整处理吞吐、提交 P95、提交至 Reviewer 开始 P95、Reviewer 耗时和返回 P95、并发与队列峰值 |
| `cmd/monitor-eval` → `RunSignalEvalCases` | 固定文字案例逐条调用实际配置的 Reviewer | 单条调查 P95、token、工具调用、precision/recall/FPR、每案例结果 |
| `scripts/export-monitor.ps1` | 从现有 Run 导出人工验收数据 | 采到信号到后端提醒的延迟统计、保留调查的 token 汇总 |

合成负载命令：

~~~powershell
go run ./cmd/monitor-load -requests 160 -out .cache/monitor-load.json
go run ./cmd/monitor-load -mode async -requests 160 -out .cache/monitor-async-load.json
~~~

两个 harness 都固定 8 个 Run、8 个 worker、mock 判断延迟 40ms，最多接受 10,000 条请求，命令有 1 分钟 context deadline。同步模式 `p95_ingest_ms` 是一次 `Ingest` 调用耗时，吞吐在提交过程结束时计算。异步模式的吞吐覆盖提交和队列排空；`p95_submit_ms` 只测 admission 调用，`p95_queue_wait_ms` 从 `SubmitSignal` 调用开始计至 Reviewer 开始，`p95_submit_to_review_done_ms` 计至 Reviewer 返回。mock 延迟不是模型 P95。

### 尚未测量的路径

异步时效快速路径的合成吞吐和排队 P95 已测；普通信号按计划限频时的端到端延迟、真实直播漏采率、真实模型下采集到提醒的端到端延迟，以及网页实际收到提醒的时间仍未测。真实直播漏采率需要完整源记录与采集结果对照；异步合成负载不能代表真实采集链路。

固定文字评测已记录模型单条调查 P95；历史摘要见上文。它与合成负载 `Ingest` P95 分别描述不同路径。

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
