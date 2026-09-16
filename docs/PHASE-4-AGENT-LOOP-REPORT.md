# LiveGuard Phase 4：Agent Tool Loop 验收报告

## 产品行为

用户只需要描述目标：“确认直播页面的活动入口已经开启；证据不足时调用诊断工具并给出修复建议。”

系统实际执行：

```text
自然语言目标
  → Planner 生成 6 项检查
  → Chrome 读取页面并截图
  → 活动入口检查得到 unverified
  → Agent 选择 diagnose_live_page
  → PostgreSQL 创建带幂等键的 Tool Run
  → Redis Stream 投递
  → Sandbox Worker 在受限 Docker 容器中分析页面快照
  → 工具返回 activity_entry=missing
  → Agent 将检查更新为 failed，并生成修复建议
```

## 真实验收结果

验收任务：`task_1789396073485_aac239df`

工具 Run：`sbx_1789396074850_46d88541`

事件顺序：

```text
created → planning → plan → browser → evidence
→ agent_decision → tool_queued → tool_result
→ agent_synthesis → report
```

工具输出：

```text
tool=diagnose_live_page
activity_entry=missing
recommendation=check_activity_component_and_release_switch
live_status=present
diagnosis_complete
```

最终业务结论：

```text
活动入口检查：failed
Agent 调用隔离诊断工具后确认活动入口缺失，
建议检查活动组件配置和发布开关。
```

工具执行状态为 `passed`，说明诊断程序本身正常完成；业务检查为 `failed`，说明它发现页面不满足上线要求。这两个状态被分开记录，避免把“工具运行成功”错误理解成“业务验收通过”。

容器 `cleaned=true`，执行后无同名容器残留；Redis Consumer Group `XPENDING=0`。

可视化任务报告：[`../artifacts/phase-4-agent-loop/agent-loop.png`](../artifacts/phase-4-agent-loop/agent-loop.png)

## 为什么它不只是固定流程

页面已经提供明确活动入口时，Agent 直接使用浏览器证据完成报告，不调用 Sandbox。页面缺少证据时，Agent 才选择诊断工具；遇到验证码时则停止并交给人工。工具选择由观察结果决定，同一个目标会走不同路径。

目前是一轮受约束 Tool Loop。下一步可让 Agent 根据第一次工具结果选择配置检查、日志查询或回归复测，形成多轮循环，并加入最大步数、Token/时间预算和终止条件。
