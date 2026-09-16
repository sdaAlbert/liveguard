package agent

import (
	"context"
	"strings"

	"liveguard/internal/domain"
)

type ToolDecision struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	CallID string `json:"call_id,omitempty"`
}

type DecisionInput struct {
	Objective  string               `json:"objective"`
	PageTitle  string               `json:"page_title"`
	PageText   string               `json:"page_text"`
	Checks     []domain.CheckResult `json:"checks"`
	NeedsHuman bool                 `json:"needs_human"`
}

type DecisionResult struct {
	Decision *ToolDecision
	Audit    domain.DecisionAudit
}

type ToolDecider interface {
	Decide(context.Context, DecisionInput) (DecisionResult, error)
}

type DeterministicToolDecider struct{}

func (DeterministicToolDecider) Decide(_ context.Context, input DecisionInput) (DecisionResult, error) {
	decision := DecideTool(input.Objective, input.Checks, input.NeedsHuman)
	audit := domain.DecisionAudit{Source: "deterministic", PolicyReason: "not evaluated"}
	if decision != nil {
		audit.RequestedTool, audit.Reason = decision.Name, decision.Reason
	}
	return DecisionResult{Decision: decision, Audit: audit}, nil
}

type ToolPolicy struct{}

type PolicyResult struct {
	Approved bool
	Reason   string
}

func (ToolPolicy) Evaluate(input DecisionInput, decision *ToolDecision) PolicyResult {
	if decision == nil {
		return PolicyResult{Reason: "model did not request a tool"}
	}
	if input.NeedsHuman {
		return PolicyResult{Reason: "human verification blocks autonomous tools"}
	}
	if decision.Name != "diagnose_live_page" {
		return PolicyResult{Reason: "tool is not in the server allowlist"}
	}
	if strings.TrimSpace(decision.Reason) == "" || len([]rune(decision.Reason)) > 300 {
		return PolicyResult{Reason: "tool reason must contain 1-300 characters"}
	}
	wantsActivity := strings.Contains(input.Objective, "活动") || strings.Contains(input.Objective, "入口")
	if !wantsActivity {
		return PolicyResult{Reason: "objective does not authorize activity diagnosis"}
	}
	for _, check := range input.Checks {
		if check.Kind == "activity_entry" && check.Status == domain.CheckUnverified {
			return PolicyResult{Approved: true, Reason: "allowlisted diagnosis for an unverified activity check"}
		}
	}
	return PolicyResult{Reason: "browser evidence does not contain an unverified activity check"}
}

func DecideTool(objective string, checks []domain.CheckResult, needsHuman bool) *ToolDecision {
	if needsHuman {
		return nil
	}
	wantsActivity := strings.Contains(objective, "活动") || strings.Contains(objective, "入口")
	for _, check := range checks {
		if check.Kind == "activity_entry" && check.Status == domain.CheckUnverified && wantsActivity {
			return &ToolDecision{Name: "diagnose_live_page", Reason: "浏览器没有找到活动入口证据，需要在隔离环境中分析页面快照并给出配置建议"}
		}
	}
	return nil
}

func ApplyDiagnosis(checks []domain.CheckResult, output string) ([]domain.CheckResult, string) {
	activityMissing := strings.Contains(output, "activity_entry=missing")
	activityPresent := strings.Contains(output, "activity_entry=present")
	for i := range checks {
		if checks[i].Kind != "activity_entry" || checks[i].Status != domain.CheckUnverified {
			continue
		}
		switch {
		case activityMissing:
			checks[i].Status = domain.CheckFailed
			checks[i].Observed = "Sandbox 诊断确认活动入口缺失；建议检查活动组件配置和发布开关"
		case activityPresent:
			checks[i].Status = domain.CheckPassed
			checks[i].Observed = "Sandbox 诊断确认页面快照包含活动入口"
		}
	}
	if activityMissing {
		return checks, "Agent 调用隔离诊断工具后确认活动入口缺失，建议检查活动组件配置和发布开关"
	}
	return checks, "Agent 已结合浏览器证据和隔离诊断工具完成验收"
}
