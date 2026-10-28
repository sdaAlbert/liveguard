package monitor

import (
	"context"
	"fmt"
	"strings"
)

// GoalPlanner turns one user request into an inspectable, bounded monitor plan.
type GoalPlanner interface {
	Plan(context.Context, string) (MonitoringPlan, error)
}

type ruleGoalPlanner struct{}

func (ruleGoalPlanner) Plan(_ context.Context, goal string) (MonitoringPlan, error) {
	return defaultGoalPlan(goal, "本地规则"), nil
}

func defaultGoalPlan(goal, planner string) MonitoringPlan {
	visual := goalRequiresFrameInspection(goal)
	asr := goalNeedsSpeechSignals(goal)
	fast := goalNeedsFastPlan(goal)
	captureSeconds, reviewSeconds := 1, 60
	if fast {
		captureSeconds, reviewSeconds = 1, 15
	}
	if visual && !fast {
		reviewSeconds = 30
	}
	tools := []string{"search_observations", "search_recent_alerts", "create_alert", "inspect_live_frame"}
	visionPolicy := "agent_decides"
	signals := []string{"ocr"}
	if visual {
		visionPolicy = "goal_required"
		signals = append(signals, "vision")
	}
	strategy := "持续读取直播字幕；有新信号时按需判断，并检查重复提醒。"
	if asr {
		strategy = "建议同时开启口播识别；把语音转写与字幕合并为证据，再决定是否提醒。"
	}
	if fast {
		strategy += " 目标强调及时性，采用较快的采样和审查节奏。"
	}
	if visual {
		strategy += " 目标依赖画面属性，必须先检查当前画面再作结论。"
	}
	consent := []string{}
	alertScope := "any"
	if goalNeedsChatRegion(goal) {
		alertScope = "chat"
	}
	if asr {
		consent = append(consent, "需要你在监控卡片手动开启口播识别并授权分享音频")
	}
	return MonitoringPlan{
		Summary: strategy, CaptureIntervalSeconds: captureSeconds, ReviewIntervalSeconds: reviewSeconds,
		UseOCR: true, RecommendASR: asr, VisionPolicy: visionPolicy, Tools: tools,
		PresenceTerms: presenceTermsFromGoal(goal), UserConsent: consent, Planner: planner,
		Signals:    signals,
		AlertScope: alertScope,
	}
}

func normalizeMonitoringPlan(plan MonitoringPlan, goal string) MonitoringPlan {
	if plan.Summary == "" {
		fallback := defaultGoalPlan(goal, "本地规则兜底")
		plan.Summary = fallback.Summary
	}
	if runes := []rune(plan.Summary); len(runes) > 280 {
		plan.Summary = string(runes[:280])
	}
	plan.PresenceTerms = normalizePresenceTerms(plan.PresenceTerms, goal)
	if len(plan.PresenceTerms) == 0 {
		plan.PresenceTerms = presenceTermsFromGoal(goal)
	}
	// OCR coverage is intentionally independent from Agent review cadence.
	// The local text pass runs at one-second intervals unless deployment config
	// explicitly imposes a slower minimum.
	plan.CaptureIntervalSeconds = 1
	if plan.ReviewIntervalSeconds != 15 && plan.ReviewIntervalSeconds != 30 && plan.ReviewIntervalSeconds != 60 {
		plan.ReviewIntervalSeconds = 60
	}
	plan.UseOCR = true // live browser monitoring currently has a local OCR signal source
	if goalNeedsSpeechSignals(goal) {
		plan.RecommendASR = true
	}
	if goalNeedsFastPlan(goal) {
		plan.CaptureIntervalSeconds = 1
		plan.ReviewIntervalSeconds = 15
	}
	if plan.VisionPolicy != "goal_required" && plan.VisionPolicy != "agent_decides" {
		plan.VisionPolicy = "agent_decides"
	}
	if goalRequiresFrameInspection(goal) {
		plan.VisionPolicy = "goal_required"
	} else if plan.VisionPolicy == "goal_required" && !periodicVision(plan) {
		// A model cannot silently turn a spoken-content request into mandatory
		// image uploads; explicit visual intent is the only required-image case.
		plan.VisionPolicy = "agent_decides"
	}
	signals := []string{"ocr"}
	if goalRequiresFrameInspection(goal) || periodicVision(plan) {
		signals = append(signals, "vision")
	}
	plan.Signals = signals
	if plan.AlertScope != "chat" {
		plan.AlertScope = "any"
	}
	if goalNeedsChatRegion(goal) {
		plan.AlertScope = "chat"
	}
	if goalIsExplicitTextOnly(goal) {
		plan.Signals = []string{"ocr"}
		plan.VisionPolicy = "agent_decides"
	}
	allowed := map[string]bool{"search_observations": true, "search_recent_alerts": true, "create_alert": true, "inspect_live_frame": true}
	seen := map[string]bool{}
	tools := make([]string, 0, len(plan.Tools)+4)
	for _, name := range plan.Tools {
		if allowed[name] && !seen[name] {
			tools = append(tools, name)
			seen[name] = true
		}
	}
	for _, name := range []string{"search_observations", "search_recent_alerts", "create_alert"} {
		if !seen[name] {
			tools = append(tools, name)
			seen[name] = true
		}
	}
	if !seen["inspect_live_frame"] {
		tools = append(tools, "inspect_live_frame")
	}
	if plan.RecommendASR {
		if len(plan.UserConsent) == 0 {
			plan.UserConsent = []string{"需要你在监控卡片手动开启口播识别并授权分享音频"}
		}
	}
	plan.Tools = tools
	plan.UserConsent = compactStrings(plan.UserConsent)
	if plan.Planner == "" {
		plan.Planner = "本地规则兜底"
	}
	plan.Summary = summarizeMonitoringPlan(plan)
	return plan
}

