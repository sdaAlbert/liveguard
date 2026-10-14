# 架构与 Agent 行为

第一次阅读项目时，建议先看[项目导览](PROJECT-GUIDE.md)：它用一次监控的完整路线解释目录、状态和术语。本篇作为第二层文档，深入讲每个模块如何协作及其限制。

## 六个难点：当前怎么做、还缺什么

| 难点 | 当前实现 | 仍然存在的限制 / 下一步 |
|---|---|---|
| 信号覆盖 | 截图独立定时采集；临时帧进入 1 个 OCR worker + 2 帧等待队列，OCR 后删除；聊天目标最多间隔 1 秒；音频录制与最多 2 个上传/ASR 请求并行 | 截图仍不是完整评论流；OCR 积压会跳过新帧并累计计数；只有 Agent 调用 `save_frame_evidence` 才归档图片 |
| 证据与语义判断 | 保存 OCR 行框坐标、图片引用、目标版本、有效期和显式引用 ID；调查冻结检索时间上界，提交不再读取新的“最新六条”替代引用；聊天文字和所看原图必须匹配 | 坐标不自动等于聊天/字幕分类；引用、反讽和复杂否定仍需模型判断；整场检索是字面匹配 |
| 调度与时效 | 每窗口 1 条调查、全进程 4 个槽位；就绪窗口按 FIFO 公平授予；窗口内使用小幅优先级、每 30 秒老化和 deadline；重复文字及明确数值更新可合并，取消/更正可中断较低优先级旧调查 | 每窗口仍只有 8 条等待；不承诺任意高峰无损，也不对任意改写做语义事件合并 |
| 成本控制 | 本地 OCR 行去重；文字无新增通常不调用判断模型；调查节流；每次最多 4 轮/8 工具调用/1 次成功看图；稳定降频与候选加速受 Go 校验 | 变化检测发生在 OCR 之后，没有图像差异门控；没有每日金额/token 硬上限；周期视觉仍会花费；不能声称“无变化零成本” |
| 跨调查历史 | 观测持续落库；Agent 按目标搜索近期或整场记录，并按需查询已提醒信息 | 没有独立摘要 TTL 或事件状态账本；没有长期日程、定时唤醒、向量检索；提醒查询目前仅覆盖近期 |
| 持续运行可靠性 | Run/输入健康/调查状态分开；监控卡片可暂停/继续；登录或验证提示暂停输入；额度耗尽明确结束；PostgreSQL 追加观测流水；已暂停 Run 可在重启后继续 | 登录检测是文字启发式；连续采集失败需用户恢复；流水和截图没有自动清理策略；没有消息送达回执和全天运行 SLO |

核心取舍是：**先保障观察和证据，再改善判断。** 模型只能解释实际收到的信号，无法追回采样间隔中从未采到的评论。

## 完整数据流与模块职责

~~~mermaid
flowchart TD
    U[用户：URL + Goal] --> GP[目标规划 GoalPlanner]
    GP --> N[Go 归一化计划与权限]
    N --> RUN[Run：GoalVersion + Sampling]
    RUN --> C[专用 Chrome / CDP 截图]
    C --> FR[临时截图供 OCR 使用]
    FR --> OCR[有界队列中的本地 Tesseract OCR]
    OCR --> ND[OCR 行精确去重]
    A[用户授权网页音频分享] --> ASR[短片段 ASR]
    ASR --> IN[SubmitSignal：校验 + 回执 + 有界入队]
    ND --> LEDGER[追加观测流水：新 OCR/ASR/截图引用]
    LEDGER --> IN
    ASR --> IN
    C --> FT[视觉计划：无新文字时定时 frame 信号]
    FT --> IN
    IN --> Q[每窗口队列：优先级老化 / 逐条 deadline / 有限合并]
    Q --> G[单窗口互斥 + 公平授予全进程 4 个槽位]
    G --> CTX[当前触发观测 + 目标 / 计划 / 当前时间]
    CTX --> R[共享多轮 ReAct]
    R --> TOOLS[按目标搜近期/整场观测 / 查提醒 / 按需看当前或归档图]
    TOOLS --> R
    R --> D[alert / wait / ignore + 可选采样建议]
    D --> COMMIT[Go 原子提交：取消 / Run 状态 / GoalVersion / 执行期限]
    COMMIT --> REPO[Run 快照 + 完整观测流水]
    REPO --> UI[HTTP + SSE 更新本地监控室]
