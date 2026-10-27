# 运行、配置与故障处理

[English](OPERATIONS.en.md) | 简体中文

第一次接触项目可先读[项目导览](PROJECT-GUIDE.md)，了解 Run、观测、调查和提醒分别是什么；本篇聚焦如何启动、配置、排障和备份。

## 运行时长与API额度

**LiveGuard是持续运行的监控。** Run使用context.WithCancel，没有总时长deadline、自动结束计时器或按StartedAt计算的十分钟上限。模型API可用、直播输入有效且服务保持运行时，会持续收集新信号，调查上下文会反复重建；不是让同一次模型请求一直运行。

- 用户点击停止、服务退出或明确的模型API额度/余额耗尽，会结束监控。
- 普通网络异常、瞬时限流、一次调查超时，不结束整个会话；有限重试后继续监听，连续模型请求错误达到阈值时才短暂熔断。
- 登录/验证码会暂停输入并提示用户处理；临时截图、OCR 或状态写入失败进入恢复状态，以最长 30 秒退避继续尝试，成功后恢复健康。
- API额度错误在共享适配层识别为ErrModelQuotaExhausted；当前配置的共享Reviewer记录不可用状态，停止该配置下的活跃窗口，取消采集和队列、释放窗口，并保留结束原因。
- Run.Status=failed、stop_reason=api_quota_exhausted、last_error说明额度原因；模型设置返回quota_exhausted，网页显示“API额度已耗尽”。普通HTTP429本身不会被认定为余额耗尽。
- 恢复额度/配额后，重新保存模型配置并新建监控。保存重置当前绑定的不可用状态，不会自动恢复已经结束的窗口，也不会在保存时测试Key。
- 不主动查询余额，也不提前按估算金额停止。只有实际模型请求返回明确的额度/余额不可用信息时触发终止；没有新信号时也不会为了耗尽API而空调用。

### 时间限制分别约束什么？

| 时间 / 限制 | 作用对象 | 是否结束监控会话 |
|---|---|---|
| 会话总时长 | Run | **无固定上限** |
| 单条观测/队列有效期 | 默认文字从观察时刻起 2 分钟，frame 30 秒；明确倒计时、相对未来时间或 valid_until 可覆盖默认值 | 仅停止过期调查；Run 和已入库的历史继续保留 |
| 90秒调查 / 12秒工具 / 60秒HTTP请求 | 单次调查、工具和模型请求 | 本次执行停止或失败，不是整个Run结束 |
| 15分钟评测程序期限 | cmd/monitor-eval离线评测 | 仅限制评测命令，与真实监控运行时长无关 |

