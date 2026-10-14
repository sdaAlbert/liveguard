# 运行、配置与故障处理

第一次接触项目可先读[项目导览](PROJECT-GUIDE.md)，了解 Run、观测、调查和提醒分别是什么；本篇聚焦如何启动、配置、排障和备份。

## 运行时长与API额度

**LiveGuard是持续运行的监控。** Run使用context.WithCancel，没有总时长deadline、自动结束计时器或按StartedAt计算的十分钟上限。模型API可用、直播输入有效且服务保持运行时，会持续收集新信号，调查上下文会反复重建；不是让同一次模型请求一直运行。

- 用户点击停止、服务退出或明确的模型API额度/余额耗尽，会结束监控。
- 普通网络异常、瞬时限流、一次调查超时，不结束整个会话；按有限重试/熔断继续监听。
- 登录/验证码，或连续三次实际采集失败，会明确暂停输入并提示用户处理；没有假装仍在有效观察。
- API额度错误在共享适配层识别为ErrModelQuotaExhausted；当前配置的共享Reviewer记录不可用状态，停止该配置下的活跃窗口，取消采集和队列、释放窗口，并保留结束原因。
- Run.Status=failed、stop_reason=api_quota_exhausted、last_error说明额度原因；模型设置返回quota_exhausted，网页显示“API额度已耗尽”。普通HTTP429本身不会被认定为余额耗尽。
- 恢复额度/配额后，重新保存模型配置并新建监控。保存重置当前绑定的不可用状态，不会自动恢复已经结束的窗口，也不会在保存时测试Key。
- 不主动查询余额，也不提前按估算金额停止。只有实际模型请求返回明确的额度/余额不可用信息时触发终止；没有新信号时也不会为了耗尽API而空调用。

### 时间限制分别约束什么？

| 时间 / 限制 | 作用对象 | 是否结束监控会话 |
|---|---|---|
| 会话总时长 | Run | **无固定上限** |
| 单条观测/队列有效期 | 默认文字从观察时刻起 2 分钟，frame 30 秒；明确倒计时、相对未来时间或 valid_until 可覆盖默认值 | 仅停止过期调查；Run 和已入库的历史继续保留 |
| 35秒调查 / 12秒工具 / 25秒HTTP请求 | 单次调查、工具和模型请求 | 本次执行停止或失败，不是整个Run结束 |
| 15分钟评测程序期限 | cmd/monitor-eval离线评测 | 仅限制评测命令，与真实监控运行时长无关 |

