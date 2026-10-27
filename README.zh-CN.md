# LiveGuard

[English](README.md) | [简体中文](README.zh-CN.md)

[![CI](https://github.com/sdaAlbert/liveguard/actions/workflows/ci.yml/badge.svg)](https://github.com/sdaAlbert/liveguard/actions/workflows/ci.yml)

LiveGuard 是一个本地运行的直播监控 Agent，帮助用户不用一直盯着多个直播屏幕，也能持续关注指定内容并收到提醒。输入直播地址和关注目标后，它会采集可见文字，并在你授权后转写口播；Agent 结合当前信号和历史观测进行调查，提醒连同证据保存在本地监控室。

项目支持抖音、B 站、快手等网页直播平台，使用同一套浏览器采集、OCR 和 Agent 调查流程，不依赖单个平台的私有接口。当前主要应用和多路测试场景是 B 站。平台登录、风控和同时观看限制仍然适用，LiveGuard 不会绕过这些限制。

## 能做什么

- 使用专用 Chrome 会话截图，通过本地 OCR 读取直播页面中的可见文字。
- 可选处理用户主动分享的直播音频；当前 ASR 接入 MiniMax。
- Agent 可调查信号、检索历史观测，并在需要时检查截图。
- 默认使用本机 SQLite 保存监控状态、观测流水和可恢复的待处理信号；JSON 导出用于查看和归档。

## 运行效果

监控室汇总运行中的直播任务和待处理状态。

![LiveGuard 监控室运行状态](docs/images/monitor-room.png)

详情页展示 Agent 状态、关键证据和调查记录。

![LiveGuard 监控详情与 Agent 调查过程](docs/images/investigation-detail.png)

## 快速开始

需要 Windows 10/11、Go 1.25、Google Chrome 和 Tesseract OCR。中文识别建议安装 `chi_sim` 数据包。

```powershell
git clone https://github.com/sdaAlbert/liveguard.git
cd liveguard
powershell -ExecutionPolicy Bypass -File ./scripts/run-mvp.ps1
```

打开 <http://127.0.0.1:8080>，在专用 Chrome 窗口登录直播平台，然后新建监控。需要使用大模型时，在“模型设置”中选择提供方并配置 API Key；保存设置不会发送探测请求。

## 项目边界

LiveGuard 只能分析实际采集到的信号；采样间隔、页面可见内容和 OCR 质量都会影响覆盖范围。提醒保存在应用内，不会发送到微信、短信或其他外部渠道。最多 8 个活跃监控是调度上限，不代表任意电脑都能让 8 路直播每秒完成 OCR。当前快速启动流程面向 Windows。

## 文档

- [文档导航](docs/README.md)：阅读顺序和主题索引。
- [项目导览：从目录到一次提醒](docs/PROJECT-GUIDE.md)：目录、术语、调用流程、状态和测试地图。
- [架构与 Agent 行为](docs/ARCHITECTURE.md)：证据、工具、调度、上下文和持久化设计。
- [运行、配置与故障处理](docs/OPERATIONS.md)：启动、本机数据、限制和恢复方式。
- [评测与验证](docs/EVALUATION.md)：自动化测试、真实运行记录、合成负载及证据边界。
- [Agent Harness 详解](docs/AGENT-HARNESS.md) · [优化路线图](docs/ROADMAP.md) · [贡献指南](CONTRIBUTING.md)
- [English documentation index](docs/README.en.md)

## 更新记录

- **2026-10 — SQLite 持久化：**将运行时的 JSON 快照、JSONL 观测流水和内存 Pending 队列统一到 SQLite 事务存储，使监控状态、观测和已接收的待处理信号能一起提交，并在重启后恢复仍有效的排队工作。移除了重复的 JSON 运行仓储；JSON 导出继续用于查看和归档，但不包含 Pending 队列。

## 安全与隐私

服务默认只监听本机，不适合作为公网服务直接暴露。API Key 保存在 Go 服务进程内存或用户配置的环境变量中；不要提交密钥、登录资料或私人截图。详细说明见[运行文档](docs/OPERATIONS.md)。

## License

[MIT](LICENSE)
