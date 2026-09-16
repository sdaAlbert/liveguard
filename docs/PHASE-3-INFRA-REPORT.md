# LiveGuard Phase 3：PostgreSQL + Redis Streams 验收报告

## 交付内容

Sandbox 执行从进程内 goroutine 升级为可恢复的异步任务链：

```text
HTTP API
  │  INSERT run + unique idempotency key
  ▼
PostgreSQL ── XADD run_id ──> Redis Stream
                                 │ Consumer Group / XREADGROUP
                                 ▼
                           Sandbox Worker
                                 │ Docker lifecycle
                                 ▼
PostgreSQL <── result + evidence ─┘
                   Redis XACK
```

PostgreSQL 保存 Run 全量状态和审计证据。Redis Streams 保存待执行消息，Consumer Group 跟踪已投递但未确认的 Pending Entry。Worker 完成数据库写入后才 `XACK`。

## 验收结果

验收时间：2026-09-14 22:06—22:12（Asia/Shanghai）

| 能力 | 验收方法 | 结果 |
|---|---|---|
| 基础设施健康 | Compose healthcheck | PostgreSQL healthy；Redis healthy |
| 异步执行 | API 提交 4 项套件，由 Stream Worker 消费 | 4/4 PASS |
| 幂等提交 | 相同 `Idempotency-Key` 连续提交两次 | 两次返回完全相同的 4 个 Run ID；数据库只有 4 条 |
| 持久化 | 完成后停止并重启 LiveGuard | PostgreSQL 恢复 4/4 PASS 记录 |
| 崩溃重领 | 任务运行中强杀 Worker | 新 Consumer 使用 `XAUTOCLAIM` 重领同一消息 |
| 孤儿清理 | 强杀时保留正在运行的 Docker 容器 | 重启后删除孤儿、重新执行并得到 PASS |
| 消费确认 | 恢复完成后检查 Consumer Group | `XPENDING = 0` |

崩溃恢复任务证据：

```text
Run ID: sbx_1789394962893_38fba691
before restart: orphan container Up
after restart:  status=passed, timed_out=true, exit_code=137, cleaned=true
orphan container remaining: false
```

控制台截图：[`../artifacts/phase-3-infra/dashboard.png`](../artifacts/phase-3-infra/dashboard.png)

## 一致性设计

API 使用 PostgreSQL 唯一索引约束 `idempotency_key`。同一业务请求重复到达时读取已有 Run，不再向 Redis 重复投递。消息处理采用 at-least-once 语义：Worker 先持久化结果，再确认消息；如果进程提前退出，消息保留在 Pending Entries List 中，由其他 Consumer 在可见性超时后重领。

Docker 容器名由 Run ID 确定。Worker 在创建容器时发现同名孤儿，会先强制清理再执行同一个 Run，因此进程崩溃不会永久泄漏容器或卡死消息。

## 运行方式

```powershell
docker compose up -d --wait
$env:LIVEGUARD_INFRA_MODE="durable"
go run ./cmd/liveguard
```

打开 `http://127.0.0.1:8080`。Sandbox Lab 顶部会显示：

```text
POSTGRES HEALTHY · REDIS HEALTHY · PENDING 0 · N RUNS
```

## 下一阶段

下一步把 Sandbox Worker 从控制面进程中拆出，通过 Kitex 或 gRPC 定义带 deadline、错误码和 trace context 的 RPC 边界；随后接入 OpenTelemetry，把 HTTP、PostgreSQL、Redis、RPC 和 Docker 执行串成一条 Trace。浏览器任务存储也迁移到 PostgreSQL，并实现按检查项 checkpoint。
