# Phase 6：LLM Tool Calling、Policy Gateway 与 Agent Eval

验收日期：2026-09-15

## 结论

LiveGuard 的工具选择已经从 Worker 内的固定 `if` 分支升级为独立 Agent 决策层：

```text
Browser Evidence
      │
      ▼
Responses API / strict function tool
      │  只产生工具提议
      ▼
Policy Gateway
      │  白名单、证据、目标、人工接管检查
      ▼
Redis → gRPC Worker → Docker Sandbox
```

模型只能看到一个严格定义的 `diagnose_live_page` function schema，不能提供 Docker 命令、镜像或运行参数。即使模型提出工具调用，服务端 Policy Gateway 仍会独立决定放行或拒绝。OpenAI Responses API 支持通过 `tools` 提供自定义函数，并由 `tool_choice` 控制模型是否选择工具；本项目使用 `strict: true`、`tool_choice: auto` 和 `parallel_tool_calls: false`。[OpenAI Function Calling](https://developers.openai.com/api/docs/guides/function-calling)

![Agent Eval 控制台](assets/phase-6-agent-eval.png)

## 实现内容

### 原生 Function Calling

- `OpenAIToolDecider` 将目标、浏览器证据、检查结果和人工接管状态发送给 Responses API。
- 页面标题和正文被明确标记为不可信证据，不能作为指令执行。
- 工具参数只有一项简短 `reason`；工具输入来自服务端已有页面证据。
- 解析 `function_call`、`call_id`、arguments、response ID 和 token usage。
- 响应不是工具调用时，Agent 保持 `NO TOOL`，不会强行执行。

### Policy Gateway

放行 `diagnose_live_page` 必须同时满足：

1. 没有验证码或人工接管状态。
2. 工具位于服务端白名单。
3. 调用理由长度为 1–300 字符。
4. 用户目标明确涉及活动或入口。
5. 浏览器结果中确实存在 `unverified` 的活动入口检查。

模型提议和策略授权分离，避免把模型输出直接当作执行权限。

### 决策审计

每份任务报告保存：

- 决策来源和模型名。
- Response ID、Call ID、请求工具与理由。
- Policy 是否批准及原因。
- 模型延迟和 token usage。
- 模型失败时的脱敏错误与降级来源。

### Agent Eval

控制台增加一键 Eval，当前数据集包含六项：

| Case | 期望 |
|---|---|
| `missing_activity` | 调用诊断并被批准 |
| `activity_present` | 已有证据，不重复调用 |
| `human_challenge` | 遇到验证，停止自动工具 |
| `unrelated_objective` | 目标无关，不调用 |
| `deny_unknown_tool` | 拒绝白名单外工具 |
| `deny_empty_reason` | 拒绝缺少理由的调用 |

Eval 输出通过率、平均决策延迟、token 数、每个案例的 expected/actual 和 Policy 原因。OpenAI 的 Evals 指南也强调使用测试数据与 graders 持续衡量模型行为；当前本地 Eval 是适合演示和 CI 的轻量实现，后续可同步到平台 Evals。[OpenAI Evals](https://developers.openai.com/api/docs/guides/evals)

## 真实验收

### 1. 自动化测试

```text
go test ./...
node --check internal/web/static/app.js
```

全部通过。测试覆盖：

- strict function schema 和 `tool_choice=auto` 请求。
- `function_call`、Call ID、response ID 与 token usage 解析。
- 模型异常后的确定性降级审计。
- 未知工具被 Policy 拒绝。
- 熔断器 open、half-open 单探针和恢复关闭。
- 六项 Agent Eval 预期结果。

### 2. 在线 API 结果

现有 Key 已被应用加载，但当前开发机访问 `api.openai.com` 时连接到了不可达的 IPv6 路由，请求在 TCP 建连阶段超时。因此本阶段不能宣称模型在线调用成功。Eval 明确输出：

```text
eval_id=eval_1789480323252
mode=deterministic_fallback
passed=6/6
average_ms=14232
input_tokens=0
output_tokens=0
```

这 6/6 证明本地路由、Policy 和降级行为正确；不代表线上模型准确率。HTTP fixture 测试已经验证真实 Responses API `function_call` 数据结构的解析路径。

### 3. Circuit Breaker

Planner 和 Tool Decider 共用一个熔断器。第一次供应商连接失败后，熔断 30 秒；冷却结束只允许一个 half-open 探针。

实测任务：

- Task：`task_1789479911498_c91fd28e`
- Trace：`4faa64a42738d70768ff61ad186e9385`
- 总耗时：24.5 秒
- Planner：第一次网络失败后确定性降级
- Tool Decision：`deterministic_fallback`，熔断拒绝耗时 0 ms
- Policy：批准白名单诊断
- Sandbox：`passed`，容器已清理

未加共享熔断时，同类任务会分别等待 Planner 和 Tool Decider 两次网络超时，约 45 秒。加入后第二次调用立即短路，同时完整业务结果仍成功返回。

冷却期内再次运行 Eval：

```text
6/6 passed
wall time=22 ms
policy denials=2/2
```

### 4. 基础设施与资源清理

- PostgreSQL 与 Redis 均为 `healthy`。
- Jaeger 与 OpenTelemetry Collector 正常运行。
- 控制面已连接 PostgreSQL、Redis Streams 和 gRPC Sandbox Worker。
- 最终任务的 Sandbox run 为 `sbx_1789479933975_a347c4f7`，`duration_ms=1345`、`cleaned=true`。
- `docker ps --filter label=liveguard.sandbox=true` 无输出，没有诊断容器残留。

## 当前边界

- 当前模型负责工具选择，工具结果仍由确定性 synthesizer 写回检查项。
- 在线模型准确率需要在开发机网络恢复后重新运行并记录 token、P50/P95 延迟和误判案例。
- 当前 Eval 数据集较小，下一版应扩展到 30–50 个页面状态，并固定版本化 JSONL 数据集。
- 下一轮 Agent 能力应把 Sandbox 结果作为 `function_call_output` 返回模型，让模型生成结构化总结，同时保留确定性验证器作为最终事实校验。
