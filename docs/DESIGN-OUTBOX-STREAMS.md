# 从数据库提交到 Sandbox：LiveGuard 的 Outbox 与 Redis Streams

这篇说明只讨论已经实现的异步执行链。目标不是宣称“消息绝不重复”，而是在单机演示环境中做到：任务不会因为数据库和队列之间的一次崩溃而静默丢失，重复消息不会覆盖较新的结果，失败状态可以被观察和恢复。

## 问题

如果服务先写 PostgreSQL、再向 Redis 发消息，中间存在一个崩溃窗口：

```text
INSERT sandbox_run 成功
进程崩溃
XADD 尚未执行
```

数据库里有任务，但 Worker 永远收不到它。反过来，先发消息再写数据库，会产生“消息存在但任务不存在”的问题。

## 写入路径

`internal/sandbox/durable.go` 的 `InsertOrGet` 在同一个 PostgreSQL 事务里完成两次写入：

1. 插入 `sandbox_runs`。
2. 插入 `sandbox_outbox`，`published_at` 初始为空。
3. 提交事务。

Outbox Publisher 每 500 ms 扫描未发布记录，将 `run_id` 和 Trace Context 写入 Redis Stream，成功后更新 `published_at`。

```text
HTTP / Agent
     │
     ▼
PostgreSQL transaction
  ├─ sandbox_runs
  └─ sandbox_outbox
     │
     ▼
Outbox Publisher ──XADD──> liveguard:sandbox:runs
```

如果进程在事务提交前崩溃，两条记录一起回滚；如果提交后、发布前崩溃，重启后的 Publisher 会再次扫描这条 Outbox。

## 为什么仍然可能重复

Publisher 可能已经 `XADD` 成功，却在写 `published_at` 前崩溃。重启后它会再次发布同一个 `run_id`。因此这里提供的是 **at-least-once delivery**，不是 exactly-once delivery。

LiveGuard 用三层机制吸收重复：

1. 提交时的 `idempotency_key` 有唯一约束，相同 Key 和相同输入返回原 Run；相同 Key 和不同请求哈希会被拒绝。
2. Consumer 在 PostgreSQL 中获取 45 秒 lease 并递增 `attempt`，已经处于终态的 Run 不再执行。
3. 写回结果时同时匹配 `run_id + attempt + lease_owner`。旧 Worker 的迟到结果无法覆盖新 attempt，这就是 fencing。

## Worker 崩溃如何恢复

Redis Consumer Group 读到消息后不会立即 ACK。执行完成、结果写回 PostgreSQL 后才 ACK。

如果 Worker 在执行中退出：

1. 消息留在 Pending Entries List。
2. 空闲达到 50 秒后，其他 Consumer 通过 `XAUTOCLAIM` 接管。
3. PostgreSQL lease 过期后，新 Consumer 获取新的 attempt。
4. Sandbox 重新执行，最终结果带着新的 fencing token 写回。

Trace Context 作为 Stream 字段传播；Consumer 恢复上下文后再创建 `redis.sandbox.consume` span，所以一次任务可以跨越 HTTP、Redis、gRPC 和 Docker 查看。

## 故障行为

| 故障位置 | 结果 |
|---|---|
| Run 与 Outbox 事务提交前 | 一起回滚，调用方收到失败 |
| 事务提交后、XADD 前 | Outbox 保留，Publisher 重试 |
| XADD 后、标记 published 前 | 可能重复发布，由 claim/终态检查吸收 |
| Consumer 收到消息后崩溃 | 消息保持 Pending，随后被 `XAUTOCLAIM` |
| 旧 attempt 迟到写回 | `SaveClaimed` 条件不匹配，结果被拒绝 |
| 消息缺少 `run_id` | 进入 `liveguard:sandbox:dlq` 并 ACK 原消息 |
| Redis 中的 Run 在数据库不存在 | 进入 DLQ 并 ACK 原消息 |

## 当前边界

- 当前 Outbox Publisher 按单控制面进程设计，没有使用 `FOR UPDATE SKIP LOCKED` 做多 Publisher 分片。
- 45 秒 lease 没有 heartbeat，所以工具 deadline 必须短于 lease。
- DLQ 当前处理格式错误和孤儿消息；执行失败会保持 Pending 等待重领，尚未实现最大 attempt 预算。
- 诊断工具是幂等的服务端预定义操作。若未来允许有外部副作用的工具，还需要业务幂等键或下游去重表。
- Redis Streams 解决的是当前任务调度问题。项目没有使用 Kafka，也不把两者描述成完全等价。

这些限制保留在文档里，因为可靠性设计的重点不是“没有重复”，而是清楚说明重复可能在哪里发生，以及系统如何阻止它破坏最终状态。
