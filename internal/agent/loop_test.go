package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestLoopExecutesRegisteredToolAndFeedsObservationBack(t *testing.T) {
	loop := Loop{Tools: map[string]ToolHandler{"count": func(context.Context, ToolCall, int) ToolOutput {
		return ToolOutput{Content: "3 items", Value: 3}
	}}}
	result, err := loop.Run(context.Background(), func(_ context.Context, results []Observation, turn int) (ModelTurn, error) {
		if turn == 0 {
			return ModelTurn{Calls: []ToolCall{{ID: "one", Name: "count", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		if len(results) != 1 || results[0].Output.Content != "3 items" {
			t.Fatal("model did not receive the executed tool result")
		}
		return ModelTurn{Final: results[0].Output.Value}, nil
	})
	if err != nil || result.Value != 3 || result.StopReason != "finished" {
		t.Fatalf("unexpected result: %#v %v", result, err)
	}
}

func TestLoopDefaultTurnBudgetAllowsSixTurns(t *testing.T) {
	result, err := (Loop{}).Run(context.Background(), func(_ context.Context, _ []Observation, turn int) (ModelTurn, error) {
		if turn < 5 {
			args, _ := json.Marshal(map[string]int{"turn": turn})
			return ModelTurn{Calls: []ToolCall{{ID: string(rune('a' + turn)), Name: "read", Arguments: args}}}, nil
		}
		return ModelTurn{Final: "finished on sixth turn"}, nil
	})
	if err != nil || result.Value != "finished on sixth turn" || result.Turns != 6 || result.StopReason != "finished" {
		t.Fatalf("default turn budget stopped too early: %#v %v", result, err)
	}
}

func TestLoopStopsRepeatedCallsWithEquivalentJSONArguments(t *testing.T) {
	count := 0
	loop := Loop{Tools: map[string]ToolHandler{"read": func(context.Context, ToolCall, int) ToolOutput { count++; return ToolOutput{} }}}
	result, err := loop.Run(context.Background(), func(_ context.Context, _ []Observation, turn int) (ModelTurn, error) {
		args := json.RawMessage(`{"b":2,"a":1}`)
		if turn == 1 {
			args = json.RawMessage(`{ "a": 1, "b": 2 }`)
		}
		return ModelTurn{Calls: []ToolCall{{ID: string(rune('a' + turn)), Name: "read", Arguments: args}}}, nil
	})
	if err != nil || result.StopReason != "repeated_tool" || count != 2 {
		t.Fatalf("loop failed repeat limit: %#v %v calls=%d", result, err, count)
	}
}

func TestLoopRejectsUnknownToolAndTerminalBatchBeforeExecuting(t *testing.T) {
	executions := 0
	loop := Loop{TerminalTools: map[string]bool{"write": true}, Tools: map[string]ToolHandler{"write": func(context.Context, ToolCall, int) ToolOutput {
		executions++
		return ToolOutput{Terminal: true, Value: "written"}
	}}}
	result, err := loop.Run(context.Background(), func(_ context.Context, observations []Observation, turn int) (ModelTurn, error) {
		if turn == 0 {
			return ModelTurn{Calls: []ToolCall{{ID: "bad", Name: "shell", Arguments: json.RawMessage(`{}`)}, {ID: "write", Name: "write", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		if len(observations) != 2 || !observations[0].Output.Failed || !observations[1].Output.Failed {
			t.Fatal("policy rejection was not returned to model")
		}
		return ModelTurn{Final: "ignored"}, nil
	})
	if err != nil || result.Value != "ignored" || executions != 0 {
		t.Fatalf("unsafe batch executed: %#v %v executions=%d", result, err, executions)
	}
}

func TestLoopTotalDeadlineAndCancellationReachModel(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		loop := Loop{Limits: LoopLimits{TotalTimeout: 15 * time.Millisecond}}
		result, err := loop.Run(ctx, func(ctx context.Context, _ []Observation, _ int) (ModelTurn, error) {
			if cancelled {
				cancel()
			}
			<-ctx.Done()
			return ModelTurn{}, ctx.Err()
		})
		cancel()
		if cancelled && (!errors.Is(err, context.Canceled) || result.StopReason != "cancelled") {
			t.Fatalf("cancel not propagated: %#v %v", result, err)
		}
		if !cancelled && (!errors.Is(err, context.DeadlineExceeded) || result.StopReason != "timeout") {
			t.Fatalf("total deadline not enforced: %#v %v", result, err)
		}
	}
}

func TestLoopToolDeadlinePreventsTerminalResult(t *testing.T) {
	loop := Loop{Limits: LoopLimits{ToolTimeout: 10 * time.Millisecond}, Tools: map[string]ToolHandler{"slow": func(ctx context.Context, _ ToolCall, _ int) ToolOutput {
		<-ctx.Done()
		return ToolOutput{Terminal: true, Value: "must not return"}
	}}}
	result, err := loop.Run(context.Background(), func(_ context.Context, results []Observation, turn int) (ModelTurn, error) {
		if turn == 0 {
			return ModelTurn{Calls: []ToolCall{{ID: "slow", Name: "slow", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		if !results[0].Output.Failed {
			t.Fatal("timed-out tool accepted")
		}
		return ModelTurn{Final: "wait"}, nil
	})
	if err != nil || result.Value != "wait" {
		t.Fatalf("timed-out terminal tool escaped: %#v %v", result, err)
	}
}

func TestLoopBoundsFormatRepairsWithoutInventingToolExecutions(t *testing.T) {
	result, err := (Loop{}).Run(context.Background(), func(context.Context, []Observation, int) (ModelTurn, error) {
		return ModelTurn{RetryReason: "invalid response"}, nil
	})
	if err == nil || result.Repairs != 1 || result.Turns != 2 || result.StopReason != "invalid_model_response" || len(result.Observations) != 0 {
		t.Fatalf("format repairs escaped limits or fabricated tools: %#v %v", result, err)
	}
}
