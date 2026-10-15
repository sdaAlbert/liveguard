# 评测与验证

LiveGuard 用三类检查回答三个不同问题：功能是否按规则运行、模型是否能区分应提醒与不应提醒、系统在并发输入下会不会积压或失稳。它们的结果不能互相替代。

## 日常验证

提交代码前运行：

```powershell
go test ./...
go vet ./...
go build ./...
node --check internal/web/static/monitor.js
```

主要测试覆盖：

| 范围 | 检查内容 |
|---|---|
| Agent 循环 | 工具白名单、调用与轮数预算、超时、重复调用、终止条件、有限重试和熔断 |
| 监控服务 | 目标变更、暂停与停止、排队、过期、重复信号、并发窗口、故障恢复和调查结果提交 |
| 浏览器与识别 | Chrome 截图取消和恢复、OCR 上传路径、截图采样与队列积压处理 |
| 状态存储与网页 | 本机文件重启恢复、HTTP 接口、协作问答流程和前端脚本语法 |

这些测试主要使用固定输入、替身模型或合成网页，不代表真实模型判断质量，也不代表真实直播的漏采率。

## 模型判断评测

`cmd/monitor-eval` 使用固定文字案例，调用 MiniMax Reviewer，并与关键词基线比较。案例覆盖目标命中、条件不满足、否定表达、重复文字、OCR 噪声、模糊表达和提示注入。结果包括通过数、precision、recall、误报率、单条调查 P95、token 和工具调用数。

可通过项目脚本运行开发集：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\verify-minimax-live.ps1 -Evaluate -Split dev
```

要检查固定回归集，将 `dev` 改为 `holdout-v1`。模型评测会实际调用 API 并产生费用；改提示词或规则时先看开发集，再用回归集检查已有案例是否退步。固定文字案例只衡量这些案例上的提醒行为，不衡量直播视频采集、OCR 识别质量或最终解释是否完整。

这次提交没有重新运行付费模型评测，因此本文不报告当前模型的通过率。

## 吞吐与延迟

`cmd/monitor-load` 用固定等待 40 ms 的 mock Reviewer 测后端路径，不访问真实模型、Chrome、OCR 或数据库。同步模式测 `Service.Ingest`；异步模式测 `Service.SubmitSignal`、有界队列和后台复核。当前已有的本机结果如下：

测量环境：2026-10-15，Windows/amd64，Go 1.26.3，16 个逻辑 CPU。

| 路径 | 输入 | 结果 |
|---|---:|---|
| 同步 `Ingest` | 160 条，3 轮 | 全部接收、0 错误；P95 5–6 ms，约 1,043–1,181 条/秒 |
| 同步 `Ingest` | 800 条，3 轮 | 全部接收、0 错误；P95 30–32 ms，约 411–427 条/秒 |
| 异步 `SubmitSignal` | 160 条，3 轮 | 全部入队并处理、0 错误；约 84.9–86.6 条/秒 |
| 异步 `SubmitSignal` | 800 条，3 轮 | 全部入队并处理、0 错误；约 32.9–40.6 条/秒；排队至复核开始 P95 为 2.7–3.4 秒 |

复跑命令：

```powershell
go run ./cmd/monitor-load -requests 160
go run ./cmd/monitor-load -requests 800
go run ./cmd/monitor-load -mode async -requests 160
go run ./cmd/monitor-load -mode async -requests 800
```

负载使用 8 个 Run 和 8 个提交 worker，异步信号通过背压重试直到接收，再等队列处理完成。数字说明本机合成负载下的后端吞吐和排队情况；mock 的 40 ms 不是真实模型耗时，也不能当作端到端提醒延迟。

## 多窗口持续运行

6 分钟的本机合成稳定性检查同时运行 8 个 headless Chrome 测试页、截图和 Tesseract OCR，并用 15 秒 mock Reviewer 模拟慢复核。最近一次运行中，8 个窗口结束时均为健康状态；每个窗口截图 71–73 次，共跳过 20 帧积压。该结果说明这次测试没有复现多窗口采集停止的问题，不证明真实平台或真实登录状态下同样稳定。

复跑需要本机安装 Chrome 和 Tesseract：

```powershell
$env:LIVEGUARD_STABILITY_DURATION = "6m"
go test ./internal/monitor -run TestEightBrowserMonitorsSustained -count=1 -v
```

## 尚未覆盖

- **真实直播漏采率**：需要将采集结果与完整、带时间标注的直播或评论记录对照。
- **真实模型端到端延迟**：当前 mock 负载不含 API、浏览器、OCR、持久化和网页展示时间。
- **提醒送达时间**：后端生成提醒不等于浏览器已经收到或用户已经看到。
- **长时间存储增长**：当前没有在这份评测中测观测流水的磁盘增长速度。

因此，汇报性能时要说明测试路径、输入规模和依赖类型；不能把合成队列吞吐说成真实直播处理能力。