源码入口：[Run创建与采集恢复](../internal/monitor/service.go)、[额度终止](../internal/monitor/quota.go)、[提供方错误分类](../internal/monitor/model_errors.go)。额度与限流按提供方错误元数据区分，不能根据直播文字中的“余额不足”触发：参考[OpenAI错误码](https://developers.openai.com/api/docs/guides/error-codes)、[MiniMax错误码](https://platform.minimax.io/docs/api-reference/errorcode)、[DeepSeek错误码](https://api-docs.deepseek.com/quick_start/error_codes/)。


## 本机数据文件与长期运行

LiveGuard 默认把监控状态写入本机 `runtime/liveguard.sqlite`，不连接外部数据库：

| 信息 | 默认位置 | 退出/重启后的行为 |
|---|---|---|
| Goal/Plan/Sampling、有限事件/调查/提醒快照 | `runtime/liveguard.sqlite` 的 `runs` 表 | 下次启动恢复；此前运行中的监控会被暂停，用户可手动继续 |
| 完整观测流水 | 同一数据库的 `observations` 表 | 和产生观测的 Run 更新在同一事务中提交，可分页查询 |
| 待处理信号、排队顺序和等待中的图片 | 同一数据库的 `pending_signals` 表 | 暂停或进程重启后恢复仍有效且属于当前目标的信号；继续监控后重新调查 |
| 服务运行日志 | `runtime/liveguard.log`，达到 10 MiB 后滚动到 `.1`；可用 `LIVEGUARD_LOG_PATH` 改路径 | 保留在本机；容器/服务部署需自行采集或备份 |
| 正在运行的模型请求、模型轮次和工具调用位置、在途取消、reviewSlots、节流时间 | Service 与 Agent 运行时内存 | 不恢复；暂停或重启后取消该请求，并从已持久化的信号重新开始一次调查 |
| 调查中查看的截图 | 系统临时目录中的 `liveguard-agent-frame-*.png` | `save_frame_evidence` 只在当前调查中保留图片；正常结束后删除，不归档图片。强制终止后的清理尚未专项验证 |
| Chrome 登录资料 | `runtime/live-profile/` | 浏览器资料仍在本机；这不代表监控任务或队列自动续跑 |
| API Key | 本地 Go 进程内存，或用户提供的环境变量 | UI 设置不写仓库/浏览器；重启需重新配置或从环境读取 |
| 网页提醒已读 | 浏览器 localStorage | 只影响网页展示，不是后端事件幂等依据 |
| 人工导出/评测报告 | `.cache/` | 被 Git 忽略；不是服务自动写出的状态数据库 |

SQLite 是唯一的持久化运行仓储，数据库路径可用 `LIVEGUARD_SQLITE_PATH` 修改。Run 快照、观测记录和新信号的队列登记使用 SQLite 事务；每个 Run 最多保存 8 个待处理信号，单张暂存图片最多 4 MiB、全部待处理图片最多 64 MiB。重启后，仍未过期且 GoalVersion 匹配的排队或处理中信号会回到队列；过期或属于旧目标的信号会丢弃。正在进行的模型请求无法从原轮次续跑，恢复时会以已保存证据重新调查，外部模型调用与本地提交之间仍可能产生重复调用，因此这是至少一次处理，不是 exactly-once。既有 JSON/JSONL 文件不会自动导入，LiveGuard 也不再把它们作为运行仓储打开；需要查看旧文件时可用文本或 JSON 工具手动检查。

生成用于查看和归档的 JSON 副本：`go run ./cmd/monitor-store-export -db runtime/liveguard.sqlite -out runtime/json-backup-2026-10-27`。输出目录必须是新目录，包含 `liveguard.json` 和 `liveguard.json.observations.jsonl`；导出读取同一 SQLite 快照，但不包含 Pending 队列，不是完整备份，也不能重新导入 LiveGuard。运行时只支持 SQLite 持久化和显式选择的临时内存模式；`LIVEGUARD_INFRA_MODE=memory` 下进程退出后 Run 和观测都会丢失。复制 SQLite 数据库作为完整备份时先停止服务，并一并复制存在的 `-wal` / `-shm` 文件。当前调查截图不会作为历史归档保存；队列中等待处理的图片字节则暂存在 SQLite，队列结束后随记录删除。

OCR 先做文本去重，再把新增行写入流水和入队；Agent 复核有独立节流，不会随 OCR 频率每秒调用模型。常规采样图片和调查中选用的截图都使用临时文件，正常处理结束后删除；强制终止后的临时文件清理尚未专项验证。可关联的 OCR 文字和文字结论会写入监控记录。Run 快照中的事件数量有上限，完整观测流水单独追加保存。观测流水没有自动清理策略，磁盘会随时间增长；需要自行备份或清理。

服务启动时 RecoverInterrupted 会把遗留的 `running` Run 标成 `paused` 并保存 `process_interrupted` 原因，同时回收有效 Pending 项。用户点击继续会重建采集与队列，保留同一 Run 的历史，并重新调查恢复的信号；当前工具调用、模型轮次和浏览器执行位置不会恢复。

### 有界历史不等于全局资源有界

| 每个 Run 的上限 | 值 |
|---|---|
| Events | 2000 |
| Investigations / Alerts | 各 500 |
| 单调查证据 / 单提醒证据 | 各 100 |
| 单调查步骤 | 64 |
| EventReceipts | 2048 且最长 24 小时 |

裁剪后 HistoryTruncated=true；Summary 的 Events/Alerts 等数字主要反映仍保留的记录，不是永久累计统计。导出 token 总和也只覆盖保留的调查，而且未包含目标规划、ASR及不可见失败请求的全部成本。

MemoryRepository 的 Run map 没有已停止会话数量/过期清理限制；连续新建停止仍会增长。文件模式每次更新都会对整个 Run 做 JSON 深拷贝并原子写入快照；高频更新会增加 CPU 和磁盘写入。不能凭“单 Run 有上限”宣称支持无限运行。

## 故障与恢复

RunStatus、SourceHealth、Investigation.Status 是三个维度。Run 可处于 running、paused、stopped、failed 或 completed；paused 表示用户暂停整个采集/调查运行时。例如 Run 仍然 running 但 SourceHealth=capture_failed，意味着会话未被用户停止，采集却不能继续有效工作；必须查看健康和 LastError，不能只看 running。

| 场景 | 当前反馈与行为 | 当前限制 |
|---|---|---|
| 登录/验证码提示 | needs_login，关闭输入队列；用户处理页面后暂停再继续以重新核验 | 不自动登录/绕过验证，文字检测仍可能误判 |
| 截图/OCR失败 | 截图与 OCR 分阶段计数；delayed 状态指数退避，最长 30 秒后继续尝试，成功后自动恢复；状态写入失败记录独立日志并重试 | 明确登录/验证仍需用户处理；旧目标结果不能复活旧运行时。OCR 积压只替换等待画面，不取消正在识别的画面，不算源故障 |
| OCR 文字过长 | 内部 OCR 按最多 4000 字符拆成多条可引用观测，保留全部识别文字和来源 | 外部输入接口仍校验单条长度；拆分不会提高 OCR 识别准确率 |
| 无文字/无语音 | ErrNoText 可进入无文字视觉检查路径；ErrNoSpeech 是正常无转写结果 | 截图成功只说明可截图，不证明直播仍在播放、评论持续刷新 |
| 模型限流/网络问题 | 普通限流或网络问题使用有限重试和熔断；失败留下failed调查，继续收新信号；明确额度/余额不可用则结束当前配置下的活跃监控并显示原因 | 单次传输可能已进行一次有限重试；提供方可能仍对重试请求计费；未知错误不猜成额度耗尽 |
| 模型格式错误 | 共享 Agent 最多一次格式纠正，消耗原轮数预算；再次无效停止调查 | 不是无限重试；失败不切换成关键词提醒 |
| 工具错误/超时/预算耗尽 | 记录轨迹、成功状态、耗时与 StopReason；不完整调查不提交正常提醒 | context 超时要求适配器响应取消，不能强杀任意忽略 context 的 Go 代码 |
| 队列满/调查任务过期 | Pending 展示丢弃/过期/合并数量和原因；SQLite 保存未完成项，过期后删除队列项但保留观测；拒绝接收可重试原 ID | 接收回执不是完成回执；完成或失败后的每事件状态不是永久任务账本 |
| 改目标/暂停/停止 | 改目标递增 GoalVersion；暂停取消旧工作但保留 Run；停止终结 Run；提交检查防迟到结果生效 | Resume 重建运行时；浏览器音频需要用户重新授权 |
| 进程退出/重启 | 下次启动时活跃监控转为 paused；SQLite 中仍有效的 Pending 信号重新入队；记录退出由本机 API 还是操作系统信号触发 | 用户点击继续后重新调查信号；不会续接原模型轮次或旧浏览器操作。内存模式的状态会丢失 |
| 网页断开 | 已接受的排队任务使用监控 context，不随 HTTP 客户端断开取消；SSE 是更新提示，网页重新拉取快照 | SSE 不是持久消息总线或外部送达确认 |

配置入口为三个提供方统一设置的熔断阈值为连续 2 次模型请求错误、冷却 30 秒；之后只允许一个恢复探测。Agent 总时限或信号到期不计作模型错误，因此不会仅因调查预算用完就触发熔断。单次模型请求最多等待 60 秒，整个 Agent 调查最多 90 秒；信号仍受自己的有效期约束，过期后不会继续提交提醒。共享 DoModelRequest 对可重放的请求，仅在有限网络错误、408/429/500/502/503/504 上重试，通常等 250ms；Retry-After 超过 2 秒不自动等到重试。401/403 不进行无意义重试。模型可能对两次请求都计费。请求日志记录 `review_id`（触发信号 ID）、`attempt_id`（每次调查尝试的唯一 ID）、轮次、HTTP 状态、重试次数、请求耗时、可用上下文预算和超时来源；Agent trace 还记录等全局名额、模型轮次开始、工具开始/结束和调查结束。工具日志不包含原始参数、OCR 文本或模型提示词。计划阶段的请求也使用同一传输机制；格式纠正与传输重试分别计数。

排查 Agent 当前进度时，在 PowerShell 运行 `Select-String -Path .\runtime\liveguard.log -Pattern 'attempt_id=' | Select-Object -Last 100`。按同一个 `attempt_id` 查看完整尝试：`agent review waiting for slot` 表示在等调查名额/冷却，`agent review slot acquired` 表示已获准，`agent model turn started` 表示模型请求刚开始，`model request completed/failed` 表示请求返回，`agent tool started/completed` 表示 Go 工具执行，最后看 `agent review finished` 和 `unexpected livestream review completed/incomplete/failed`。只有开始行而暂时没有结束行，说明该阶段仍在执行或进程尚未写出结束结果。旧进程不会产生新增格式，重启到新版本后生效。

排查请求日志时，`review_id` 是信号 ID，不能唯一标识调查尝试；同一信号再次调查时 `turn` 会重新从 1 开始，应以 `attempt_id` 分组。`context_budget_ms` 表示该请求开始时其 context deadline 的剩余**毫秒数**，不是 token 预算；`client_timeout_ms` 是 HTTP 客户端超时。`input_tokens` / `output_tokens` 是单次模型请求的用量；`unexpected livestream review completed/incomplete` 汇总一次调查的多轮用量。日志中的 `context canceled` 要结合 `LiveGuard shutdown requested`、暂停/停止记录和 `timeout_source` 判断；如果它紧跟进程中断，通常表示请求被运行生命周期取消，不应误判为 token 超限。`model response truncated at token limit` 则是单次响应触及输出限制；`turn_budget` / `call_budget` 表示调查循环用尽轮数或调用数。当前没有每 Run 或每日累计 token 硬预算，单轮输出限制和调查轮数限制见[Agent Harness 详解](AGENT-HARNESS.md)。

采集恢复在代码中区分“可重试delayed”与“真正paused”，入队只拦截needs_login/capture_failed；成功观测先恢复输入健康，队列满或图片预算拒绝不计为采集失败。明确额度错误也有用户可见的结束原因。未解决的问题与候选改进方向见[优化路线图](ROADMAP.md)。

## 本地运行与接口

### 快速启动

当前提供 Windows 脚本。需要 Windows 10/11、Go 1.25、Google Chrome、Tesseract；中文直播建议安装 chi_sim 数据。ASR 若要处理浏览器音频，需可用 FFmpeg 及 MiniMax 配置。macOS/Linux 尚无完整安装与真实浏览器验收说明。

~~~powershell
git clone https://github.com/sdaAlbert/liveguard.git
cd liveguard
go version
$env:LIVEGUARD_INFRA_MODE = "sqlite"
powershell -ExecutionPolicy Bypass -File ./scripts/run-mvp.ps1
~~~

启动脚本会显示实际 SQLite 文件位置，默认是 `runtime/liveguard.sqlite`；日志也会出现 `SQLite monitor state persisted to ...`。验证 Pending 恢复：创建一个真实直播监控，等 Agent 正在调查时让新的 OCR 信号进入待处理队列，然后在信号观察时间起 2 分钟内按 Ctrl+C，等待服务退出，再用同一命令启动并点击该 Run 的“继续”。Run 应先显示已暂停，继续后 Agent 会重新调查中断信号。图片信号只有 30 秒有效期，需更快重启。模型正在执行的轮次不会续接；看到新的调查记录/模型请求表示恢复后重新执行。不要切换 SQLite 路径，否则会打开另一个数据库。

打开 http://127.0.0.1:8080 ，先打开专用 Chrome 登录，再新建直播监控。在右上角“模型设置”配置提供方、API Key、模型和可选地址。保存设置不发送模型请求；设置仅向本机 Go 服务发送密钥，不放入浏览器存储或仓库。留空 Key 可复用同一提供方当前进程密钥。重启需重新填写或读环境。

模型配置影响后续调查；目标计划在新建/改目标时重新生成，其能力投影会按当前模型更新。MiniMax-M3、GPT-6 Luna、GPT-6 Sol、DeepSeek Flash 默认声明支持工具调用和图片输入；自定义模型需按实际能力设置图片理解，服务不会通过探测请求验证该声明。OpenAI 的 GPT-6 Luna 和 GPT-6 Sol 经 Chat Completions 调用时使用 `reasoning_effort: none`。三家模型共用规划与调查策略，线上请求格式由各自协议适配器转换。切换“本地规则”停用模型规划和判断，已单独配置的 MiniMax ASR 不因此关闭。当前 UI Key 配置也不自动建立 ASR，ASR主要由启动环境配置决定。

可在启动前设置 MINIMAX_API_KEY / OPENAI_API_KEY / DEEPSEEK_API_KEY 和 LIVEGUARD_MONITOR_PROVIDER。脚本默认 auto 并尝试复用本机 OpenClaw MiniMax 配置；直接运行 Go 默认 deterministic，想用环境中的模型还需 LIVEGUARD_MONITOR_LLM_MODE=auto。

Tesseract 非默认安装位置时：

~~~powershell
$env:LIVEGUARD_TESSERACT_BIN = "C:\path\to\tesseract.exe"
$env:LIVEGUARD_OCR_LANGUAGE = "chi_sim+eng"
~~~

代理不是必需项，按本机实际代理设置 HTTPS_PROXY。程序与启动脚本尊重已有代理环境，不自动将用户端口改写为某台机器上的固定端口。

### 常用配置

| 配置 | 用途 |
|---|---|
| LIVEGUARD_ADDR | 默认 127.0.0.1:8080 |
| LIVEGUARD_MONITOR_LLM_MODE | 直接 Go 启动默认 deterministic；auto 启用提供方配置 |
| LIVEGUARD_MONITOR_PROVIDER / MODEL / ENDPOINT | 模型提供方、模型与地址；默认值是仓库配置，不代表服务商最新推荐 |
| LIVEGUARD_MONITOR_SUPPORTS_VISION | 自定义模型图片能力的显式声明；不发送探测请求 |
| LIVEGUARD_MONITOR_REVIEW_INTERVAL | 全局调查启动间隔下限，默认 15s |
| LIVEGUARD_LIVE_CAPTURE_INTERVAL | 可选全局采集间隔下限；未设置时本地 OCR 目标间隔为 1s |
| LIVEGUARD_LIVE_CAPTURE_WARMUP | 新页面预热，默认 7s |
| LIVEGUARD_TESSERACT_BIN / OCR_LANGUAGE | OCR 路径和语言 |
| LIVEGUARD_ASR_MODE / MODEL / ENDPOINT / FFMPEG_BIN | ASR 启动配置；off 关闭；分享音频仍需用户主动授权 |
| LIVEGUARD_ALLOWED_HOSTS | 可选的专用浏览器主机白名单，逗号分隔；未设置时接受任意有效 HTTP(S) 目标。服务如暴露给其他用户，应配置白名单 |
| LIVEGUARD_INFRA_MODE | `sqlite`（默认，本机持久化）或 `memory`（临时进程内存，退出丢失） |
| LIVEGUARD_SQLITE_PATH | SQLite 数据库位置，默认 `runtime/liveguard.sqlite` |
| LIVEGUARD_FRAME_ARCHIVE_DIR | 已有归档截图的读取目录，默认 `runtime/frames`（Git 忽略）；当前调查不会向该目录写入新截图 |
| LIVEGUARD_LOG_PATH | 持久化服务日志位置，默认 `runtime/liveguard.log`，10 MiB 滚动保留一个旧文件 |
| LIVEGUARD_OTEL_ENDPOINT | 可选 OTLP 导出器；当前主要依靠 `runtime/liveguard.log` 中的结构化模型/Agent 过程记录，尚未为 Agent 调查建立完整 OTel span |

配置示例见 [.env.example](../.env.example)。不要把密钥写进公开文档。

### HTTP 接口

| 接口 | 用途 |
|---|---|
| POST /api/monitors | 创建：target_url、goal、source_mode；返回202 |
| GET /api/monitors | 会话摘要 |
| GET /api/monitors/{id} | 会话详情、证据、调查、提醒和 Pending |
| PUT /api/monitors/{id}/goal | 更新关注目标，递增 GoalVersion |
| POST /api/monitors/{id}/instruction | 保存给 Agent 的通用指令；暂停时提交会继续监控，以“目标：”开头时更新关注目标 |
| POST /api/monitors/{id}/pause | 暂停采集与调查，保留此监控卡片及流水 |
| POST /api/monitors/{id}/resume | 从暂停状态继续同一监控；运行槽位已满时返回 429 |
| POST /api/monitors/{id}/stop | 停止 |
| POST /api/monitors/{id}/signals | 文字信号，支持 ID 幂等 |
| POST /api/monitors/{id}/ocr | multipart 图片识别并提交 |
| POST /api/monitors/{id}/audio | 用户授权的音频片段识别并提交 |
| GET /api/events | SSE 更新提示；详情仍读快照 |
| GET/POST /api/browser/session | 查询/打开专用浏览器窗口，不代表已登录成功 |
| GET/PUT /api/settings/model | 本机模型设置 |

source_mode 支持 browser_ocr（真实截图）、ocr_upload（手工输入/上传）、demo_replay（合成演示）。缺省模式在 Go Service 中是 demo_replay，不能因演示流走通就判断真实采集走通。

## 项目结构与术语

| 目录 | 职责 |
|---|---|
| cmd/liveguard | 本地服务入口、配置、仓储与采集适配器装配 |
| cmd/monitor-eval、cmd/monitor-load | 真实文字模型评测、合成后端负载 |
| internal/agent | 通用有界循环、HTTP、重试与熔断 |
| internal/monitor | 生命周期、领域类型、队列、感知、计划、语义调查、观测检索、仓储与评测 |
| internal/browser | 专用 Chrome 会话和 CDP 截图 |
| internal/web | 网页、HTTP 接口和 SSE |
| internal/telemetry | 可选 OTLP 初始化 |
| scripts | 启动、显式付费验证、验收导出 |
| docs/ROADMAP.md | 已知问题、候选优化方向与验收证据 |

Monitor Run 是持续监控会话；Signal 是一次观察；Evidence 是可关联来源和时间的支持/反驳信息；Investigation 是有限调查；Pending 是尚未完成的执行工作；Memory 是有限历史解释；Alert 是后端提醒记录。接收、确认、提醒、网页已读、外部送达应分别表述。

## 安全边界

LiveGuard 是本地参考项目，默认只监听本机，没有用户认证与 TLS；不能作为已加固的公网服务部署。浏览器使用独立资料目录，不要分享登录资料和私人截图。站点限制尚未完整覆盖重定向、DNS 重绑定与 SSRF。网页信号不应被当作系统指令，模型工具由服务器白名单控制。

仓库忽略 .env.local、runtime/、artifacts/、state/ 和 .cache/。API Key 保存在本地环境或进程内存；不要打印或提交密钥。安全问题通过 GitHub 私密漏洞报告提交，不公开凭据、私人 URL 或用户数据。
