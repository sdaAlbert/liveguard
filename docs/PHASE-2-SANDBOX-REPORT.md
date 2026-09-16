# LiveGuard Phase 2：Docker Sandbox V1 验收报告

## 交付目标

这一阶段把“Agent 能调用工具”推进到“Agent 的不可信任务在受限执行环境中运行”。用户可以从控制台一键运行四项策略测试，并看到容器的退出码、耗时、输出、是否超时、是否 OOM 和是否完成清理。

## 执行边界

HTTP API 只接受 `normal`、`timeout`、`write_denied` 和 `network_denied` 四个场景名。命令和 Docker 参数保存在服务端，用户输入不会进入宿主机 Shell。每次运行使用随机容器名并执行如下固定策略：

```text
image              golang:1.26 (--pull never)
network            none
root filesystem    read-only
temporary space    /tmp tmpfs, 16 MiB, noexec, nosuid
identity           uid/gid 65534
memory             128 MiB, swap disabled above that limit
CPU                0.5
processes          32 PIDs
capabilities       drop ALL
privilege          no-new-privileges
deadline           3 or 8 seconds
output             capped at 16 KiB
```

## 真实 Docker 验收结果

验收时间：2026-09-14 21:54（Asia/Shanghai）

| 场景 | 状态 | 耗时 | 退出码 | 关键证据 | 清理 |
|---|---:|---:|---:|---|---:|
| 正常执行 | PASS | 1469 ms | 0 | `uid=65534(nobody)`、`result=PASS` | CLEAN |
| 超时终止 | PASS | 4151 ms | 137 | `timed_out=true`，任务输出停在 `long task started` | CLEAN |
| 只读拦截 | PASS | 1547 ms | 1 | `/etc/liveguard-proof: Read-only file system` | CLEAN |
| 断网拦截 | PASS | 1542 ms | 42 | `NETWORK_BLOCKED` | CLEAN |

套件结果为 **4/4**。执行后运行 `docker ps -a --filter name=liveguard-sbx-` 没有返回容器，证明清理闭环完成。

控制台截图：[`../artifacts/sandbox-lab-v1/dashboard.png`](../artifacts/sandbox-lab-v1/dashboard.png)

## 验证命令

```powershell
go vet ./...
go test ./...
go build -o .cache/liveguard.exe ./cmd/liveguard
```

上述命令全部通过。单元测试覆盖策略参数、场景白名单、输出裁剪和 32 路并发提交的 ID 唯一性。

## 验收中发现并修复的问题

第一次套件运行时，四个并发任务依赖 `UnixNano` 生成 ID。在 Windows 的实际时钟粒度下四次调用得到同一个值，map 中前三个任务被覆盖。现已改用“毫秒时间戳 + 加密随机数”的项目统一 ID，并添加 32 路并发回归测试。

这个问题说明异步系统不能把高精度时间等同于唯一性。生产设计中，任务 ID 还应由数据库唯一约束兜底；重复投递则由独立幂等键处理。

## 当前限制与下一步

V1 证明了容器生命周期和隔离策略，仍只运行固定验收命令。下一步会加入代码包上传与策略校验、镜像白名单、并发配额和排队；随后把 Sandbox Worker 拆成 gRPC 服务，并用 Redis Streams、PostgreSQL 与 OpenTelemetry补齐消息恢复、持久化租约和跨服务 Trace。
