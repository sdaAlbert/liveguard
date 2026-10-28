package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"liveguard/internal/agent"
	"liveguard/internal/domain"
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

const reviewMaxOutputTokens = 4096

func (r *AgentReviewer) Review(ctx context.Context, input UnexpectedEventInput) (decision UnexpectedEventDecision, err error) {
	started := time.Now()
	if r.Model == nil {
		return decision, errors.New("monitor model is not configured")
	}
	if len(input.RecentEvidence) == 0 {
		return decision, errors.New("review requires a triggering signal")
	}
	eventID := input.RecentEvidence[0].ID
	attemptID := strings.TrimSpace(input.AttemptID)
	if attemptID == "" {
		attemptID = domain.NewID("review")
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
			decision.StopReason = "circuit_open"
			setReviewMetadata(&decision, r.Model.ModelID(), 0, 0, started)
			return decision, err
		}
	}
	log.Printf("agent review started: review_id=%q attempt_id=%q event_id=%q model=%q goal_version=%d", eventID, attemptID, eventID, r.Model.ModelID(), input.GoalVersion)
	input = boundedReviewInput(input)
	evidence := boundedEvidence(input.RecentEvidence)
	frameAvailable := caps.Vision && (input.TriggerFrame != "" || input.CaptureFrame != nil)
	initial, _ := json.Marshal(map[string]any{
		"user_goal":          input.Goal,
		"user_goal_role":     "preference, not an exclusive filter on important unexpected events",
		"user_clarification": strings.TrimSpace(input.UserClarification),
		"user_instructions":  input.UserInstructions,
		"monitor_plan":       input.Plan, "sampling_state": input.Sampling,
		"live_baseline":                   input.Baseline,
		"historical_investigation_memory": input.RecentMemory,
		"frame_check":                     input.FrameCheck, "server_time": time.Now().UTC(),
		"frame_available": frameAvailable, "model_capabilities": caps,
		"triggering_signal": evidence[0],
	})
	definitions := r.toolsForPlan(input.Plan)
	if len(input.UserInstructions) == 0 && strings.TrimSpace(input.UserClarification) == "" {
		filtered := definitions[:0]
		for _, definition := range definitions {
			if definition["name"] != "update_monitoring_goal" {
				filtered = append(filtered, definition)
			}
		}
		definitions = filtered
	}
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
		Initial: string(initial), Tools: toolDefinitions(definitions), MaxOutputTokens: reviewMaxOutputTokens, EnsureRequiredTool: true,
	})
	state := reviewState{
		readEvidenceTurn: -1, searchAlertsTurn: -1, frameInspectTurn: -1,
		contextBudget: newReviewContextBudget(), seenEvidenceIDs: make(map[string]bool),
	}
	if eventID := input.RecentEvidence[0].ID; eventID != "" {
		state.seenEvidenceIDs[eventID] = true
	}
	defer func() {
		if state.frameAssetOwned && state.frameAsset != nil {
			_ = os.Remove(state.frameAsset.Path)
		}
	}()
	tools := map[string]agent.ToolHandler{}
	for _, definition := range definitions {
		name := definition["name"].(string)
		tools[name] = func(toolCtx context.Context, call agent.ToolCall, turn int) agent.ToolOutput {
			toolStarted := time.Now()
			log.Printf("agent tool started: review_id=%q attempt_id=%q event_id=%q turn=%d tool=%q call_id=%q", eventID, attemptID, eventID, turn+1, call.Name, call.ID)
			result, value, terminal := r.executeTool(toolCtx, call, input, evidence, &state, turn)
			log.Printf("agent tool completed: review_id=%q attempt_id=%q event_id=%q turn=%d tool=%q call_id=%q succeeded=%t duration_ms=%d terminal=%t", eventID, attemptID, eventID, turn+1, call.Name, call.ID, !result.IsError, time.Since(toolStarted).Milliseconds(), terminal && !result.IsError)
			return agent.ToolOutput{Content: result.Content, Outcome: result.Outcome, Failed: result.IsError, Terminal: terminal, Value: value}
		}
	}
	loop := agent.Loop{Limits: r.Limits, Tools: tools, TerminalTools: map[string]bool{"create_alert": true, "request_user_input": true, "update_monitoring_goal": true}}
	var inTokens, outTokens int64
	var retries int
	var priorToolObservations []agent.Observation
	model := r.Model.ModelID()
	feedback := ""
	ctx = withModelRunID(ctx, input.RunID)
	ctx = withModelCallTrace(ctx, eventID, attemptID, -1)
	result, loopErr := loop.Run(ctx, func(turnCtx context.Context, previous []agent.Observation, turn int) (agent.ModelTurn, error) {
		if turn >= compactSessionAfterTurn && feedback == "" {
			if compactor, ok := session.(modelSessionCompactor); ok {
				if memory, ready := compactInvestigationMemory(priorToolObservations); ready {
					if compactor.CompactContext(memory) {
						log.Printf("agent session context compacted: review_id=%q attempt_id=%q turn=%d prior_tool_results=%d memory_chars=%d", eventID, attemptID, turn+1, len(priorToolObservations), runeCount(memory))
					}
				}
			}
		}
		turnCtx = withModelCallTrace(turnCtx, eventID, attemptID, turn+1)
		requiredTool := ""
		if turn == 0 && frameAvailable && reviewRequiresVision(input) {
			requiredTool = "inspect_live_frame"
		}
		log.Printf("agent model turn started: review_id=%q attempt_id=%q event_id=%q turn=%d previous_tool_results=%d required_tool=%q context_budget_ms=%d", eventID, attemptID, eventID, turn+1, len(previous), requiredTool, deadlineBudget(turnCtx))
		reply, err := session.Next(turnCtx, previous, requiredTool, feedback)
		feedback = ""
		priorToolObservations = append(priorToolObservations, previous...)
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
		if state.frameAsset != nil && state.frameAsset.OCR != nil {
			decision.VisualOCREvidenceID = state.frameAsset.OCR.ID
		}
	}
	for _, observation := range result.Observations {
		decision.ToolCalls = append(decision.ToolCalls, ReviewToolCall{
			Tool: observation.Call.Name, Outcome: observation.Output.Outcome,
			Succeeded: !observation.Output.Failed, DurationMS: observation.DurationMS,
		})
	}
	decision.StopReason, decision.Turns, decision.Repairs = result.StopReason, result.Turns, result.Repairs
	decision.HistoryRecordsReused = state.historyRecordsReused
	if state.readEvidenceTurn >= 0 && decision.Action != "alert" {
		used := map[string]bool{}
		for _, event := range state.historyEvidence {
			if used[event.ID] {
				continue
			}
			used[event.ID] = true
			item := Evidence{
				GoalVersion: event.GoalVersion, RoomID: event.RoomID,
				EventID: event.ID, Source: string(event.Source), Text: boundedReviewText(event.Text, 400),
				Confidence: event.Confidence, ObservedAt: event.ObservedAt,
			}
			if item.GoalVersion == 0 {
				item.GoalVersion = input.GoalVersion
			}
			decision.UsedEvidence = append(decision.UsedEvidence, item)
		}
	}
	decision.TransportRetries = retries
	decision.AttemptID = attemptID
	setReviewMetadata(&decision, model, inTokens, outTokens, started)
	if errors.Is(loopErr, ErrModelQuotaExhausted) {
		r.quotaUnavailable.Store(true)
	}
	if r.Breaker != nil {
		// An expired or cancelled queued signal is not evidence that the model
		// provider is unhealthy. Count provider/model failures, but leave the
		// breaker unchanged when the caller's investigation context ended first.
		if loopErr != nil && result.StopReason == "model_error" && !errors.Is(loopErr, ErrModelQuotaExhausted) && !errors.Is(loopErr, context.Canceled) {
			r.Breaker.Failure()
		} else if loopErr == nil {
			r.Breaker.Success()
		}
	}
	log.Printf("agent review finished: review_id=%q attempt_id=%q event_id=%q model=%q stop_reason=%s turns=%d tools=%d history_records_reused=%d input_tokens=%d output_tokens=%d latency_ms=%d error=%q", eventID, attemptID, eventID, model, decision.StopReason, decision.Turns, len(result.Observations), decision.HistoryRecordsReused, decision.InputTokens, decision.OutputTokens, decision.LatencyMS, loopErr)
	return decision, loopErr
}

