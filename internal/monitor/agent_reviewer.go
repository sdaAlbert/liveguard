package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"liveguard/internal/agent"
)

// AgentReviewer runs one investigation identically for every ToolModel.
// Credentials and wire formats never participate in business decisions here.
type AgentReviewer struct {
	Model            ToolModel
	Breaker          *agent.CircuitBreaker
	Limits           agent.LoopLimits
	quotaUnavailable atomic.Bool
}

func (r *AgentReviewer) Capabilities() ModelCapabilities {
	if r.Model == nil {
		return ModelCapabilities{}
	}
	return r.Model.Capabilities()
}

func (r *AgentReviewer) QuotaExhausted() bool { return r.quotaUnavailable.Load() }

const reviewFormatCorrection = "Server format correction: return a JSON decision with action wait or ignore, category, reason and confidence when no alert is warranted. To alert, satisfy evidence/duplicate prerequisites then call create_alert alone. If and only if the user must define a preference or rule, call request_user_input alone; do not return ask_user as JSON. Never create an alert through a final response. This repair uses the existing turn budget."

func (r *AgentReviewer) Review(ctx context.Context, input UnexpectedEventInput) (decision UnexpectedEventDecision, err error) {
	started := time.Now()
	if r.Model == nil {
		return decision, errors.New("monitor model is not configured")
	}
	if len(input.RecentEvidence) == 0 {
		return decision, errors.New("review requires a triggering signal")
	}
	if r.QuotaExhausted() {
		return UnexpectedEventDecision{Model: r.Model.ModelID()}, ErrModelQuotaExhausted
	}
	caps := r.Capabilities()
	if !caps.ToolCalling {
		return decision, errors.New("configured model must support tool calling")
	}
	if reviewRequiresVision(input) && !caps.Vision {
		return UnexpectedEventDecision{Action: "wait", Category: "other", Reason: "当前模型未配置图片理解能力；请切换支持看图的模型后确认画面或聊天区目标", Model: r.Model.ModelID(), StopReason: "finished"}, nil
	}
	if r.Breaker != nil {
		if err := r.Breaker.BeforeRequest(); err != nil {
			return decision, err
		}
	}
	input = boundedReviewInput(input)
	evidence := boundedEvidence(input.RecentEvidence)
	frameAvailable := caps.Vision && (input.TriggerFrame != "" || input.CaptureFrame != nil)
	initial, _ := json.Marshal(map[string]any{
		"user_goal":          input.Goal,
		"user_goal_role":     "preference, not an exclusive filter on important unexpected events",
		"user_clarification": strings.TrimSpace(input.UserClarification),
		"monitor_plan":       input.Plan, "sampling_state": input.Sampling,
		"frame_check": input.FrameCheck, "server_time": time.Now().UTC(),
		"frame_available": frameAvailable, "model_capabilities": caps,
		"triggering_signal": evidence[0],
	})
	definitions := r.toolsForPlan(input.Plan)
	if !userClarificationAllowed(input) {
		filtered := definitions[:0]
		for _, definition := range definitions {
			if definition["name"] != "request_user_input" {
				filtered = append(filtered, definition)
			}
		}
		definitions = filtered
	}
	session := r.Model.NewSession(ModelSessionConfig{
		System:  reviewInstructions(),
		Initial: string(initial), Tools: toolDefinitions(definitions), MaxOutputTokens: 512, EnsureRequiredTool: true,
	})
	state := reviewState{readEvidenceTurn: -1, searchAlertsTurn: -1, frameInspectTurn: -1}
	defer func() {
		if state.frameAssetOwned && state.frameAsset != nil {
			_ = os.Remove(state.frameAsset.Path)
		}
	}()
	tools := map[string]agent.ToolHandler{}
	for _, definition := range definitions {
		name := definition["name"].(string)
		tools[name] = func(toolCtx context.Context, call agent.ToolCall, turn int) agent.ToolOutput {
			result, value, terminal := r.executeTool(toolCtx, call, input, evidence, &state, turn)
			return agent.ToolOutput{Content: result.Content, Outcome: result.Outcome, Failed: result.IsError, Terminal: terminal, Value: value}
		}
	}
	loop := agent.Loop{Limits: r.Limits, Tools: tools, TerminalTools: map[string]bool{"create_alert": true, "request_user_input": true}}
	var inTokens, outTokens int64
	var retries int
	model := r.Model.ModelID()
	feedback := ""
	result, loopErr := loop.Run(ctx, func(turnCtx context.Context, previous []agent.Observation, turn int) (agent.ModelTurn, error) {
		requiredTool := ""
		if turn == 0 && frameAvailable && reviewRequiresVision(input) {
			requiredTool = "inspect_live_frame"
		}
		reply, err := session.Next(turnCtx, previous, requiredTool, feedback)
		feedback = ""
		retries += reply.TransportRetries
		inTokens += reply.InputTokens
		outTokens += reply.OutputTokens
		if reply.Model != "" {
			model = reply.Model
		}
		if err != nil {
			return agent.ModelTurn{}, err
		}
		if len(reply.Calls) == 0 {
			value, err := parseReviewDecision(reply.Text)
			if err != nil || value.FormatRejected || value.Action == "alert" || value.Action == "ask_user" {
				feedback = reviewFormatCorrection
				return agent.ModelTurn{RetryReason: "invalid terminal decision"}, nil
			}
			return agent.ModelTurn{Final: value}, nil
		}
		return agent.ModelTurn{Calls: reply.Calls}, nil
	})
	if value, ok := result.Value.(UnexpectedEventDecision); ok {
		decision = value
	} else {
		decision = UnexpectedEventDecision{Action: "wait", Category: "other", Reason: "调查达到运行约束，等待下一条证据"}
	}
	if state.frameInspected {
		decision.VisualObservedAt = state.frameObservedAt
		decision.VisualEvidenceID, decision.VisualArchiveID = state.frameEventID, state.frameArchiveID
	}
	for _, observation := range result.Observations {
		decision.ToolCalls = append(decision.ToolCalls, ReviewToolCall{
			Tool: observation.Call.Name, Outcome: observation.Output.Outcome,
			Succeeded: !observation.Output.Failed, DurationMS: observation.DurationMS,
		})
	}
	decision.StopReason, decision.Turns, decision.Repairs = result.StopReason, result.Turns, result.Repairs
	if state.readEvidenceTurn >= 0 && decision.Action != "alert" {
		used := map[string]bool{}
		for _, event := range state.historyEvidence {
			if used[event.ID] {
				continue
			}
			used[event.ID] = true
			decision.UsedEvidence = append(decision.UsedEvidence, Evidence{
				EventID: event.ID, Source: string(event.Source), Text: boundedReviewText(event.Text, 400),
				Confidence: event.Confidence, ObservedAt: event.ObservedAt,
			})
		}
	}
	decision.TransportRetries = retries
	setReviewMetadata(&decision, model, inTokens, outTokens, started)
	if errors.Is(loopErr, ErrModelQuotaExhausted) {
		r.quotaUnavailable.Store(true)
	}
	if r.Breaker != nil {
		if loopErr != nil && !errors.Is(loopErr, context.Canceled) {
			r.Breaker.Failure()
		} else if loopErr == nil {
			r.Breaker.Success()
		}
	}
	return decision, loopErr
}

func userClarificationAllowed(input UnexpectedEventInput) bool {
	if strings.TrimSpace(input.UserClarification) != "" || len(input.RecentEvidence) == 0 {
		return false
	}
	event := input.RecentEvidence[0]
	if time.Until(signalDeadline(event)) <= 30*time.Second {
		return false
	}
	return !containsAny(strings.ToLower(event.Text), []string{"秒杀", "秒没", "抢", "限量", "手慢无", "马上开抢", "限时抢", "限时优惠", "即将结束", "福袋开启", "倒计时"})
}

func boundedReviewInput(input UnexpectedEventInput) UnexpectedEventInput {
	input.RecentEvidence = append([]SignalEvent(nil), input.RecentEvidence[:min(len(input.RecentEvidence), 6)]...)
	for i := range input.RecentEvidence {
		input.RecentEvidence[i].Text = boundedReviewText(input.RecentEvidence[i].Text, 600)
	}
	// RecentAlerts remains local Go state. Only search_recent_alerts can
	// expose at most five summaries; no protocol receives the full history.
	return input
}

func boundedReviewText(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
