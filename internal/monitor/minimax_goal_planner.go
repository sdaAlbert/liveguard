package monitor

import (
	"context"
	"net/http"
)

// MiniMaxGoalPlanner keeps existing eval/script callers on the shared planner.
type MiniMaxGoalPlanner struct {
	APIKey   string
	Model    string
	Endpoint string
	Client   *http.Client
}

func (p *MiniMaxGoalPlanner) Plan(ctx context.Context, goal string) (MonitoringPlan, error) {
	model := p.Model
	if model == "" {
		model = "MiniMax-M3"
	}
	planner := AgentGoalPlanner{Model: &MessagesModel{APIKey: p.APIKey, Model: model, Endpoint: p.Endpoint, Client: p.Client, Vision: true}}
	return planner.Plan(ctx, goal)
}