~~~

一次监控会话会持续很久，一次调查只处理有限信号。一次调查内部可以有多轮模型请求；它结束后，下一次调查重新构建短上下文，不继承整段聊天历史。

### 是否拆成“观测 Agent、OCR Agent、意图 Agent”？

**当前建议：解耦模块和执行节奏，保留一个负责调查的 Agent。** 截图与 OCR 是确定性的采集工作，不需要各自运行自主 ReAct。把它们都叫 Agent 会掩盖实际职责，额外引入模型调用、上下文传递和状态一致性问题。

| 模块 | 输入 → 输出 | 当前职责与代码 |
|---|---|---|
| 目标规划 | 用户 Goal → MonitoringPlan | 新建/改目标时执行；模型只提出计划，Go 归一化间隔、感知方式和权限。[goal_plan.go](../internal/monitor/goal_plan.go)、[agent_goal_planner.go](../internal/monitor/agent_goal_planner.go) |
| 观测 | URL/窗口 → 截图 | 复用专用 Chrome 登录资料和每个窗口的页面；不判断事件。[live_capture.go](../internal/browser/live_capture.go) |
| 感知 | 图片/音频 → SignalEvent | Tesseract 与 ASR 适配器，不携带用户长期语义状态。[perception.go](../internal/monitor/perception.go)、[minimax_transcriber.go](../internal/monitor/minimax_transcriber.go) |
| 信号接收和调度 | SignalEvent → 待调查任务 | 校验、幂等、暂存截图、背压、TTL、取消与并发限制。[pending.go](../internal/monitor/pending.go)、[service.go](../internal/monitor/service.go) |
| 语义调查 | 目标 + 触发观测 + 按需查询结果 → 决策 | 模型理解金额、时态、排除条件、引用与否定；受统一工具前置条件约束。[agent_reviewer.go](../internal/monitor/agent_reviewer.go)、[review_tools.go](../internal/monitor/review_tools.go) |
| 全程记录与查询 | OCR/ASR/frame → 观测流水 | 新观测落仓储；Agent 按目标搜索近期或整场记录并分页，模型只收到有限查询结果。[ledger.go](../internal/monitor/ledger.go)、[postgres.go](../internal/monitor/postgres.go) |
| Harness | 模型轮次 + 工具请求 → 有限执行结果 | 工具白名单、参数大小、重复调用、总预算、超时和轨迹；不包含直播领域规则。[loop.go](../internal/agent/loop.go) |
| 状态提交 | 决策 → 更新 Run | 重新检查当前状态与目标版本；模型 I/O 不放在仓储事务中。[repository.go](../internal/monitor/repository.go)、[service.go](../internal/monitor/service.go) |

TextRecognizer、Transcriber、FrameCapturer、GoalPlanner、UnexpectedEventReviewer、Repository 是实际替换和测试位置。无需为了未来可能增加的功能先铺一层没有调用者的通用“多 Agent 框架”。

下一步如果 OCR 明显拖慢采集，可将截图与 OCR 拆成独立 worker：截图进入有界环形缓冲，OCR 使用明确的丢帧和容量策略。要同时保留图片、捕获时间和区域；不能只保留最新帧后声称完整覆盖评论。这是执行解耦，不要求再建一个语言模型 Agent。

### 模型配置与实际使用解耦

~~~mermaid
flowchart LR
    S[本地设置：提供方 / 模型 / 地址 / 密钥 / 能力] --> C[ModelConfig：解析配置]
    C --> M[ToolModel 协议接口]
    M --> MM[MessagesModel：MiniMax Messages]
    M --> CC[ChatCompletionsProvider：OpenAI / DeepSeek]
    GP[共享 AgentGoalPlanner] --> M
    AR[共享 AgentReviewer + Harness] --> M
    AR --> T[统一工具 / 检索上下文 / 证据门槛]