func summarizeMonitoringPlan(plan MonitoringPlan) string {
	parts := []string{fmt.Sprintf("本地 OCR 每 %d 秒采样", plan.CaptureIntervalSeconds)}
	parts = append(parts, fmt.Sprintf("有新信号时最短每 %d 秒交给 Agent 复核", plan.ReviewIntervalSeconds))
	if len(plan.PresenceTerms) > 0 {
		parts = append(parts, fmt.Sprintf("跟踪 %q 在可见 OCR 画面中的出现与消失", strings.Join(plan.PresenceTerms, "、")))
	}
	if plan.RecommendASR {
		parts = append(parts, "建议你授权开启口播识别")
	} else {
		parts = append(parts, "当前计划以画面文字为主")
	}
	if plan.ModelCapabilities != nil && !plan.ModelCapabilities.Vision {
		parts = append(parts, "当前模型仅处理文字，不执行看图")
	} else if plan.VisionPolicy == "goal_required" {
		parts = append(parts, "目标依赖画面属性，先调用看图工具")
	} else {
		parts = append(parts, "Agent 可按需调用看图工具")
	}
	if periodicVision(plan) {
		parts = append(parts, "无新文字时也会定时检查画面；短暂事件可能落在采样间隔内")
	}
	if plan.CapabilityWarning != "" {
		parts = append(parts, plan.CapabilityWarning)
	}
	return strings.Join(parts, "；")
}

func goalNeedsSpeechSignals(goal string) bool {
	goal = strings.ToLower(goal)
	return containsAny(goal, []string{"口播", "主播说", "说出", "说的", "提到", "宣布", "播报", "新闻", "对话", "声音", "语音", "每条", "每一条", "讲到", "asr", "speech", "spoken"})
}

func goalIsExplicitTextOnly(goal string) bool {
	goal = strings.ToLower(goal)
	return containsAny(goal, []string{"字幕", "文字", "聊天区", "评论", "口播", "主播说", "说出", "说的", "提到", "播报"}) && !containsAny(goal, []string{"画面里", "画面中", "查看画面", "看一下画面", "图像", "看图", "视觉", "看到", "颜色", "外观", "展示", "包装"})
}

func goalNeedsChatRegion(goal string) bool {
	return containsAny(strings.ToLower(goal), []string{"聊天", "弹幕", "评论", "有人问", "观众问", "网友问", "chat", "comment"})
}

func goalNeedsFastPlan(goal string) bool {
	goal = strings.ToLower(goal)
	return containsAny(goal, []string{"实时", "马上", "立刻", "一出现", "每次", "每条", "每一条", "不要错过", "尽快", "immediately", "every time", "every item", "real-time"})
}

func compactStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
