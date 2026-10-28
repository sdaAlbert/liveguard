package agent

import (
	"context"
	"encoding/json"
	"testing"
)

type replayRecorder struct{ steps []ReplayStep }

func (r *replayRecorder) RecordModelTurn(turn int, response ModelTurn, err error) {
	step := ReplayStep{Sequence: len(r.steps) + 1, Kind: "model", Turn: turn, ModelTurn: &response}
	if err != nil {
		step.Error = err.Error()
	}
	r.steps = append(r.steps, step)
}

func (r *replayRecorder) RecordToolResult(turn int, call ToolCall, output ToolOutput, durationMS int64, rejected bool) {
	r.steps = append(r.steps, ReplayStep{Sequence: len(r.steps) + 1, Kind: "tool", Turn: turn, Call: &call, Output: &output, DurationMS: durationMS, Rejected: rejected})
}

func TestLoopReplayUsesCapturedTurnsAndDoesNotExecuteTools(t *testing.T) {
	toolCalls := 0
	loop := Loop{
		Limits: LoopLimits{MaxTurns: 3, MaxCalls: 3},
		Tools: map[string]ToolHandler{
			"read": func(context.Context, ToolCall, int) ToolOutput {
				toolCalls++
				return ToolOutput{Content: "captured evidence", Outcome: "read complete", Value: "evidence"}
			},
			"finish": func(context.Context, ToolCall, int) ToolOutput {
				toolCalls++
				return ToolOutput{Content: "accepted", Outcome: "decision accepted", Terminal: true, Value: "alert"}
			},
		},
		TerminalTools: map[string]bool{"finish": true},
	}
	recorder := &replayRecorder{}
	loop.Recorder = recorder
	original, err := loop.Run(context.Background(), func(_ context.Context, previous []Observation, turn int) (ModelTurn, error) {
		if turn == 0 {
			return ModelTurn{Calls: []ToolCall{{ID: "read-1", Name: "read", Arguments: json.RawMessage(`{"event_id":"evt-1"}`)}}}, nil
		}
		if len(previous) != 1 || previous[0].Output.Value != "evidence" {
			t.Fatalf("second model turn did not receive the recorded tool result: %#v", previous)
		}
		return ModelTurn{Calls: []ToolCall{{ID: "finish-1", Name: "finish", Arguments: json.RawMessage(`{}`)}}}, nil
	})
	if err != nil || original.Value != "alert" || original.StopReason != "terminal_tool" || toolCalls != 2 {
		t.Fatalf("original loop did not complete as expected: result=%#v err=%v toolCalls=%d", original, err, toolCalls)
	}
	if len(recorder.steps) != 4 {
		t.Fatalf("expected two model turns and two tool results, got %d steps", len(recorder.steps))
	}

	loop.Recorder = nil
	replayed, err := loop.Replay(context.Background(), recorder.steps)
	if err != nil || replayed.StopReason != original.StopReason || replayed.Value != original.Value || replayed.Turns != original.Turns {
		t.Fatalf("replay diverged: result=%#v err=%v original=%#v", replayed, err, original)
	}
	if toolCalls != 2 {
		t.Fatalf("replay executed a live tool handler: calls=%d", toolCalls)
	}
	if len(replayed.Observations) != 2 || replayed.Observations[0].Output.Content != "captured evidence" || replayed.Observations[1].Output.Value != "alert" {
		t.Fatalf("replay did not return recorded tool results: %#v", replayed.Observations)
	}
}

func TestLoopReplayPreservesAllowlistRejections(t *testing.T) {
	loop := Loop{Tools: map[string]ToolHandler{"read": func(context.Context, ToolCall, int) ToolOutput {
		t.Fatal("unexpected live tool handler call")
		return ToolOutput{}
	}}}
	recorder := &replayRecorder{}
	loop.Recorder = recorder
	original, err := loop.Run(context.Background(), func(_ context.Context, previous []Observation, turn int) (ModelTurn, error) {
		if turn == 0 {
			return ModelTurn{Calls: []ToolCall{{ID: "bad-1", Name: "shell", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		if len(previous) != 1 || !previous[0].Output.Failed {
			t.Fatalf("unknown tool was not rejected: %#v", previous)
		}
		return ModelTurn{Final: "safe"}, nil
	})
	if err != nil || original.StopReason != "finished" {
		t.Fatalf("original rejection flow failed: result=%#v err=%v", original, err)
	}
	if len(recorder.steps) != 3 || !recorder.steps[1].Rejected {
		t.Fatalf("expected the rejected tool to be captured: %#v", recorder.steps)
	}

	loop.Recorder = nil
	replayed, err := loop.Replay(context.Background(), recorder.steps)
	if err != nil || replayed.StopReason != original.StopReason || replayed.Value != original.Value || len(replayed.Observations) != 1 || !replayed.Observations[0].Output.Failed {
		t.Fatalf("replay did not preserve the allowlist rejection: result=%#v err=%v", replayed, err)
	}
}

func TestLoopReplayRejectsModifiedCallSequence(t *testing.T) {
	loop := Loop{Tools: map[string]ToolHandler{"read": func(context.Context, ToolCall, int) ToolOutput { return ToolOutput{Content: "saved"} }}}
	recorder := &replayRecorder{}
	loop.Recorder = recorder
	_, err := loop.Run(context.Background(), func(context.Context, []Observation, int) (ModelTurn, error) {
		return ModelTurn{Calls: []ToolCall{{ID: "read-1", Name: "read", Arguments: json.RawMessage(`{"id":"one"}`)}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range recorder.steps {
		if recorder.steps[i].Kind == "tool" {
			recorder.steps[i].Call.Arguments = json.RawMessage(`{"id":"changed"}`)
			break
		}
	}
	loop.Recorder = nil
	if _, err := loop.Replay(context.Background(), recorder.steps); err == nil {
		t.Fatal("replay accepted a modified tool call")
	}
}