~~~

| 层 | 负责什么 | 不负责什么 |
|---|---|---|
| ModelConfig / ConfigureModel | 解析提供方默认值、复用进程内密钥、声明能力、装配适配器 | 不为不同提供方创建不同调查策略 |
| ToolModel / ModelSession | 统一模型轮次、工具定义、调用和观察结果接口 | 不访问完整监控历史，不决定提醒 |
| MessagesModel | MiniMax 鉴权、工具协议、图片块、原始消息块回传 | 不包含目标/提醒/检索规则 |
| ChatCompletionsProvider | OpenAI兼容鉴权、function调用、tool_call_id结果回传、图片消息转换；保留原始assistant字段 | 不再用单次JSON判断替代ReAct，不序列化全部RecentAlerts |
| AgentGoalPlanner / AgentReviewer | 三个提供方共用规划、调查、格式修正、有限重试、预算及业务工具 | 不直接读取API Key或拼接提供方请求 |

[配置](../internal/monitor/model_config.go)、[协议接口](../internal/monitor/model_session.go)、[共享规划](../internal/monitor/agent_goal_planner.go)、[共享调查](../internal/monitor/agent_reviewer.go) 是主要阅读入口。MiniMaxGoalPlanner / MiniMaxUnexpectedEventReviewer 只保留现有评测与代码调用的兼容入口，内部委托共享实现；服务配置不再按提供方选择不同Agent。

### 统一行为与模型能力

| 模式 | 规划 / 调查 / 检索 | 图片能力 |
|---|---|---|
| MiniMax | 共享结构化规划、多轮工具循环、按需证据查询 | 由配置声明；适配器转换为Messages图片块 |
| OpenAI | 与MiniMax相同的Agent执行规则 | 由配置声明；适配器转换为Chat Completions图片输入 |
| DeepSeek | 与其他提供方相同的Agent执行规则，保留原生推理字段回传 | 默认 deepseek-flash 声明支持图片；自定义模型需按实际能力显式配置 |
| 本地规则 | 固定Go规则，不运行模型循环，不执行模型检索决策 | 不看图 |

必须区分“统一Agent行为”和“模型实际具备同等能力”。所有配置模型必须支持工具调用；图片能力通过 supports_vision 显式覆盖，UI提供“所选模型支持图片理解”选项，也可使用 LIVEGUARD_MONITOR_SUPPORTS_VISION=true/false。仓库只为默认 MiniMax-M3 和 gpt-4.1-mini 配置声明视觉默认值；其他自定义模型默认文字能力。这个声明不是连通性测试或模型能力自动探测。

目标计划保留视觉需求，并显示 model_capabilities / capability_warning；不会把“红色包装出现”改成“文本提到红色”。无视觉能力时不注册inspect_live_frame、不产生无文字的视觉调查，视觉/聊天目标反馈“等待确认，请配置支持看图的模型”。无图或未成功看图的视觉提醒在工具层和统一提交层都会被拒绝。现有运行会话读取与后续调查按当前绑定能力投影计划；原始关注意图不改变。

