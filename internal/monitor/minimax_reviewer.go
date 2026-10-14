package monitor

import (
	"context"
	"net/http"
	"strings"

	"liveguard/internal/agent"
)

// MiniMaxUnexpectedEventReviewer preserves the programmatic entrypoint used
// by existing evals. All decisions and tools run through AgentReviewer.
type MiniMaxUnexpectedEventReviewer struct {
	APIKey   string
	Model    string
	Client   *http.Client
	Endpoint string
	Breaker  *agent.CircuitBreaker
	Limits   agent.LoopLimits
}

func (r *MiniMaxUnexpectedEventReviewer) runtime() *AgentReviewer {
	model := r.Model
	if model == "" {
		model = "MiniMax-M3"
	}
	return &AgentReviewer{Model: &MessagesModel{APIKey: r.APIKey, Model: model, Client: r.Client, Endpoint: r.Endpoint, Vision: true}, Breaker: r.Breaker, Limits: r.Limits}
}
func (r *MiniMaxUnexpectedEventReviewer) Review(ctx context.Context, input UnexpectedEventInput) (UnexpectedEventDecision, error) {
	return r.runtime().Review(ctx, input)
}
func (r *MiniMaxUnexpectedEventReviewer) Capabilities() ModelCapabilities {
	return r.runtime().Capabilities()
}
func (r *MiniMaxUnexpectedEventReviewer) AdditionalToolNames() []string {
	return r.runtime().AdditionalToolNames()
}
func (r *MiniMaxUnexpectedEventReviewer) executeTool(ctx context.Context, block miniMaxContentBlock, input UnexpectedEventInput, evidence []map[string]any, state *miniMaxReviewState, turn int) (reviewToolResult, UnexpectedEventDecision, bool) {
	return r.runtime().executeTool(ctx, agent.ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input}, input, evidence, state, turn)
}

type miniMaxReviewState = reviewState

func miniMaxToolsForPlan(plan MonitoringPlan) []map[string]any { return reviewToolsForPlan(plan) }
func runMiniMaxTool(block miniMaxContentBlock, input UnexpectedEventInput, evidence []map[string]any, state *miniMaxReviewState, turn int) (reviewToolResult, UnexpectedEventDecision, bool) {
	return runReviewTool(agent.ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input}, input, evidence, state, turn)
}
func parseMiniMaxDecision(content []miniMaxContentBlock) (UnexpectedEventDecision, error) {
	var output strings.Builder
	for _, block := range content {
		if block.Type == "text" {
			output.WriteString(block.Text)
		}
	}
	return parseReviewDecision(output.String())
}
