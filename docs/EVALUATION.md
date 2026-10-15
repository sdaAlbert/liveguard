# 评测与验证

[English](EVALUATION.en.md) | 简体中文

LiveGuard 的验证包括自动化测试、运行记录和合成负载。固定文字评测的代码与案例仍在，但当前结果暂不作为模型质量结论；在评测判定链路修复并复核前，不发布准确率数字。下面的 mock 负载数据也不代表真实模型或真实直播表现。

## 自动化测试

提交代码前运行：

```powershell
go test ./...
go vet ./...
go build ./...
node --check internal/web/static/monitor.js
```

测试覆盖 Agent 工具权限与预算、重试和超时、监控状态变化、队列和幂等、故障恢复、文件重启恢复、浏览器采集取消、OCR 上传及网页接口。大部分服务测试使用固定输入和 mock Reviewer；它们验证程序规则，不验证模型判断质量。

## 真实运行中的耗时

以下数据从 2026-10-15 的 LiveGuard 运行记录汇总，使用 MiniMax-M3。它们来自非受控的真实使用，输入内容、网络和目标各不相同，因此可说明系统经历过什么耗时，不能单独用来比较模型版本。

| 阶段 | 样本 | P50 | P95 | 说明 |
|---|---:|---:|---:|---|
| 模型 HTTP 请求 | 319 次成功请求 | 3.4 秒 | 9.8 秒 | 另记录 48 次失败请求；不等于一次完整调查 |
| Agent 调查 | 53 条有有效耗时的调查记录 | 17.1 秒 | 35.0 秒 | 包含一次调查中的多轮模型请求和工具步骤 |
| 观测到后端提醒 | 6 条有完整计时的提醒 | 80.5 秒 | 115.0 秒 | 从观测时间算到后端生成提醒，不包含网页显示和用户看到提醒的时间 |

调查和提醒的耗时比单次模型请求长，是因为它们还包含排队、工具执行及多轮判断。提醒延迟目前只有 6 个有效样本，应视为初步运行数据。

## 合成负载

`cmd/monitor-load` 使用固定等待 40 ms 的 mock Reviewer。它测服务接收和队列处理能力，不调用模型、浏览器、OCR 或数据库。

| 路径 | 输入 | 结果 |
|---|---:|---|
| 同步 `Service.Ingest` | 160 条，3 轮 | 全部接收、0 错误；P95 5–6 ms，约 1,043–1,181 条/秒 |
| 同步 `Service.Ingest` | 800 条，3 轮 | 全部接收、0 错误；P95 30–32 ms，约 411–427 条/秒 |
| 异步 `Service.SubmitSignal` | 160 条，3 轮 | 全部入队并处理、0 错误；约 84.9–86.6 条/秒 |
| 异步 `Service.SubmitSignal` | 800 条，3 轮 | 全部入队并处理、0 错误；约 32.9–40.6 条/秒；排队 P95 为 2.7–3.4 秒 |

负载环境为 Windows/amd64、Go 1.26.3、16 个逻辑 CPU；使用 8 个监控 Run 和 8 个提交 worker。复跑命令：

```powershell
go run ./cmd/monitor-load -requests 160
go run ./cmd/monitor-load -requests 800
go run ./cmd/monitor-load -mode async -requests 160
go run ./cmd/monitor-load -mode async -requests 800
```

这些吞吐数只适用于 mock 后端负载。实际模型服务吞吐受模型响应时间、每条调查调用轮数和输入速率影响；现有真实运行记录不足以形成受控的端到端吞吐测试。

## 多窗口持续运行

最近一次 6 分钟稳定性测试同时运行 8 个 headless Chrome 测试页、截图和 Tesseract OCR，并用 15 秒 mock Reviewer 模拟慢复核。8 个窗口结束时均为健康状态，每个窗口截图 71–73 次，共跳过 20 帧积压；本轮没有复现多窗口采集停止。测试页是合成网页，不代表真实直播平台的采集效果。

复跑需要本机安装 Chrome 和 Tesseract：

```powershell
$env:LIVEGUARD_STABILITY_DURATION = "6m"
go test ./internal/monitor -run TestEightBrowserMonitorsSustained -count=1 -v
```

## 评测范围

- 真实运行数据已能反映模型请求、调查和少量提醒的耗时，但样本不受控，网页展示时间也未记录。
- 直播漏采率暂不量化：可靠测量需要完整源材料和人工标注，不纳入当前指标。
