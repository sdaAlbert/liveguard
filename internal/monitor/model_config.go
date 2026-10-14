package monitor

import "strings"

// ModelConfig is resolved configuration, not Agent behavior. Unknown/custom
// model names conservatively default to text; users can declare vision explicitly.
type ModelConfig struct {
	Provider     string
	Model        string
	Endpoint     string
	APIKey       string
	Capabilities ModelCapabilities
}

type modelDefaults struct{ Model, Endpoint string }

var providerDefaults = map[string]modelDefaults{
	"minimax":  {Model: "MiniMax-M3", Endpoint: "https://api.minimaxi.com/anthropic/v1/messages"},
	"openai":   {Model: "gpt-4.1-mini", Endpoint: "https://api.openai.com/v1/chat/completions"},
	"deepseek": {Model: "deepseek-flash", Endpoint: "https://api.deepseek.com/chat/completions"},
}

func defaultVisionCapability(provider, model string) bool {
	model = strings.ToLower(model)
	return (provider == "minimax" && model == "minimax-m3") ||
		(provider == "openai" && model == "gpt-4.1-mini") ||
		(provider == "deepseek" && model == "deepseek-flash")
}

func configuredToolModel(config ModelConfig) ToolModel {
	if config.Provider == "minimax" {
		return &MessagesModel{APIKey: config.APIKey, Model: config.Model, Endpoint: config.Endpoint, Vision: config.Capabilities.Vision}
	}
	return &ChatCompletionsProvider{APIKey: config.APIKey, Model: config.Model, Endpoint: config.Endpoint, Provider: config.Provider, Vision: config.Capabilities.Vision}
}

func planWithCapabilities(plan MonitoringPlan, goal string, caps ModelCapabilities) MonitoringPlan {
	captureInterval, reviewInterval := plan.CaptureIntervalSeconds, plan.ReviewIntervalSeconds
	plan.ModelCapabilities = nil // Normalize intended signals, not the old binding.
	plan = normalizeMonitoringPlan(plan, goal)
	// Cadence may already include a Go-enforced deployment floor. Capability
	// projection must not turn that value back into a model plan enum.
	if captureInterval > 0 {
		plan.CaptureIntervalSeconds = captureInterval
	}
	if reviewInterval > 0 {
		plan.ReviewIntervalSeconds = reviewInterval
	}
	plan.ModelCapabilities = &caps
	plan.CapabilityWarning = ""
	if !caps.Vision {
		filtered := plan.Tools[:0]
		for _, name := range plan.Tools {
			if name != "inspect_live_frame" && name != "save_frame_evidence" {
				filtered = append(filtered, name)
			}
		}
		plan.Tools = filtered
		if plan.VisionPolicy == "goal_required" || plan.AlertScope == "chat" || goalRequiresFrameInspection(goal) || goalNeedsChatRegion(goal) {
			plan.CapabilityWarning = "当前模型没有图片理解能力，该画面或聊天区目标只能等待确认；请配置支持看图的模型"
		}
	}
	plan.Summary = summarizeMonitoringPlan(plan)
	return plan
}

// planForReviewer projects the stored intent onto the currently bound model.
// It never changes the user's goal or silently removes a required visual check.
func planForReviewer(plan MonitoringPlan, goal string, reviewer UnexpectedEventReviewer) MonitoringPlan {
	if reviewer == nil {
		return planWithCapabilities(plan, goal, ModelCapabilities{})
	}
	if model, ok := reviewer.(interface{ Capabilities() ModelCapabilities }); ok {
		plan = planWithCapabilities(plan, goal, model.Capabilities())
	}
	if catalog, ok := reviewer.(interface{ AdditionalToolNames() []string }); ok {
		seen := map[string]bool{}
		for _, name := range plan.Tools {
			seen[name] = true
		}
		for _, name := range catalog.AdditionalToolNames() {
			if !seen[name] {
				plan.Tools = append(plan.Tools, name)
				seen[name] = true
			}
		}
	}
	return plan
}
