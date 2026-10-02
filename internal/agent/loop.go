package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Loop owns execution limits; adapters own model protocols and business policy.
// No provider-specific messages, monitor state, or shell commands live here.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type ToolOutput struct {
	Content  any
	Outcome  string
	Failed   bool
	Terminal bool
	Value    any
}

type Observation struct {
	Call       ToolCall
	Output     ToolOutput
	DurationMS int64
}

type ModelTurn struct {
	Calls       []ToolCall
	Final       any
	RetryReason string // adapter supplies correction feedback for the next turn
}

type ToolHandler func(context.Context, ToolCall, int) ToolOutput
type NextTurn func(context.Context, []Observation, int) (ModelTurn, error)

type LoopLimits struct {
	MaxTurns         int
	MaxCalls         int
	MaxRepeatedCalls int
	TotalTimeout     time.Duration
	ToolTimeout      time.Duration
}

type LoopResult struct {
	Value        any
	Observations []Observation
	Turns        int
	StopReason   string
	Repairs      int
}

type Loop struct {
	Limits        LoopLimits
	Tools         map[string]ToolHandler
	TerminalTools map[string]bool
}

func (l Loop) Run(parent context.Context, next NextTurn) (result LoopResult, err error) {
	limits := l.Limits
	if limits.MaxTurns <= 0 {
		limits.MaxTurns = 4
	}
	if limits.MaxCalls <= 0 {
		limits.MaxCalls = 8
	}
	if limits.MaxRepeatedCalls <= 0 {
		limits.MaxRepeatedCalls = 2
	}
	if limits.TotalTimeout <= 0 {
		limits.TotalTimeout = 35 * time.Second
	}
	if limits.ToolTimeout <= 0 {
		limits.ToolTimeout = 12 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, limits.TotalTimeout)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
			result.Value = nil
			result.StopReason = "timeout"
			if parent.Err() != nil {
				result.StopReason = "cancelled"
			}
		}
	}()
	seen := map[string]int{}
	ids := map[string]bool{}
	var previous []Observation
	for turn := 0; turn < limits.MaxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Turns++
		response, err := next(ctx, previous, turn)
		if err != nil {
			result.StopReason = "model_error"
			return result, err
		}
		if response.RetryReason != "" {
			if result.Repairs >= 1 {
				result.StopReason = "invalid_model_response"
				return result, errors.New("model output remained invalid after one format repair")
			}
			result.Repairs++
			previous = nil
			continue
		}
		if len(response.Calls) == 0 {
			if response.Final == nil {
				result.StopReason = "invalid_final"
				return result, errors.New("model returned no tools or final result")
			}
			result.Value, result.StopReason = response.Final, "finished"
			return result, nil
		}
		previous = nil
		for _, call := range response.Calls {
			if len(result.Observations) >= limits.MaxCalls {
				result.StopReason = "call_budget"
				return result, nil
			}
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			var output ToolOutput
			started := time.Now()
			var args any
			validArgs := len(call.Arguments) <= 8192 && json.Unmarshal(call.Arguments, &args) == nil
			canonical, _ := json.Marshal(args)
			fingerprint := call.Name + ":" + string(canonical)
			seen[fingerprint]++
			if seen[fingerprint] > limits.MaxRepeatedCalls {
				result.StopReason = "repeated_tool"
				return result, nil
			}
			handler, allowed := l.Tools[call.Name]
			switch {
			case call.ID == "" || ids[call.ID]:
				output = rejectedTool("invalid or repeated tool call ID")
			case !validArgs:
				output = rejectedTool("invalid or oversized JSON arguments")
			case !allowed:
				output = rejectedTool("tool is not in the server allowlist")
			case l.TerminalTools[call.Name] && len(response.Calls) != 1:
				output = rejectedTool("terminal tool must be called alone")
			default:
				toolCtx, cancelTool := context.WithTimeout(ctx, limits.ToolTimeout)
				output = handler(toolCtx, call, turn)
				if toolCtx.Err() != nil {
					output = rejectedTool("tool cancelled or timed out")
				}
				cancelTool()
			}
			ids[call.ID] = true
			observation := Observation{Call: call, Output: output, DurationMS: time.Since(started).Milliseconds()}
			previous = append(previous, observation)
			result.Observations = append(result.Observations, observation)
			if output.Terminal && !output.Failed {
				// Side-effect tools must be a separate final choice, after read
				// results have been delivered to the model.
				if len(response.Calls) != 1 {
					result.StopReason = "invalid_terminal_batch"
					return result, errors.New("terminal tool must be called alone")
				}
				result.Value, result.StopReason = output.Value, "terminal_tool"
				return result, nil
			}
		}
	}
	result.StopReason = "turn_budget"
	return result, nil
}

func rejectedTool(message string) ToolOutput {
	encoded, _ := json.Marshal(map[string]string{"error": message})
	return ToolOutput{Content: string(encoded), Outcome: fmt.Sprintf("工具请求被拒绝：%s", message), Failed: true}
}
