# LiveGuard 开源可行性与同赛道项目调研

调研日期：2026-09-15

## 结论

LiveGuard 适合开源，但应定位为：

> 一个用 Go 实现的、面向直播页面巡检场景的 Agent Execution Platform 参考项目，展示浏览器取证、LLM Tool Calling、服务端策略授权、异步任务、隔离执行、失败恢复、Agent Eval 与全链路观测。

不建议把它描述成通用 Agent Framework、通用浏览器 Agent 或生产级强隔离 Sandbox。成熟开源项目在这些方向已经有更大的功能面和社区。LiveGuard 的价值在于小而完整、场景具体、容易在一台电脑上演示，以及基础设施设计可以逐层讲清楚。

## 最值得研究的项目

| 项目 | 它更强的地方 | LiveGuard 可以保留的差异 |
|---|---|---|
| [E2B Runtime](https://github.com/e2b-dev/runtime) | Go 控制面、Firecracker microVM、快照恢复、控制面/数据面分离、网络策略、密钥与身份、完整 OTel | E2B 是通用 Sandbox 云；LiveGuard 是业务 Agent 的端到端参考实现，Windows + Docker Desktop 即可演示 |
| [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) | Docker/Kubernetes、gVisor/Kata/Firecracker、多语言 SDK、CLI、MCP、网络策略、Credential Vault | LiveGuard 更小，包含明确的直播巡检决策、Policy、Eval 和业务报告 |
| [OpenHands](https://github.com/OpenHands/OpenHands) | 完整编码 Agent、Browser/Shell/File 工具、事件流、会话和成熟运行时 | LiveGuard 不做通用编码，重点展示一个后端候选人能完整解释的业务闭环 |
| [Dify](https://github.com/langgenius/dify) | Agent Workflow、RAG、模型管理、插件、可视化编排和生产部署 | LiveGuard 的 Go、Redis Streams、PostgreSQL Outbox、gRPC Worker、Docker 执行链更集中地体现后端 Infra |
| [Browser Use](https://github.com/browser-use/browser-use) | 浏览器理解、动态网页操作、云浏览器、CAPTCHA 和代理能力 | LiveGuard 强调只读取证、服务端授权、异步诊断和审计，不追求通用网页操作 |
| [CloudWeGo Eino](https://github.com/cloudwego/eino) | 字节开源的 Go LLM/Agent 框架，包含组件、Graph、ADK、工具和中断恢复 | 可在后续版本用 Eino 替换自研的部分模型编排，同时保留 LiveGuard 的运行时和业务层 |
| [SandboxFusion](https://github.com/bytedance/SandboxFusion) | 字节开源代码执行与评测 Sandbox，支持多语言与多种 benchmark | LiveGuard 包含浏览器证据、业务决策、持久化队列、RPC、策略网关和可视化报告 |
| [agent-infra/sandbox](https://github.com/agent-infra/sandbox) | 单容器集成 Browser、Shell、File、MCP、VS Code、Jupyter 与多语言 SDK | LiveGuard 的工具范围更窄，但权限更容易审计，基础设施链路更适合面试讲解 |

Daytona 曾经是很好的 Sandbox 架构参考，但其 GitHub README 已注明：从 2026 年 6 月起核心开发转为私有代码库，公开仓库不再维护。因此不建议把它作为当前首选依赖或主要开源对标。[Daytona repository notice](https://github.com/daytonaio/daytona)

## 当前仓库具备的开源价值

1. 业务目标具体：检查直播状态和活动入口，不是空泛聊天机器人。
2. Agent 有真实决策：模型提出工具调用，Policy Gateway 独立批准或拒绝。
3. 执行链完整：HTTP → Agent → PostgreSQL Outbox → Redis Streams → gRPC Worker → Docker Sandbox。
4. 失败路径可以演示：模型超时降级、熔断、任务重试、Pending reclaim、DLQ、幂等和容器清理。
5. 可观测：任务、工具调用和 Sandbox run 可以通过同一个 Trace ID 串联。
6. 可以本机验收：带控制台、浏览器截图、Sandbox 测试套件和 Agent Eval。

这比单纯调用 LLM API 的作品更能证明后端和 AI Infra 能力。

## 不能夸大的地方

- Sandbox 目前是 Docker 容器隔离，共享宿主机内核，不等同于 microVM、gVisor 或 Kata 的强隔离。
- gRPC Worker 和 HTTP 控制面默认只绑定 localhost，没有生产级认证、TLS 和租户隔离。
- Docker 镜像使用 tag，尚未固定 digest 或验证镜像签名。
- 浏览器能力以页面取证为主，没有 Browser Use 那样的通用交互和复杂页面自治能力。
- Eval 只有 6 个固定案例，当前开发机连接 OpenAI 超时，6/6 是降级和 Policy 结果，不是线上模型准确率。
- 项目使用 Redis Streams，而不是 Kafka。面试时可以说已经理解并实现异步事件流、消费组、重试和 DLQ，但不能说项目使用了 Kafka。

## 开源准备状态

- [x] Apache-2.0 `LICENSE`。
- [x] 不含密钥的 `.env.example`。
- [x] 写明 Docker、网络、密钥与本地服务边界的 `SECURITY.md`。
- [x] 包含启动、测试与 Proto 生成说明的 `CONTRIBUTING.md`。
- [x] GitHub Actions 执行 Go 测试、静态检查和前端语法检查。
- [x] README 首屏包含定位、架构图、两分钟启动命令、能力边界和真实控制台截图。
- [x] `.gitignore` 排除 `.env.local`、`state/`、`runtime/`、`.cache/`、内部学习日志和运行产物。
- [x] Windows + Docker Desktop 的确定性一键演示，不需要模型 Key。
- [ ] 录制 60 秒操作 GIF；当前静态截图已经可以说明界面和验收结果。
- [ ] 创建首个提交并推送 GitHub；发布前再执行一次 secret scan 和依赖许可证检查。

当前扫描没有发现硬编码 OpenAI Key；命中的 `OPENAI_API_KEY` 只是源码中的环境变量名。

## v0.1 发布前实测

2026-09-16 在 Windows + Docker Desktop 上按 README 的一键脚本重新验收：

- PostgreSQL、Redis 和 gRPC Worker 均为 `healthy`，控制面使用 durable 模式。
- Agent Eval 为 6/6，模式为 `deterministic`。
- Sandbox Lab 为 4/4：正常执行、只读拦截、断网拦截和超时强杀全部通过，容器全部清理。
- “活动入口缺失”业务任务最终为 `completed`；Agent 请求 `diagnose_live_page`，Policy 批准，Sandbox 返回缺失证据，检查项由 `unverified` 更新为 `failed`。
- Jaeger 根据任务 Trace ID 找到 9 个 spans，覆盖 `agent.plan`、`browser.inspect`、`agent.decide_tool`、Redis 消费、gRPC Execute 和 `docker.sandbox.execute`，服务包含控制面与 Sandbox Worker。

## 推荐的后续技术路线

### 开源 v0.1

- 先完成仓库卫生、CI、架构图和一键启动。
- 保持 Redis Streams，不为勾选技术名词强行加入 Kafka。
- 把“Docker 容器隔离”和“预定义工具”写清楚。

### 面试增强 v0.2

- 抽象 `EventBus`，提供 Redis Streams 和 Kafka 两种 adapter，并做一次故障与吞吐对比。
- 用 Eino ADK 实现可选 Agent backend，比较自研 loop 和框架 loop 的取舍。
- 将 Sandbox 输出作为 `function_call_output` 交回模型，再由确定性校验器核对事实。
- 扩充 30–50 个版本化 JSONL Eval，记录准确率、P50/P95、token 和失败类别。

### 生产化研究 v0.3

- Worker mTLS、认证、租户配额与细粒度 RBAC。
- 镜像 digest、签名验证和依赖供应链检查。
- Linux 环境增加 gVisor/Kata 可选 runtime，并与 Docker 默认模式对比。
- 水平扩容、背压、容量模型和压测报告。

## 对外介绍建议

仓库标题可以使用：

> LiveGuard — A Go reference architecture for evidence-driven browser agents with policy-gated tool calling and sandboxed execution.

面试中应说明这是针对直播巡检场景做的个人参考实现，借鉴成熟系统的思想，但核心执行链、失败恢复、策略授权、审计与可视化均在项目中实际跑通。
