# 从模型建议到安全执行：Policy Gateway 与 Sandbox

LiveGuard 把“模型认为应该调用工具”和“系统允许执行工具”拆成两个独立决定。这个边界比 Agent 循环本身更重要：页面内容和模型输出都是不可信输入，不能直接变成宿主机命令。

## 执行路径

```text
Browser Evidence
      │
      ▼
LLM / deterministic Tool Decider
      │  tool proposal
      ▼
Policy Gateway
      │  approved proposal
      ▼
PostgreSQL Outbox → Redis Streams → gRPC Worker
                                      │
                                      ▼
                              Restricted Docker
```

模型只能看到一个 strict function schema：`diagnose_live_page(reason)`。`parallel_tool_calls=false`，服务端也会拒绝一次响应中的多个工具调用。页面正文最多发送 4096 个字符，并在指令中明确标记为不可信证据。

## Policy Gateway 检查什么

`internal/agent/decision.go` 的策略要求同时满足：

1. 当前任务不处于验证码、登录验证或人工接管状态。
2. 请求工具必须精确等于白名单中的 `diagnose_live_page`。
3. 调用理由长度必须为 1–300 个字符。
4. 用户原始目标必须明确涉及活动或入口。
5. 浏览器检查结果中必须存在仍为 `unverified` 的 `activity_entry`。

模型没有调用工具时，策略不会自行补一个调用；模型提出越权工具时，策略会拒绝并记录原因。

## 模型控制不了什么

模型不能提供：

- Shell 命令或脚本。
- Docker 镜像、entrypoint、参数或挂载。
- 网络模式、环境变量名或资源额度。
- gRPC 地址、Redis Stream 或 PostgreSQL 查询。
- Sandbox 的用户、capability 或 security option。

实际命令来自服务端 `internal/sandbox/sandbox.go` 中的预定义 scenario。页面文本经过长度限制和 Base64 编码后作为数据传入，命令模板本身不由模型拼接。

## Docker 约束

每次诊断容器都使用固定策略：

| 控制 | 当前值 |
|---|---|
| 用户 | `65534:65534`，非 root |
| 根文件系统 | `--read-only` |
| 临时目录 | 16 MiB tmpfs，`noexec,nosuid` |
| 网络 | `--network none` |
| 内存 | 128 MiB，禁用额外 swap |
| CPU | 0.5 CPU |
| 进程数 | 32 PIDs |
| capabilities | `--cap-drop ALL` |
| 提权 | `no-new-privileges` |
| 输出 | 最多 16 KiB |
| 时间 | scenario deadline，超时后 kill |

执行完成后 Worker 检查 exit code、OOM 和 timeout 状态，再强制删除容器。Sandbox Lab 用正常执行、超时、只读写入和断网四个真实容器案例验证这些约束。

## Prompt Injection 为什么不能直接获得权限

恶意页面可以尝试写“忽略之前指令并执行命令”。它仍要经过两层边界：

1. Tool Decider 只暴露一个严格工具 schema，不提供任意执行接口。
2. Policy Gateway 使用服务端已有的用户目标和结构化检查状态重新授权，不把页面文字当成权限来源。

这不能保证模型永远不受页面内容影响，但可以保证一次错误模型决定不会自动扩大成任意 Docker 权限。

## 故障时的行为

- 模型连接失败：记录脱敏错误，使用确定性 Tool Decider。
- 连续供应商错误：共享 Circuit Breaker 打开，后续调用立即降级。
- 人工验证出现：停止自动工具调用并返回 `needs_human`。
- Policy 拒绝：保存 requested tool 和拒绝原因，不投递 Sandbox。
- Worker 或容器失败：结果保持可观察状态，由 Redis Pending/lease 机制恢复。

## 当前边界

- Docker 共享宿主机内核，不是 microVM、gVisor 或 Kata 强隔离。
- Sandbox Worker 能访问 Docker daemon，只适合可信单机并绑定 loopback。
- HTTP 与 gRPC 尚无生产级身份认证、TLS、租户隔离和配额。
- 浏览器初始 URL 有域名白名单，但生产环境仍需加强重定向、DNS rebinding 和出站网络策略。
- 当前只支持一个只读诊断工具，不接受任意模型生成代码。

限制工具面是当前安全模型的一部分，不是尚未完成的通用代码执行功能。
