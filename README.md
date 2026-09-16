# LiveGuard

[中文](README.md) · [English](README_EN.md)

> 一个用 Go 构建的直播页面巡检 Agent：浏览器负责取证，模型负责提出工具调用，策略层负责授权，Sandbox 负责隔离执行，执行链展示持久化投递、策略审计和分布式追踪。

LiveGuard 不是聊天机器人，也不是通用网页自动化框架。它用一个具体业务场景回答一个工程问题：**怎样让不稳定的模型安全、可靠、可观测地调用真实工具？**

![Agent Eval 控制台](docs/assets/phase-6-agent-eval.png)

## 一次任务会经历什么

1. 用户提交直播页面和验收目标。
2. 独立 Chrome 会话读取页面标题、可见文本和直播状态，并保存截图。
3. Agent 根据证据决定是否请求 `diagnose_live_page`。
4. 服务端 Policy Gateway 组件检查工具白名单、目标、证据和人工接管状态。
5. 通过授权的任务与 Outbox 在 PostgreSQL 中原子保存，再进入 Redis Streams。
6. gRPC Sandbox Worker 把已经采集并裁剪的页面证据交给受限 Docker 容器中的服务端预定义诊断；容器本身不访问目标网页。
7. 工具结果回到任务报告；OpenTelemetry 用同一 Trace ID 串起 HTTP、Agent、Redis、gRPC 和 Docker。

模型只有**建议权**，没有 Docker 参数、任意命令或网络访问的控制权。即使模型不可用，确定性降级也会保留审计记录并完成可验证的本地流程。

## 一键本地演示

需要 Windows、Go、Google Chrome、支持 `docker compose up --wait` 的 Compose v2，以及已经启动的 Docker Desktop。在仓库根目录执行以下命令。默认 Demo 使用确定性 Agent，不需要 `.env.local` 或 API Key，也不会访问在线模型；首次拉取镜像和 Go 模块的时间取决于网络。

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\demo.ps1
```

如需自动打开控制台，在命令末尾添加 `-OpenBrowser`。

脚本会启动 PostgreSQL、Redis、OpenTelemetry Collector、Jaeger、gRPC Sandbox Worker 和 Go 控制面。然后打开：

- 控制台：<http://127.0.0.1:8080>
- Jaeger：<http://127.0.0.1:16686>

在控制台点击“载入活动入口缺失场景”并启动任务。一次成功验收应看到：`SOURCE=deterministic`、`diagnose_live_page` 获得 `APPROVED`、活动入口检查由 `unverified` 更新为 `failed`、Sandbox Run 完成并带有 Trace ID。页面底部的 Agent Eval 应为 6/6；Sandbox Lab 应为 4/4，并验证非 root、只读根文件系统、断网和超时强杀。复制 Trace ID 到 Jaeger 搜索，可以查看控制面到 Worker 的 spans。

查看状态或停止：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\demo.ps1 status
powershell -ExecutionPolicy Bypass -File .\scripts\demo.ps1 stop
```

需要手动启动或调试单个进程时，参见 [贡献指南](CONTRIBUTING.md)。

若启动失败，先查看 `runtime/demo/*.err.log`。默认占用 `8080`、`9090`、`4317`、`5433`、`6380` 和 `16686` 端口。`stop` 会停止受脚本跟踪的两个 Go 进程和 Compose 服务，但保留 PostgreSQL/Redis volumes；彻底删除本地数据可执行 `docker compose down -v`。

## 架构

```mermaid
flowchart LR
    U[Browser UI] -->|HTTP / SSE| C[Go Control Plane]
    C --> B[Chrome Evidence Runner]
    B --> A[Agent Tool Proposal]
    A --> P[Policy Gateway]
    P -->|approved| DB[(PostgreSQL<br/>Run + Outbox)]
    DB --> Q[(Redis Streams)]
    Q -->|consume / reclaim| G[gRPC Sandbox Worker]
    G --> D[Restricted Docker Sandbox]
    D -->|tool result| C
    C -. OTLP .-> O[OpenTelemetry Collector]
    G -. OTLP .-> O
    O --> J[Jaeger]
```

在本项目的进程边界内，控制面不接触 Docker daemon，只有绑定在本机回环地址的 Sandbox Worker 创建容器。PostgreSQL 是 Sandbox Run 的事实来源；Redis Streams 负责异步投递和 Pending 重领；gRPC 定义执行边界；Trace Context 会跨 Redis 和 gRPC 传播。浏览器任务目前仍使用本地 JSONL 存储。

## 已经实现并验证

