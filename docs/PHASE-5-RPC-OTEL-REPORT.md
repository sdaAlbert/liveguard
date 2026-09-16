# Phase 5：独立 Sandbox Worker 与全链路追踪验收报告

验收日期：2026-09-15

## 结论

这一阶段已经把 Docker 执行从 LiveGuard 控制面拆成独立 gRPC Worker，并跑通下面的真实链路：

```text
HTTP 创建巡检
  → Agent 规划与浏览器取证
  → PostgreSQL Run + Transactional Outbox
  → Redis Stream / Consumer Group
  → gRPC SandboxExecutor.Execute
  → Docker 隔离容器
  → PostgreSQL 保存执行证据
  → Agent 综合最终报告
```

OpenTelemetry 的 W3C Trace Context 跨越异步 Redis 消息和同步 gRPC 调用。Jaeger 中一次任务同时包含 `liveguard-control-plane` 和 `liveguard-sandbox-worker` 两个服务。

![Agent 任务证据](assets/phase-5-agent-trace.png)

![Jaeger 跨服务 Trace](assets/phase-5-jaeger-trace.png)

## 本阶段实现

- `SandboxExecutor` protobuf/gRPC 契约、客户端、服务端、标准健康检查和 reflection。
- 独立 `cmd/sandbox-worker` 进程；控制面在 durable 模式下不再直接访问 Docker。
- PostgreSQL 事务内同时写 `sandbox_runs` 与 `sandbox_outbox`，后台发布器再投递 Redis，关闭数据库与队列双写窗口。
- Redis Streams Consumer Group、45 秒数据库租约、50 秒 Pending 重领、attempt fencing 和结果保存校验。
- 幂等键同时校验请求摘要，避免同一个键悄悄复用不同输入。
- 无效消息进入 `liveguard:sandbox:dlq` 并 ACK，避免毒消息永久阻塞消费组。
- Redis 读取失败退避、ACK 错误日志、16 KiB 子进程输出上限、Worker 并发上限 2。
- OTel spans 覆盖 HTTP、Agent、Planner、Browser、Redis consumer、gRPC client/server 和 Docker executor。
- 控制台显示 PostgreSQL、Redis、Worker、Outbox、Pending、DLQ，并提供任务和工具的 Jaeger Trace 链接。

## 真实验收证据

### 1. Agent Tool Loop

- Task：`task_1789465910736_79ebd46a`
- Sandbox Run：`sbx_1789465911900_cfe910aa`
- Trace：`49ea1e716b5facf7070a15c14312644f`
- 结果：任务 `completed`，工具 `passed`，容器 `cleaned=true`
- 输出：`activity_entry=missing`、`live_status=present`，并给出检查活动组件配置与发布开关的建议。
- 产品结论：页面能打开且直播正常，但本次发布缺少活动入口，产品或研发需要检查营销组件和发布开关。

### 2. Trace 连续性

Jaeger 查询结果：

- Services：2
- Spans：9
- 控制面：`POST /api/tasks`、`agent.run`、`agent.plan`、`browser.inspect`、`agent.decide_tool`、`redis.sandbox.consume`、gRPC client
- Worker：gRPC server、`docker.sandbox.execute`

任务、工具结果和 Jaeger 中的 Trace ID 完全一致。

### 3. Worker 崩溃恢复

1. 停止 Sandbox Worker。
2. 提交 `sbx_1789466082666_7711ef60`。
3. 控制面保持可用，健康状态为 `worker=unavailable`，Redis 为 `pending=1`。
4. 重启 Worker；租约到期后消息被自动重领。
5. 最终 `status=passed`、`attempt=2`、`cleaned=true`、`pending=0`。

这证明队列提供至少一次交付，数据库租约和 attempt fencing 防止旧执行结果覆盖新结果。

### 4. 毒消息隔离

向 Redis Stream 注入一条缺少 `run_id` 的消息后：

```text
pending=0
dead_letter=1
worker=healthy
```

坏消息被保存到 DLQ，正常消费没有被卡住。

### 5. 自动化验证

```text
go test ./...
```

全部 Go package 通过。新增的 gRPC bufconn 集成测试验证 RPC 序列化、Sandbox 执行、attempt fencing token、退出码、输出和清理证据不会在进程边界丢失；Docker 参数顺序回归测试防止运行标签再次落到镜像名之后。

独立 Worker 的真实四场景回归 `suite_1789466272790_23c1f668` 为 4/4 通过：正常执行 exit 0、超时强杀 exit 137、只读拦截 exit 1、断网拦截 exit 42；四个容器均 `cleaned=true`，结束后 Outbox 与 Pending 均为 0。

## 本阶段发现并修复的问题

真实运行首次发现 Docker `--label` 插入到了镜像名之后，容器把它当作可执行文件并返回 exit 127。修复参数边界后补了回归测试，再次执行同一业务场景通过。这个过程本身适合在面试中说明：单元测试验证局部逻辑，真实容器测试验证 Docker CLI 的语义边界。

## 当前限制

- 浏览器任务仍保存于 JSONL；PostgreSQL 当前负责 Sandbox Run 和 Outbox。
- 租约以固定上界覆盖现有白名单工具，尚未实现长任务 heartbeat。
- 本阶段验证了单 Worker 故障恢复，尚未做多 Worker 并发压测和延迟分位数基线。
- 当前在线验收使用确定性 Planner；OpenAI Planner 已有降级路径，但开发机网络还未完成在线模型验证。
- Sandbox 只接受服务端白名单工具，尚未支持签名代码包和临时工作区上传。

下一阶段应优先实现“LLM 原生 Tool Calling + Agent Eval”：模型从工具 schema 中选择工具，策略层独立批准或拒绝，保存每次决策输入输出，并用一组正常、缺入口、验证码、超时页面衡量成功率、误判率、工具调用次数和成本。
