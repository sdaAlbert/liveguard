# LiveGuard 项目说明

> 本文件是项目的单一事实来源。范围、架构决策、完成状态和演示脚本都在这里持续更新。

## 1. 项目定位

LiveGuard 是一个使用 Go 构建的可恢复浏览器巡检 Agent 平台。用户输入直播页面地址和自然语言验收目标，Agent 生成受约束的检查计划，在独立 Chrome 会话中执行只读检查，并输出可追踪的截图和页面证据。

第一目标站点是真实抖音公开页面。为了得到可重复、可注入缺陷的闭环，第二阶段会增加一个本地直播活动测试站。

项目不绕过验证码，不自动关注、评论、点赞或送礼，不把“无法验证”伪装成“通过”。

## 2. 产品故事

直播活动上线前，产品、运营和测试需要根据需求逐项检查页面入口、登录状态、直播状态、地区或语言提示，并保留截图和复现步骤。LiveGuard 把自然语言要求转换成检查计划，自动完成可以安全执行的部分；遇到登录、验证码或含糊条件时暂停并请求人工处理。

一次运行的最终产物不是一句模型回答，而是一份包含检查项、观察结果、状态、截图、耗时和工具事件的验收报告。

当前版本已形成一次完整的 Agent Tool Loop：浏览器先收集证据；Responses API 可基于 strict function schema 提出 `diagnose_live_page` 调用；独立 Policy Gateway 审批后，任务经 Redis Streams 和 gRPC 进入 Docker Sandbox；工具结果回到 Agent 后，原先的 `unverified` 被更新为明确的业务失败和修复建议。模型不可用时会留下审计记录并确定性降级。

## 3. 核心原则

1. **证据优先**：每个结论必须绑定页面文本、URL、截图或工具结果。
2. **能力最小化**：模型只能选择白名单检查类型，不能执行任意代码。
3. **失败可见**：超时、验证码、模型失败和浏览器故障有独立状态。
4. **任务可恢复**：任务和步骤持久化；Worker 重启后重新领取未完成任务。
5. **真实与可重复并存**：真实抖音证明适应性，本地站证明正确性和闭环。

## 4. 当前架构

```text
Chrome 控制台 ──HTTP/SSE──> Go Control API
                              │
                              ├──> Append-only Browser Task Store
                              │
                              └──> Agent Worker
                                      ├──> OpenAI Planner
                                      ├──> Tool Policy
                                      ├──> Chrome Runner / 截图证据
                                      └──> PostgreSQL Run + Outbox
                                                   │
                                                   └──> Redis Stream
                                                          │
                                                          └──gRPC──> Sandbox Worker
                                                                         │
                                                                         └──> Docker Engine

所有阶段 ──OTLP──> OpenTelemetry Collector ──> Jaeger
```

控制面与 Sandbox Worker 已经拆成两个进程。控制面负责任务、Agent、浏览器与异步调度；Sandbox Worker 是唯一能够访问 Docker daemon 的进程。gRPC deadline、健康检查和错误码定义了服务边界，Trace Context 经 Redis 和 gRPC 跨进程传播。

Sandbox Lab 使用 Docker CLI 作为可替换的执行边界。HTTP API 只能选择服务端白名单场景，不能提交任意命令或 Docker 参数；容器创建、执行、检查、强杀和清理形成完整生命周期。

## 5. 任务状态机

```text
QUEUED -> PLANNING -> RUNNING -> COMPLETED
                  \          -> NEEDS_HUMAN
                   \         -> FAILED
                    --------> CANCELLED
```

进程启动时，遗留在 `PLANNING` 或 `RUNNING` 的任务会重新进入 `QUEUED`。恢复粒度是完整巡检任务；第二阶段改为按检查项恢复。

## 6. 第一阶段范围

### 输入

- 公开网页 URL，默认限制为 `douyin.com` 及其子域名。
- 自然语言巡检目标。

### 白名单检查类型

- 页面能否打开。
- 页面标题是否存在。
- 是否出现登录、验证码或安全验证。
- 页面中是否存在与目标有关的可见文本。
- 是否能识别直播中或未开播状态。

### 输出

- 任务状态和实时事件时间线。
- 每项检查的 `passed`、`failed`、`unverified` 或 `needs_human` 状态。
- 页面标题、最终 URL、可见文本摘要。
- PNG 截图证据。
- 模型规划是否成功、是否降级到确定性计划。

## 7. 两周演进路线