图片观察在Go中使用与提供方无关的ModelContent/ModelImage，协议适配器只负责翻译。Chat Completions先回传全部工具结果，再用user图片消息交给模型。DeepSeek工具多轮调用需要保留原生reasoning_content，这里保存原始assistant消息字段。协议依据：[OpenAI工具调用](https://developers.openai.com/api/docs/guides/function-calling)、[OpenAI图片输入](https://developers.openai.com/api/docs/guides/images-vision)、[DeepSeek工具推理字段](https://api-docs.deepseek.com/guides/thinking_mode/)。

三家共用同一套 Agent 行为不代表模型质量相同。各提供方仍需分别做协议兼容和真实模型验收；历史 MiniMax 实验和 27/30 结果不能作为三家统一后的验收结果。

## Agent 到底能调用哪些工具

### 统一调查工具：按能力注册

三个提供方共用一套简短的调查工具。观测查询涵盖 OCR、ASR 和已确认的视觉描述；看图工具按模型视觉能力注册；每次调查仍受统一工具调用预算限制。

| 工具 | 参数 | 实际返回 / 作用 | 服务端约束 |
|---|---|---|---|
| search_observations | scope、text 或 text_any、source、room_id、from、until、before + before_sequence + before_id、limit | 用同一个工具查询近期或整场 OCR/ASR/视觉/frame 观测；每页最多 12 条；返回 total_matches（符合筛选条件的观测记录总数） | text_any 最多 8 个短语，任一短语匹配即可；`has_more` 返回三段式游标，避免同一时间戳的记录在翻页时遗漏；总数统计的是观测记录，不是语义去重后的真实事件 |
| search_recent_alerts | category | 当前房间、指定类别、约 2 分钟内最多 5 条历史摘要，以及重复判断提示 | 提醒前必须收到查询结果；同类不自动等于同一事件 |
| inspect_live_frame | 可选 observation_id | 查看当前/触发截图；也可查看搜索结果中的归档截图 | 每调查最多一次成功看图；路径由服务端解析；视觉属性与聊天区域确认需要看图 |
| save_frame_evidence | 无 | 保存 Agent 刚检查过的同一张截图，并记录关联 OCR | 必须先看图并收到图像结果；仅在图片支持提醒或需留档时调用 |
| create_alert | category、summary、reason、confidence、visual_finding、evidence_ids、evidence_basis；可选 sampling | 提交带明确证据引用的提醒提案，并终止调查 | 必须单独调用；先收到证据搜索和提醒查询结果；置信度至少 0.72；引用 1–6 条本次调查实际读到的观测；实际写入由 Go 在模型返回后完成 |

没有独立的 `finish_review` 工具。无需提醒时，Agent 以结构化最终结果返回 `wait` 或 `ignore`；只有提醒使用 `create_alert` 工具提交。`evidence_basis=live` 表示当前时效判断，所引用观测必须未超过各自 valid_until 且属于当前目标版本；`history` 用于回答历史问题，允许引用较早观测，但不能据此声称事件此刻仍在发生。

create_alert 的文字上限：summary 200、reason 300、visual_finding 400 字符。工具 JSON 参数总大小最多 8192 字节。引用必须来自触发观测或本次 `search_observations` 已返回的页面；截图采样标记只有实际看图后才可引用。执行定义和前置校验见 [review_tools.go](../internal/monitor/review_tools.go) 与 [evidence.go](../internal/monitor/evidence.go)，通用执行限制见 [loop.go](../internal/agent/loop.go)。

### 目标规划工具与规则动作，不要混为一谈

set_monitoring_plan 是共享 AgentGoalPlanner 的结构化输出工具，只在新建/改目标时使用，不是调查 Agent 随时改用户目标的权限。最终计划仍需 Go 归一化。

本地规则轨迹里的 read_live_window、query_recent_alerts、reanalyze_evidence、request_human_takeover、create_alert 是 [runtime.go](../internal/monitor/runtime.go) 的固定动作名称，并非额外开放给模型的工具。reanalyze_evidence 是 Go 规则复核，不是再次调用模型或通用脚本。

Agent 不能调用 Shell、执行模型生成代码、读任意文件、改登录凭据、自动发评论、领券或操作订单。工具执行成功也不代表真实世界结论一定正确。

### 一次调查实例

用户目标：“100 元及以上优惠券真正发放时提醒我。”

1. OCR 采到“120元券稍后发放”。模型读取证据后以最终结果 `wait` 判断只是预告；预告观测留在流水中，可提出 candidate 采样建议。
2. 后续 OCR/ASR 采到“120元优惠券现在开抢”。新调查获得触发观测，再按目标搜索先前预告及后续变化。
3. 模型用 search_observations 选择近期证据或按目标搜索整场记录，再调用 search_recent_alerts 检查是否重复提醒。
4. 这是明确文字条件，可直接申请 create_alert；如果目标是“聊天区有人问券在哪里领”，则还必须看图确认区域。
5. Go 检查会话未停止、目标版本未变、执行未取消或超时，再原子写入调查和提醒；网页收到更新后重新读取状态。
6. 重复原文或重复摘要在去重窗口内被抑制；不同金额、不同活动不会仅因为类别相同被一律拦截。

这是说明性路径，不承诺每次模型都会选择完全相同的工具顺序。

## 证据与语义判断

### 信号能证明什么？

SignalEvent 包含 ID、RoomID、GoalVersion、Source、Text、TextRegions、FrameArchiveID、Confidence、ObservedAt、ValidUntil、ReceivedAt、Sequence，以及感知/采集耗时。Sequence 是接收顺序，不是按直播发生时间重排的顺序。

- OCR：Tesseract TSV 保留分行文字及像素行框 `text_regions`（最多 256 行），包括文字、坐标和置信度；去重后的 OCR 记录只保留新增行对应的框。行框本身没有聊天/字幕区域标签，聊天目标仍需实际看图确认。
- ASR：用户授权分享的音频短片段转写；每段独立录制约 6 秒，录完马上开始下一段，不等待上传/ASR。最多 2 个请求并行，满时跳过新片段并显示累计数量，暂停时取消在途上传。片段交接、浏览器调度和溢出仍可能漏音，不能称为绝对无缝。
- frame：采样元数据，表示需要检查图片。它的提示文字不能当成画面内容或事件证据。
- vision：模型实际看图后给出的相关描述（旧记录可能为m3_vision）。描述记录图片观察时间，但仍是模型解释，不是像素级自动校验结果。

截图采集路径会用截图完成时刻覆盖 OCR 结束时刻作为 ObservedAt，避免把 OCR 耗时误当成直播发生时间。上传 OCR 路径目前主要采用识别时刻，仍需进一步统一采集时间合同。

### OCR 错字、来源歧义、命中/引用/否定

共享 Agent 系统指令要求把网页文字视为不可信证据，检查目标条件、金额、时态、排除项、否定和猜测。聊天目标要求同时有 OCR、成功 `inspect_live_frame`、`save_frame_evidence` 和非空 visual_finding；即使模型把类别改成 custom_goal，也不能绕过聊天目标的区域要求。低可信文字需要补充证据，不能靠自信的模型结论覆盖来源问题。

当前服务端能验证“是否看过图、是否读过证据、是否满足结构与预算”，不能独立验证“模型描述的文字真的在聊天区”或“这句话是引用还是主播承诺”。引用、反讽、复杂否定仍依赖模型；本地规则仅检查少数否定词，能力更弱。

已保存文字行框、观测时间、目标版本和归档原图 ID；后续仍需“字幕同句但非评论”“引用旧活动”“复杂否定”“相似字符”等真实源端评测。OCR 和模型置信度仍不等于校准概率。

### 三层去重，解决不同问题

| 层 | 当前机制 | 限制 |
|---|---|---|
| 输入重试幂等 | 同一个 Event ID + 指纹重复则不再处理；ID 被不同内容复用返回冲突 | 指纹覆盖房间、来源、文本、置信度；回执最多 2048 条、最长 24 小时，不是永久 exactly-once |
| OCR 增量 | 每采集流最多 512 个行指纹，2 分钟窗口；空白/标点归一化后精确比较 | 不做模糊匹配；标点改变会被忽略，不能保证保留标点承载的含义；2 分钟后相同静态文字可能再次入队 |
| 提醒去重 | 2 分钟内同房间原文或摘要归一化相等时抑制 | 函数名带 semantic，但 Go 实际没有 embedding/语义距离；改写同义句仍需模型比较历史 |

OCR 去重先 Preview，只有成功入队后才提交指纹；队列拒绝不会提前记成“已见”，否则后续重试将丢掉未调查的证据。实现见 [ocr_dedupe.go](../internal/monitor/ocr_dedupe.go)、[dedupe.go](../internal/monitor/dedupe.go)、[service.go](../internal/monitor/service.go)。

## 调度、幂等与迟到结果

### 采集不中断，调查有界等待

浏览器采用独立截图循环和有界 OCR 流水线；每帧采样时间在截图完成时记录，OCR 输入使用系统临时文件并在识别后删除。模型仅在需要图片判断时调用 `inspect_live_frame` 获取新帧；如该图支持提醒或需要留档，再调用 `save_frame_evidence` 保存刚检查的同一张图。聊天目标实际间隔以采集指标为准，截图调用本身超过一秒时无法保证一秒采到一帧。OCR 完成后调用 SubmitSignal，模型调查不阻塞截图或 OCR。实现见 [capture_pipeline.go](../internal/monitor/capture_pipeline.go) 和 [review_tools.go](../internal/monitor/review_tools.go)。

| 限制 | 当前值 / 行为 |
|---|---|
| 活跃监控窗口 | 最多 8 个；单个 Run 当前创建 1 个房间 |
| 每窗口调查 | 最多 1 个；独立队列 worker + reviewInFlight 双重约束 |
| 全进程调查 | 最多 4 个槽位，就绪窗口按等待顺序公平获得；仍是单进程机制 |
| 等待队列 | 每窗口最多 8 条等待；另可有 1 条正在处理 |
| 文字排序 | 取消/更正等转折获得有限优先级；等待每 30 秒提升一级；同级按更早有效期、再按到达顺序处理；容量满时转折更新可替换尚未老化到同级的低优先级任务，原观测保留 |
| 待处理合并 | 替换等待背景帧；同来源/房间 30 秒内完全相同文字合并；句式相同而数字变化时采用新任务，原观测都留在流水；不合并任意改写 |
| 队列期限 | 从 ObservedAt 起算：默认文字 2 分钟、frame 30 秒；明确倒计时或“数字 + 秒/分钟/小时 + 后”按该时点；来源可给 valid_until。过期任务停止，历史不删除 |
| 输入时钟 | 不按过去时间拒绝观测；最多容许 2 分钟未来偏差，以检查明显时钟错误 |
| 队列截图 | 单文件最多 4 MiB；全 Service 暂存合计最多 64 MiB；结束、取消、替换时清理 |

队列满/图片超预算返回错误和 Pending 状态；刚写入的事件及回执被回滚，同一个 ID 可以在容量恢复后重试。已经入队的调查任务超过执行期限可能被跳过，观测流水仍保留；EventReceipt 不代表已调查或已经提醒。

每个窗口至多有一个就绪调查请求。全局等待表按 FIFO 授予槽位；仍处于节流期的窗口暂时跳过。取消/更正、数值更新以及临近到期的任务可跳过普通节流，但不绕过全局并发上限。Pending 提供 Waiting/Reviewing/Processed/Failed/Dropped/Expired/Coalesced；这些是当前进程的执行指标，不是持久处理账本。实现见 [pending.go](../internal/monitor/pending.go)、[review_gate.go](../internal/monitor/review_gate.go)、[signal_policy.go](../internal/monitor/signal_policy.go)。

### 同步 Ingest 与异步 SubmitSignal 的差异

同步 Ingest 保留给测试、兼容调用和合成负载：如果被节流或无槽位，会保留事件证据但不等待另一次调查。异步 SubmitSignal 才保证已入队信号在期限内主动等待执行，不需要再来一条评论唤醒它。因此同步压测的“接收成功”不等于“每条都完成模型调查”。

### 模型外执行，提交时重新校验

1. 仓储原子接受事件并分配 Sequence，生成该次目标快照。
2. 队列等待并发许可，取得后重新加载当前目标、采样状态和提醒。
3. 模型和工具在仓储锁/事务之外执行，避免远程请求阻塞状态提交。
4. 通过 Repository.Update 回调原子提交；回调内只做有界状态变更。
5. 提交检查 context、RunRunning、GoalVersion 和本次明确引用的固定证据；live 按每条 valid_until 再验时效，history 只用于历史结论并须先搜索整场记录。调查结束时不重新以当前最新几条记录替换引用。若已接收更晚的取消、更正或同句式数值变化，旧即时结论不生效。
6. 改目标先生成并校验新计划，原子递增 GoalVersion、重置采样，再撤销旧队列与在途 context。旧结果即使迟到，也不能写入新目标的提醒。
7. Pause 关闭队列并取消运行 context，但保留 Run 和流水；Resume 用同一 Run 重建采集/调查运行时。Stop 才会终结 Run；create_alert 仍须通过提交时状态校验。

MemoryRepository 用锁与快照副本；PostgresRepository 用事务和 SELECT FOR UPDATE。Repository Version 记录快照变更；GoalVersion 专门表示关注意图变更，两者用途不同。事务只保护一个进程/仓储中的状态一致性，没有做外部通知的事务 outbox。

### 下一步调度改进

已实现默认时效、明确倒计时/相对时间，以及可选 valid_until。它们约束调查是否还有意义，不会让 Run 自动结束，也不是未来事件的定时提醒。更复杂的事件身份、延期取消对应关系仍由 Agent 查询前后观测判断。

优先级和老化已落在 Go 队列；金额/日期数值改变只在其他文字相同的情况下识别为更新，保留两条原观测。更广泛的语义事件聚合、持久事件状态账本仍未实现。

## 成本与采样策略

成本分为本地截图/OCR、ASR 请求、文字模型请求、图片输入四部分。search_observations 只检索已落账的文字，不调用模型；录音上传发生在用户授权后，ASR 调用在调查之前。

- 默认先本地 OCR 行去重；没有新文字且没有到期视觉检查，通常不发起调查模型请求。
- 整屏 UI 变化也可能产生新 OCR 行，因此“没有目标相关事件”不等于“没有模型请求”。当前不会用关键词过滤掉所有目标外信号，因为仍需发现明确重要的突发事件。
- 计划 CaptureInterval 允许 6/12/24 秒，ReviewInterval 允许 15/30/60 秒；默认规则计划是截图 6 秒、判断 60 秒，明确及时性目标是 6/15 秒，普通视觉目标判断 30 秒。模型规划可在支持范围内选择。
- 全局 reviewGap 默认 15 秒，是判断启动间隔下限，不是处理完每条信号的保证；配置更大的下限会覆盖较快计划。
- 视觉计划有独立检查：初始 VisionInterval=max(30 秒, 计划判断间隔)，稳定等待时 60 秒；有新 OCR 的普通调查也可按需看图。
- 每次成功看图后图片仍可能随调查后续消息重复发送，因此“1 次看图工具”不等于“只计费 1 次图片输入”。

### 模型建议节奏，Go 掌握实际权限

终止决策可带 sampling，mode 为 observing / stable_wait / candidate；必须是完整成功决策、置信度至少 0.72 且读过证据。reason 有长度约束。next_event_at 若存在，必须是未来 24 小时内的时间；其语义依据主要靠模型，Go 当前只校验结构与范围。

| 模式 | 进入条件与行为 |
|---|---|
| observing | 使用初始目标计划 |
| stable_wait | 连续 3 次有效稳定建议，距上次变化至少 1 分钟；提醒决策不降频；明确及时性目标维持快采样；普通采集最长 24 秒，视觉计划最长 12 秒，判断/视觉间隔 60 秒 |
| candidate | 有效候选建议可立即加速到采集 6 秒、判断 15 秒、视觉 30 秒；持续 90 秒后恢复基准 |
| 已知事件附近 | next_event_at 前后约 2 分钟加速；只在当前采样流程读取状态时生效，不是独立持久定时任务 |

实现见 [sampling.go](../internal/monitor/sampling.go)。实际启动到启动间隔还会被截图/OCR耗时、失败退避和全局配置延长。Room 的 CaptureIntervalMS 与 CaptureDurationMS 用于观察真实采集，不应把“每 6 秒”写成用户端 6 秒内必达。

下一步：在 OCR 之前加区域图像差异门控；建立每 Run/全局 token、图片、ASR 时长和金额预算；历史查询结果只作为证据，实时提醒仍需核验当前事件状态。token 与金额预算尚未实现，当前只有次数/时长硬限制。

## 上下文与持久化检索

每次调查创建新的模型会话。初始消息只包含系统规则、用户目标、计划、采样状态、当前触发观测和当前时间；过去的模型对话不会自动传入。

Agent 自己通过工具组合本次任务需要的上下文：

1. `search_observations` 查询已经记录的 OCR、ASR、frame 和视觉观测。实时提醒用 `scope=recent`，整场回顾用 `scope=all`，再按关键词、来源、房间和时间范围筛选。
2. 需要确认 OCR 内容或画面来源时，用 `inspect_live_frame` 读取当前图或查询结果关联的归档截图。
3. `search_recent_alerts` 查询已经发过的提醒，判断是否重复。
4. 模型形成 alert / wait / ignore 后，Go 校验并保存调查与提醒。下一次调查重新搜索历史，不继承本次模型对话。

| 信息 | 如何进入模型 | 当前上限 / 留存 |
|---|---|---|
| 当前任务 | 系统规则、Goal/Plan/Sampling、当前触发观测、当前时间与工具定义 | 触发文字最多 600 字符；每次调查重新构建 |
| 查询到的历史观测 | Agent 调用 `search_observations` 后收到结果；提醒需在 `evidence_ids` 中明确引用 | 近期默认最多 6 条、默认最近 15 分钟；每页最多 12 条、每条最多 400 字符；单次调查最多 6 次搜索调用 |
| 历史提醒 | Agent 调用 `search_recent_alerts` 后收到摘要 | 当前房间、同类别、2 分钟窗口，最多 5 条，每条最多 200 字符；不是整场提醒搜索 |
| 调查内工具消息 | 当前调查的模型请求、工具响应及图片块 | 受轮数、调用次数、字段长度和执行超时限制；调查结束后释放 |
| 持久记录 | 不整段注入模型；通过查询按需读取 | 本地完整观测追加到 `runtime/liveguard.json.observations.jsonl`；PostgreSQL 保存到 `live_monitor_observations`；图片只在 Agent 调用 `save_frame_evidence` 后进入 `runtime/frames` |

翻页时把 `next_before`、`next_before_sequence`、`next_before_id` 原样传回对应参数。6 次搜索调用、每页 12 条是 Agent 的输入预算，不是数据库只能查 72 条，也不是精确的模型 token 窗口。后端会按完整筛选范围计算 `total_matches`，并只向模型返回当前页；如果 `has_more=true` 且调用预算耗尽，页面证据仍不完整。`text_any` 能把模型给出的多个说法合并成一次 OR 搜索，但仍是文字包含匹配，不会自动理解同义词或把重复 OCR 归并为真实事件。

观测时间没有全局入库拒绝阈值，也没有独立摘要 TTL：迟到观测仍可保存和搜索。live 引用在工具与提交时分别检查房间、目标版本、valid_until；默认文字有效期为两分钟，明确事件时间可改变有效期。history 须查询整场记录，旧证据不能用于声称事件此刻仍然有效。检索页以调查开始时刻为上界，保证本次证据身份固定；后续调查重新查询更新。

### “待确认、已提醒、被取消”分别在哪里？

- **等待处理**：PendingStatus/队列，只表示调查还没完成，不代表事件状态。
- **证据不足待确认**：InvestigationWaiting，记录本次调查没有足够证据；后续调查搜索观测重新核验，不自动重开同一个调查。
- **事件预告、延期、开始、取消或结束**：来源文字及时间保存在观测流水，模型解释保存在调查结论；没有独立事件状态账本，Agent 需要搜索前后观测判断变化。
- **已经提醒**：Run.Alerts；网页已读保存在浏览器 localStorage，与后端提醒记录分别管理。
- **用户改目标、暂停或停止**：GoalVersion、Run 状态和取消 context 由 Go 管理，提交时重新检查。

### 运行很多小时怎么办？

观测流水持续积累，模型上下文每次重新构建并按需加载。一次查询只读取已经提交的记录，不等待未来观测。队列任务过期不代表观测被删除，旧记录仍可检索；记录和截图目前没有自动清理，存储会随运行时间增长。

例如“3 小时后开奖”的预告可以一直留在观测流水中，后续调查按目标查询并寻找更新证据。当前没有独立持久日程和定时唤醒，不能承诺在没有新调查触发时准时执行提醒。Harness 没有 tokenizer 预计算、自动压缩或总 token 硬预算，当前通过有限查询结果、轮数和执行时间约束上下文大小。