源码入口：[Run创建与采集恢复](../internal/monitor/service.go)、[额度终止](../internal/monitor/quota.go)、[提供方错误分类](../internal/monitor/model_errors.go)。额度与限流按提供方错误元数据区分，不能根据直播文字中的“余额不足”触发：参考[OpenAI错误码](https://developers.openai.com/api/docs/guides/error-codes)、[MiniMax错误码](https://platform.minimax.io/docs/api-reference/errorcode)、[DeepSeek错误码](https://api-docs.deepseek.com/quick_start/error_codes/)。


## 持久化与长期运行

### 信息究竟存在哪里？

| 信息 | 默认位置 | durable 模式 / 退出后 |
|---|---|---|
| Goal/Plan/Sampling、有限事件/调查/提醒快照、回执 | `runtime/liveguard.json` 原子 JSON 快照 | Postgres 的 `live_monitor_runs.payload` JSONB；完整观测在 `live_monitor_observations` |
| 完整观测流水 | `runtime/liveguard.json.observations.jsonl` 追加日志；旧 JSON 快照中的观测会首次启动时迁入 | `live_monitor_observations` 追加记录 |
| 队列、在途取消、reviewSlots、节流时间、图片字节预算 | Service 与 signalQueue 内存 | 不持久化；没有可恢复执行检查点 |
| 观测关联截图 | `runtime/frames/<run哈希>/<归档ID>.png`，已被 Git 忽略；归档 ID 由 Run ID 和观测 ID 生成 | OCR 采样图识别后删除；Agent 看图时先用临时图，只有调用 `save_frame_evidence` 才归档该图及其 OCR 关联 |
| Chrome 登录资料 | runtime/live-profile | 浏览器文件可继续存在；不等于监控任务恢复 |
| API Key | 本地 Go 进程内存，或用户已有环境配置 | UI 设置不写仓库/浏览器；重启重新配置或读取环境 |
| 网页提醒已读 | 浏览器 localStorage，数量有界 | 只影响网页展示，不能作为后端事件幂等依据 |
| 人工导出/评测报告 | .cache | 被 Git 忽略；不是服务自动写出的状态数据库 |

OCR 先做精确文本去重，再将新 OCR 写入流水和入队。常规采样图片不会落盘；Agent 保存图片证据时，同一归档 ID 会关联图片、画面观测和 OCR 观测，方便前端与后续调查核对。成功调查产生的判断和工具步骤也进入 Run 快照。PostgreSQL 启动时会把旧 Run 快照里尚保留的历史迁入流水；新观测不再受快照 2,000 条上限影响。本地模式把完整观测放在追加式 JSONL sidecar，避免每条新观测都重写完整历史；`runtime/liveguard.json` 和 sidecar 应一起备份。按用户要求，观测不自动清理；选中保存的截图也会持续占用磁盘，需自行备份或手动清理。

state/、artifacts/ 目录存在不意味着当前监控状态存在那里。artifacts/ 仍有本地静态文件服务入口；监控与流水持久化入口是 Repository。

### 如何启用 PostgreSQL

启动脚本默认使用 local 内存模式；设置 `LIVEGUARD_INFRA_MODE=durable` 后会保留该配置，并连接 `LIVEGUARD_DATABASE_URL` 指定的 PostgreSQL（未设置时默认 `127.0.0.1:5433/liveguard`）：

~~~powershell
$env:LIVEGUARD_INFRA_MODE = "durable"
.\scripts\run-mvp.ps1
~~~

示例用 `5432` 或其他端口时，需同时设置 `LIVEGUARD_DATABASE_URL`。经 Agent 选择保存的截图保存在 `LIVEGUARD_FRAME_ARCHIVE_DIR`（默认 `runtime/frames`），durable 备份要同时保留数据库和该目录。

服务启动时 RecoverInterrupted 使用专门的 ListRunning 查询全部遗留 running，不受终态展示的 50 条限制；将它们标成 paused，保存 process_interrupted 原因。所有 running/paused 卡片都会返回到前端，用户点击继续会重建采集与队列，保留同一 Run 的历史。不会自动重放未完成调查或恢复浏览器执行检查点；`LIVEGUARD_INFRA_MODE=memory` 才是仅进程内状态，默认 local JSON 和 durable PostgreSQL 都会保留 Run。

### 有界历史不等于全局资源有界

| 每个 Run 的上限 | 值 |
|---|---|
| Events | 2000 |
| Investigations / Alerts | 各 500 |
| 单调查证据 / 单提醒证据 | 各 100 |
| 单调查步骤 | 64 |
| EventReceipts | 2048 且最长 24 小时 |

裁剪后 HistoryTruncated=true；Summary 的 Events/Alerts 等数字主要反映仍保留的记录，不是永久累计统计。导出 token 总和也只覆盖保留的调查，而且未包含目标规划、ASR及不可见失败请求的全部成本。

MemoryRepository 的 Run map 没有已停止会话数量/过期清理限制；连续新建停止仍会增长。每次更新会对整个 Run 做 JSON 深拷贝，PostgreSQL 会整体更新 JSONB；长运行高吞吐会增加 CPU、锁等待和写放大。不能凭“单 Run 有上限”宣称支持无限运行。

下一步顺序：先限制终态 Run 留存并增加分页/删除；再把事件与调查改成追加记录，热状态只存有限索引；归档原始截图并建立引用；最后补任务恢复检查点和通知 outbox。不要一开始就同时引入 Redis、Kafka 与分布式调度。

## 故障与恢复

RunStatus、SourceHealth、Investigation.Status 是三个维度。Run 可处于 running、paused、stopped、failed 或 completed；paused 表示用户暂停整个采集/调查运行时。例如 Run 仍然 running 但 SourceHealth=capture_failed，意味着会话未被用户停止，采集却不能继续有效工作；必须查看健康和 LastError，不能只看 running。

| 场景 | 当前反馈与行为 | 当前限制 |
|---|---|---|
| 登录/验证码提示 | needs_login，关闭输入队列；用户处理页面后暂停再继续以重新核验 | 不自动登录/绕过验证，文字检测仍可能误判 |
| 截图/OCR失败 | 截图与 OCR 分阶段计数；前两次 delayed，连续三次才暂停；当前目标的成功 OCR 先恢复健康再提交 | 处理页面后暂停再继续；旧目标结果不能复活旧运行时。OCR 积压会记录跳过帧数，不算采集失败 |
| 无文字/无语音 | ErrNoText 可进入无文字视觉检查路径；ErrNoSpeech 是正常无转写结果 | 截图成功只说明可截图，不证明直播仍在播放、评论持续刷新 |
| 模型限流/网络问题 | 普通限流或网络问题使用有限重试和熔断；失败留下failed调查，继续收新信号；明确额度/余额不可用则结束当前配置下的活跃监控并显示原因 | 单次传输可能已进行一次有限重试；提供方可能仍对重试请求计费；未知错误不猜成额度耗尽 |
| 模型格式错误 | 共享 Agent 最多一次格式纠正，消耗原轮数预算；再次无效停止调查 | 不是无限重试；失败不切换成关键词提醒 |
| 工具错误/超时/预算耗尽 | 记录轨迹、成功状态、耗时与 StopReason；不完整调查不提交正常提醒 | context 超时要求适配器响应取消，不能强杀任意忽略 context 的 Go 代码 |
| 队列满/调查任务过期 | Pending 展示丢弃/过期/合并数量和原因；临时图片清理、已入库观测保留；拒绝接收可重试原 ID | 接收回执不是完成回执；尚未提供每事件完整持久处理状态 |
| 改目标/暂停/停止 | 改目标递增 GoalVersion；暂停取消旧工作但保留 Run；停止终结 Run；提交检查防迟到结果生效 | Resume 重建运行时；浏览器音频需要用户重新授权 |
| 进程退出/重启 | 默认内存状态丢失；durable 扫描全部遗留 running 并标为 paused，所有暂停卡片可点击继续 | 重建的是新的采集运行时；不自动续接旧浏览器操作或重放旧队列 |
| 网页断开 | 已接受的排队任务使用监控 context，不随 HTTP 客户端断开取消；SSE 是更新提示，网页重新拉取快照 | SSE 不是持久消息总线或外部送达确认 |

配置入口为三个提供方统一设置的熔断阈值为连续 2 次失败、冷却 30 秒；之后只允许一个恢复探测。共享 DoModelRequest 对可重放的请求，仅在有限网络错误、408/429/500/502/503/504 上重试，通常等 250ms；Retry-After 超过 2 秒不自动等到重试。401/403 不进行无意义重试。模型可能对两次请求都计费。计划阶段的请求也使用该传输机制；格式纠正与传输重试分别计数。

采集恢复在代码中区分“可重试delayed”与“真正paused”，入队只拦截needs_login/capture_failed；成功观测先恢复输入健康，队列满或图片预算拒绝不计为采集失败。明确额度错误也有用户可见的结束原因。完整审视及其他改进优先级见[代码审视与清理说明](PROJECT-REVIEW.md)。

## 本地运行与接口

### 快速启动

当前提供 Windows 脚本。需要 Windows 10/11、Go 1.25、Google Chrome、Tesseract；中文直播建议安装 chi_sim 数据。ASR 若要处理浏览器音频，需可用 FFmpeg 及 MiniMax 配置。macOS/Linux 尚无完整安装与真实浏览器验收说明。

~~~powershell
git clone https://github.com/sdaAlbert/liveguard.git
cd liveguard
go version
powershell -ExecutionPolicy Bypass -File ./scripts/run-mvp.ps1
~~~

打开 http://127.0.0.1:8080 ，先打开专用 Chrome 登录，再新建直播监控。在右上角“模型设置”配置提供方、API Key、模型和可选地址。保存设置不发送模型请求；设置仅向本机 Go 服务发送密钥，不放入浏览器存储或仓库。留空 Key 可复用同一提供方当前进程密钥。重启需重新填写或读环境。

模型配置影响后续调查；目标计划在新建/改目标时重新生成，其能力投影会按当前模型更新。MiniMax-M3、GPT-4.1 mini、DeepSeek Flash 默认声明支持工具调用和图片输入；自定义模型需显式按实际能力设置图片理解，服务不会通过探测请求验证该声明。三家模型共用规划与调查策略，线上请求格式由各自协议适配器转换。切换“本地规则”停用模型规划和判断，已单独配置的 MiniMax ASR 不因此关闭。当前 UI Key 配置也不自动建立 ASR，ASR主要由启动环境配置决定。

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
| LIVEGUARD_LIVE_CAPTURE_INTERVAL | 可选全局采集间隔下限 |
| LIVEGUARD_LIVE_CAPTURE_WARMUP | 新页面预热，默认 7s |
| LIVEGUARD_TESSERACT_BIN / OCR_LANGUAGE | OCR 路径和语言 |
| LIVEGUARD_ASR_MODE / MODEL / ENDPOINT / FFMPEG_BIN | ASR 启动配置；off 关闭；分享音频仍需用户主动授权 |
| LIVEGUARD_ALLOWED_HOSTS | 专用浏览器允许的主机；默认含抖音及本机 |
| LIVEGUARD_INFRA_MODE / DATABASE_URL | local 内存，durable 使用外部 PostgreSQL |
| LIVEGUARD_FRAME_ARCHIVE_DIR | 观测截图归档目录，默认 `runtime/frames`（Git 忽略）；durable 部署应与数据库一起备份该目录 |
| LIVEGUARD_OTEL_ENDPOINT | 可选外部 OTLP；当前主要是 HTTP 请求 span，未覆盖完整后台调查链路 |

配置示例见 [.env.example](../.env.example)。不要把密钥写进公开文档。

### HTTP 接口

| 接口 | 用途 |
|---|---|
| POST /api/monitors | 创建：target_url、goal、source_mode；返回202 |
| GET /api/monitors | 会话摘要 |
| GET /api/monitors/{id} | 会话详情、证据、调查、提醒和 Pending |
| PUT /api/monitors/{id}/goal | 更新关注目标，递增 GoalVersion |
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
| docs/PROJECT-REVIEW.md | 本次清理依据、遗留问题与下一步优先级 |

Monitor Run 是持续监控会话；Signal 是一次观察；Evidence 是可关联来源和时间的支持/反驳信息；Investigation 是有限调查；Pending 是尚未完成的执行工作；Memory 是有限历史解释；Alert 是后端提醒记录。接收、确认、提醒、网页已读、外部送达应分别表述。

## 安全边界

LiveGuard 是本地参考项目，默认只监听本机，没有用户认证与 TLS；不能作为已加固的公网服务部署。浏览器使用独立资料目录，不要分享登录资料和私人截图。站点限制尚未完整覆盖重定向、DNS 重绑定与 SSRF。网页信号不应被当作系统指令，模型工具由服务器白名单控制。

仓库忽略 .env.local、runtime/、artifacts/、state/ 和 .cache/。API Key 保存在本地环境或进程内存；不要打印或提交密钥。安全问题通过 GitHub 私密漏洞报告提交，不公开凭据、私人 URL 或用户数据。