| 阶段 | 内容 | 验收方式 |
|---|---|---|
| M0 基础骨架 | Go API、控制台、状态机、持久化、任务 Worker | 创建任务后刷新页面，状态和事件仍存在 |
| M1 浏览器执行 | 独立可见 Chrome、CDP、截图、只读检查 | 对公开页面生成证据报告 |
| M2 Agent 规划 | OpenAI Responses API、结构化计划、预算和降级 | 同一页面用不同目标生成不同检查计划 |
| M3 恢复与治理 | 租约、按检查项 checkpoint、取消、超时、重试 | 杀掉 Worker 后恢复；超时不会永久占用任务 |
| M4 完整闭环 | 本地直播活动站、缺陷注入、修复后复测 | 错误配置失败，修复后同一规则通过 |
| M5 Infra 扩展 | PostgreSQL、Redis Streams、gRPC Runner、OpenTelemetry | Worker 进程隔离、崩溃重领、重复消息幂等、完整 Trace |
| M6 Agent 治理 | 原生 Tool Calling、Policy Gateway、Eval、熔断 | 该调用、该停止、越权拒绝、故障快速降级 |
| M7 开源交付 | 中英文 README、一键确定性演示、CI、安全边界和设计文档 | 新环境不配置模型 Key 也能启动、运行并看懂完整链路 |

## 7.1 Sandbox Lab V1 验收闭环

控制台的一键验收会并发启动四个临时容器：

| 场景 | 预期证据 | 实测结果 |
|---|---|---|
| 正常执行 | 非 root 身份运行并正常退出 | `uid=65534`，exit 0 |
| 超时终止 | 3 秒 deadline 后强杀 | `timed_out=true`，exit 137 |
| 只读拦截 | 写根文件系统失败 | `Read-only file system`，exit 1 |
| 断网拦截 | 容器无法解析外部域名 | `NETWORK_BLOCKED`，exit 42 |

所有场景同时施加 128 MiB 内存、0.5 CPU、32 PIDs、`network=none`、只读根文件系统、非 root 用户、capabilities 全部移除和 `no-new-privileges`。执行结束后检查容器状态并强制删除；当前实测为 4/4 通过且无遗留容器。

## 8. 技术选择

- **Go**：API、任务系统、Agent Runtime 和浏览器执行器。
- **OpenAI Responses API**：把自然语言目标转换为受约束的结构化检查计划。
- **Chrome CLI（当前）**：零第三方依赖地完成页面加载、DOM 导出和截图，并打开可见镜像窗口。
- **chromedp（M1.1）**：网络依赖可安装后，通过 Chrome DevTools Protocol 完成逐步操作和精确页面状态采集。
- **JSONL Event Store**：M0 用于展示崩溃恢复，后续由 PostgreSQL 替换。
- **SSE**：把执行事件实时推送到控制台。
- **Chrome 独立 Profile**：与日常浏览数据隔离，并允许人工接管。
- **PostgreSQL + Transactional Outbox**：原子保存 Run 与待投递事件。
- **Redis Streams**：异步分发、Consumer Group、Pending 和故障重领。
- **gRPC**：控制面到 Sandbox Worker 的强类型执行协议、deadline 和健康检查。
- **OpenTelemetry + Jaeger**：查看 HTTP、Agent、Browser、Redis、RPC 和 Docker 的完整 Trace。
- **Responses API Function Calling**：模型从严格工具 schema 中选择工具，保存 Call ID 和 token usage。
- **Policy Gateway**：在模型和执行层之间独立实施白名单与证据约束。
- **Agent Eval**：版本化案例比较 expected/actual，统计通过率、延迟和 token。
- **Circuit Breaker**：模型供应商失败时快速降级，冷却后用单探针恢复。

## 9. 当前 Infra 设计

- PostgreSQL 已保存 Sandbox Run、状态、幂等键和执行证据；浏览器任务迁移待完成。
- Redis Streams 已负责 Sandbox 任务投递、Consumer Group 消费确认和 `XAUTOCLAIM` 失败重领。
- 控制面通过 gRPC 调用独立 Sandbox Worker，统一 deadline、错误码和健康检查。
- OpenTelemetry 已串联 API、Agent、浏览器、Redis、gRPC 和 Docker 操作。
- 工具网关统一执行域名白名单、参数校验、超时、重试和结果裁剪。

Kafka 暂不进入两周 MVP。它更适合高吞吐、长保留、多消费者事件流；当前任务调度更需要较低部署成本和 Pending Entry 恢复能力，因此优先 Redis Streams。