func deadlineBudget(ctx context.Context) int64 {
	if deadline, ok := ctx.Deadline(); ok {
		return max(0, time.Until(deadline).Milliseconds())
	}
	return 0
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
	if len(input.RecentEvidence) > 0 {
		input.RecentMemory = selectInvestigationMemory(input.RecentMemory, input.GoalVersion, input.RecentEvidence[0].RoomID, time.Now().UTC())
	} else {
		input.RecentMemory = nil
	}
	for i := range input.RecentEvidence {
		input.RecentEvidence[i].Text = boundedReviewText(input.RecentEvidence[i].Text, 600)
	}
	if len(input.UserInstructions) > 10 {
		input.UserInstructions = input.UserInstructions[len(input.UserInstructions)-10:]
	}
	for i := range input.UserInstructions {
		input.UserInstructions[i] = boundedReviewText(input.UserInstructions[i], 1000)
	}
	if input.Baseline != nil {
		baseline := *input.Baseline
		baseline.StreamType = boundedReviewText(baseline.StreamType, 40)
		baseline.Summary = boundedReviewText(baseline.Summary, 280)
		baseline.Reason = boundedReviewText(baseline.Reason, 160)
		baseline.VisibleRegions = boundBaselineList(baseline.VisibleRegions)
		input.Baseline = &baseline
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
