package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ReplayStep is one recorded boundary crossing in a Loop. Replaying never
// invokes a model provider or an original tool handler.
type ReplayStep struct {
	Sequence   int         `json:"sequence"`
	Kind       string      `json:"kind"`
	Turn       int         `json:"turn"`
	ModelTurn  *ModelTurn  `json:"model_turn,omitempty"`
	Error      string      `json:"error,omitempty"`
	Call       *ToolCall   `json:"call,omitempty"`
	Output     *ToolOutput `json:"output,omitempty"`
	DurationMS int64       `json:"duration_ms,omitempty"`
	Rejected   bool        `json:"rejected,omitempty"`
}

// Replay runs the same loop limits and validation against recorded model turns
// and tool results, while replacing every external callback with recorded data.
func (l Loop) Replay(ctx context.Context, steps []ReplayStep) (LoopResult, error) {
	if len(steps) == 0 {
		return LoopResult{}, errors.New("replay trace has no steps")
	}
	cursor := 0
	var replayErr error
	next := func(_ context.Context, _ []Observation, turn int) (ModelTurn, error) {
		if cursor >= len(steps) {
			replayErr = errors.New("replay trace ended before the next model turn")
			return ModelTurn{}, replayErr
		}
		step := steps[cursor]
		if step.Sequence != cursor+1 || step.Kind != "model" || step.Turn != turn+1 || step.ModelTurn == nil {
			replayErr = fmt.Errorf("replay trace mismatch at step %d: expected model turn %d", cursor+1, turn+1)
			return ModelTurn{}, replayErr
		}
		cursor++
		if step.Error != "" {
			return ModelTurn{}, replayError(step.Error)
		}
		return *step.ModelTurn, nil
	}
	replayLoop := l
	replayLoop.Recorder = nil
	replayLoop.ReplayTool = func(turn int, call ToolCall, validationOutput ToolOutput, rejected bool) (ToolOutput, error) {
		if cursor >= len(steps) {
			return ToolOutput{}, fmt.Errorf("replay trace ended before tool %q", call.Name)
		}
		step := steps[cursor]
		if step.Sequence != cursor+1 || step.Kind != "tool" || step.Turn != turn || step.Call == nil || step.Output == nil || !sameToolCall(*step.Call, call) || step.Rejected != rejected {
			return ToolOutput{}, fmt.Errorf("replay trace mismatch at step %d for tool %q", cursor+1, call.Name)
		}
		if rejected && !sameRejectedOutput(validationOutput, *step.Output) {
			return ToolOutput{}, fmt.Errorf("replay validation result mismatch at step %d for tool %q", cursor+1, call.Name)
		}
		cursor++
		return *step.Output, nil
	}
	result, err := replayLoop.Run(ctx, next)
	if replayErr != nil {
		return result, replayErr
	}
	if err != nil {
		return result, err
	}
	if cursor != len(steps) {
		return result, fmt.Errorf("replay trace has %d unconsumed steps", len(steps)-cursor)
	}
	for i := range result.Observations {
		// The loop's local timing is only replay overhead; use the captured tool
		// duration so the displayed trace keeps its original timing.
		for _, step := range steps {
			if step.Kind == "tool" && step.Call != nil && sameToolCall(*step.Call, result.Observations[i].Call) {
				result.Observations[i].DurationMS = step.DurationMS
				break
			}
		}
	}
	return result, nil
}

func sameRejectedOutput(a, b ToolOutput) bool {
	if a.Outcome != b.Outcome || a.Failed != b.Failed || a.Terminal != b.Terminal {
		return false
	}
	left, leftErr := json.Marshal(a.Content)
	right, rightErr := json.Marshal(b.Content)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func sameToolCall(a, b ToolCall) bool {
	if a.ID != b.ID || a.Name != b.Name {
		return false
	}
	var left, right any
	if json.Unmarshal(a.Arguments, &left) != nil || json.Unmarshal(b.Arguments, &right) != nil {
		return string(a.Arguments) == string(b.Arguments)
	}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func replayError(message string) error {
	switch message {
	case context.Canceled.Error():
		return context.Canceled
	case context.DeadlineExceeded.Error():
		return context.DeadlineExceeded
	default:
		return errors.New(message)
	}
}