## 10. 演示脚本

1. 在控制台输入真实抖音公开页面与检查目标。
2. Agent 生成计划，可见 Chrome 打开目标页面。
3. 控制台实时显示规划、导航、读取和截图事件。
4. 展示带截图证据的结构化报告。
5. 对出现登录或验证码的页面展示 `NEEDS_HUMAN`。
6. 后续演示中途杀掉 Worker，重启后恢复任务。

## 11. 当前状态

- [x] 明确产品定位、边界和两周路线。
- [x] 创建安全的本地 OpenAI API Key 配置。
- [x] M0：API、状态机、持久化和控制台，已完成端到端验证。
- [x] M1：Chrome CLI Runner 和截图证据（CDP 交互待实现）。
- [x] 增加本地 `/demo/live` 场景用于离线端到端验证。
- [x] 浏览器工具具有独立 25 秒 deadline，控制台提供无长连接截图模式。
- [x] 本地直播页已完成端到端验证：任务持久化、规划、Chrome DOM、截图、规则判断和报告全部贯通。
- [x] Sandbox Lab V1：白名单场景、Docker 容器生命周期、资源/权限/网络/超时策略和可视化证据。
- [x] Sandbox 真实 Docker 验收 4/4 通过，容器全部清理。
- [ ] M2：OpenAI Planner（实现完成；当前网络无法连接 API，已验证确定性降级）。
- [x] M3.1：Sandbox PostgreSQL 持久化、Redis Streams 队列、幂等提交和 Worker 崩溃重领。
- [x] M3.2：浏览器观察、Agent 工具决策、Sandbox 执行和结果综合的单轮 Tool Loop。
- [ ] M3.3：浏览器任务租约、按检查项 checkpoint 和统一重试策略。
- [x] M4：本地直播活动缺陷注入、诊断、修复与复测闭环。
- [x] M5：Redis、PostgreSQL、gRPC 和 OpenTelemetry。
- [x] M6.1：原生 Tool Calling、Policy Gateway、决策审计、Agent Eval 和共享熔断器。
- [x] M7：Apache-2.0、双语 README、`.env.example`、CI、一键演示、安全说明与两篇核心设计文档。

阶段测试与实际运行证据见 [`docs/PHASE-1-REPORT.md`](docs/PHASE-1-REPORT.md)、[`docs/PHASE-2-SANDBOX-REPORT.md`](docs/PHASE-2-SANDBOX-REPORT.md)、[`docs/PHASE-3-INFRA-REPORT.md`](docs/PHASE-3-INFRA-REPORT.md)、[`docs/PHASE-4-AGENT-LOOP-REPORT.md`](docs/PHASE-4-AGENT-LOOP-REPORT.md)、[`docs/PHASE-5-RPC-OTEL-REPORT.md`](docs/PHASE-5-RPC-OTEL-REPORT.md) 和 [`docs/PHASE-6-TOOL-CALLING-EVAL-REPORT.md`](docs/PHASE-6-TOOL-CALLING-EVAL-REPORT.md)。

核心架构取舍见 [`docs/DESIGN-OUTBOX-STREAMS.md`](docs/DESIGN-OUTBOX-STREAMS.md) 和 [`docs/DESIGN-POLICY-SANDBOX.md`](docs/DESIGN-POLICY-SANDBOX.md)。

## 12. 已知限制

- 真实抖音页面结构和访问策略会变化，公开页面能力需要实机验证。
- Sandbox V1 只执行服务端白名单场景；下一版再接受经过策略校验的代码包，并增加临时工作区、镜像 allowlist 和并发配额。
- 当前 Sandbox Worker 依赖本机 Docker daemon，适合单机演示；生产环境需要远程节点池、镜像预热和调度配额。
- 当前 Chrome CLI 执行器使用一个可见镜像窗口和一个自动检查进程；逐步操作画面要等 CDP 执行器完成后才能完全同步。
- 第一版任务存储适合本机演示，不适合多实例并发写入。
- 第一版 Agent 负责规划，执行层仍由确定性检查器控制。
- 当前开发机访问 OpenAI API 失败，模型规划尚未完成在线验证；失败会降级到确定性计划。
- 当前租约没有 heartbeat，执行时间超过租约上界的新工具需要先实现续租。
- 浏览器任务仍使用 JSONL，PostgreSQL 目前只保存 Sandbox Run 和 Outbox。