- Responses API strict function calling、Tool Call ID 和 token 审计。
- 服务端 Policy Gateway 组件：白名单、证据约束、目标校验和人工接管。
- PostgreSQL Transactional Outbox、幂等键和请求哈希冲突检测。
- Redis Streams Consumer Group、`XAUTOCLAIM`、Pending 和 DLQ。
- gRPC deadline、健康检查、并发限制和 stale-result fencing。
- Docker Sandbox：非 root、只读根文件系统、`network=none`、capabilities 全移除、`no-new-privileges`、CPU/内存/PID/超时/输出限制和执行后清理。
- OpenTelemetry 跨 HTTP、Agent、Redis、gRPC 和 Docker 的完整 Trace。
- 6 项 Agent 路由/策略 Eval，以及 4 项真实 Sandbox 策略验收。
- 模型超时后的共享熔断器和确定性降级。

自动化检查（最后一项需要 Node.js 22+）：

```powershell
go test ./...
go vet ./...
node --check internal/web/static/app.js
```

## 关键设计取舍

### 为什么是 Redis Streams，不是 Kafka？

当前目标是单机可复现的任务调度，需要 Consumer Group、Pending Entry 和失败重领。Redis Streams 已满足这些约束，部署成本更低。Kafka 适合更高吞吐、长保留和多消费者事件分发；项目没有为了增加技术名词而引入它。

### 为什么模型不能直接运行代码？

页面内容和模型输出都属于不可信输入。模型只能从严格 schema 中提出一个服务端预定义工具，Policy Gateway 再决定是否授权。工具命令、镜像、资源和网络策略均由服务端固定。

### 为什么使用 Outbox？

任务记录和待发布事件必须同时成功或同时失败。Outbox 先在同一 PostgreSQL 事务中落盘，再异步发布到 Redis；即使进程在两步之间崩溃，未发布事件仍会被重新扫描。

详细说明见 [Outbox 与 Redis Streams](docs/DESIGN-OUTBOX-STREAMS.md) 和 [Policy Gateway 与 Sandbox](docs/DESIGN-POLICY-SANDBOX.md)。

## 安全边界

这是本地参考实现，不是生产级多租户平台：

- HTTP 和 gRPC 默认只绑定 loopback，没有生产级认证或 TLS。
- Docker 共享宿主机内核，不能等同于 Firecracker、gVisor 或 Kata 强隔离。
- 当前只允许服务端预定义诊断，不接受任意 LLM 代码。
- 在线模型效果必须单独评测；开发机网络不可用时显示的 `deterministic_fallback` 不代表模型准确率。
- 当前实际使用 Redis Streams，没有使用 Kafka。

完整说明见 [SECURITY.md](SECURITY.md)。

## 项目结构

```text
cmd/liveguard/          Go 控制面与 Web 服务
cmd/sandbox-worker/     唯一能够访问 Docker 的 gRPC Worker
internal/agent/         Planner、Tool Calling、Policy、Eval、熔断器
internal/browser/       Chrome 页面证据采集
internal/sandbox/       Outbox、Redis Streams、任务租约、容器生命周期
internal/sandboxrpc/    gRPC client/server adapter
internal/web/           HTTP API、SSE 与嵌入式控制台
api/sandbox/v1/         Protobuf 执行协议
docs/                   设计说明和阶段验收报告
```

## 可选的在线模型模式

复制环境模板并只在本地填写自己的 Key：

```powershell
Copy-Item .env.example .env.local
```

```text
OPENAI_API_KEY=your-key
LIVEGUARD_LLM_MODE=auto
```

默认模型为 `gpt-5.6-luna`，可通过 `LIVEGUARD_MODEL` 修改。在线成功时决策审计的 `SOURCE` 会显示 `openai:<model>`；`deterministic_fallback` 表示调用失败后降级，不能当作在线模型结果。程序不会打印 Key；`.env.local`、任务状态、浏览器 Profile、截图和构建缓存已加入 `.gitignore`，发布前仍应扫描暂存区和 Git 历史。

一键脚本会强制使用确定性模式，不会采用 `.env.local` 中的 `auto`。在线验证需要按[贡献指南的手动启动步骤](CONTRIBUTING.md#start-processes-manually)分别启动 Worker 和控制面。

## 进一步阅读

- [项目范围与演进记录](PROJECT.md)
- [Phase 5：gRPC 与 OpenTelemetry 验收](docs/PHASE-5-RPC-OTEL-REPORT.md)
- [Phase 6：Tool Calling、Policy 与 Eval 验收](docs/PHASE-6-TOOL-CALLING-EVAL-REPORT.md)
- [开源项目对标调研](docs/OPEN-SOURCE-RESEARCH.md)
- [贡献指南](CONTRIBUTING.md)

## License

[Apache-2.0](LICENSE)
