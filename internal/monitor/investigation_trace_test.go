package monitor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"liveguard/internal/agent"
)

func TestSQLiteInvestigationTraceCanBeReplayedAfterReload(t *testing.T) {
	ctx := context.Background()
	repo, err := OpenSQLiteRepository(filepath.Join(t.TempDir(), "trace.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Create(ctx, &Run{ID: "trace-run", Status: RunRunning, Version: 1, StartedAt: time.Now().UTC(), Rooms: []Room{}}); err != nil {
		t.Fatal(err)
	}
	decision := UnexpectedEventDecision{Action: "alert", Category: "giveaway", Summary: "抽奖开始", Reason: "主持人已宣布", Confidence: .9}
	trace := &InvestigationTrace{
		ID: "trace-1", RunID: "trace-run", EventID: "event-1", AttemptID: "attempt-1", GoalVersion: 1,
		Status: "recording", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Limits: agent.LoopLimits{MaxTurns: 3, MaxCalls: 3},
		AllowedTools: []string{"finish"}, TerminalTools: []string{"finish"}, Steps: []agent.ReplayStep{},
	}
	if err := repo.CreateInvestigationTrace(ctx, trace); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolCall{ID: "finish-1", Name: "finish", Arguments: json.RawMessage(`{}`)}
	modelTurn := agent.ModelTurn{Calls: []agent.ToolCall{call}}
	if err := repo.AppendInvestigationTraceStep(ctx, trace.ID, agent.ReplayStep{Sequence: 1, Kind: "model", Turn: 1, ModelTurn: &modelTurn}); err != nil {
		t.Fatal(err)
	}
	toolOutput := agent.ToolOutput{Content: `{"accepted":true}`, Outcome: "accepted", Terminal: true, Value: decision}
	if err := repo.AppendInvestigationTraceStep(ctx, trace.ID, agent.ReplayStep{Sequence: 2, Kind: "tool", Turn: 1, Call: &call, Output: &toolOutput}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteInvestigationTrace(ctx, trace.ID, decision, "terminal_tool", ""); err != nil {
		t.Fatal(err)
	}

	service := NewService(repo, nil)
	replayed, err := service.ReplayInvestigation(ctx, trace.RunID, trace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Matched || replayed.StopReason != "terminal_tool" || replayed.ReplayedTurns != 1 || replayed.ReplayedTools != 1 || replayed.Decision.Summary != decision.Summary {
		t.Fatalf("persisted trace did not replay to the recorded result: %#v", replayed)
	}

	summaries, err := service.ListInvestigationTraces(ctx, trace.RunID)
	if err != nil || len(summaries) != 1 || summaries[0].ID != trace.ID || summaries[0].StepCount != 2 || summaries[0].Status != "complete" {
		t.Fatalf("trace summary was not retained: summaries=%#v err=%v", summaries, err)
	}
}
