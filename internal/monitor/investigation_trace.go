package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"liveguard/internal/agent"
	"liveguard/internal/domain"
)

const maxTraceContentBytes = 48 << 10
const maxInvestigationTracesPerRun = 20

type InvestigationTraceRepository interface {
	CreateInvestigationTrace(context.Context, *InvestigationTrace) error
	AppendInvestigationTraceStep(context.Context, string, agent.ReplayStep) error
	CompleteInvestigationTrace(context.Context, string, UnexpectedEventDecision, string, string) error
	GetInvestigationTrace(context.Context, string, string) (*InvestigationTrace, error)
	ListInvestigationTraces(context.Context, string) ([]InvestigationTraceSummary, error)
}

type InvestigationTrace struct {
	ID            string                  `json:"id"`
	RunID         string                  `json:"run_id"`
	EventID       string                  `json:"event_id"`
	AttemptID     string                  `json:"attempt_id"`
	GoalVersion   uint64                  `json:"goal_version"`
	Status        string                  `json:"status"`
	StartedAt     time.Time               `json:"started_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	CompletedAt   time.Time               `json:"completed_at,omitempty"`
	Steps         []agent.ReplayStep      `json:"steps"`
	AllowedTools  []string                `json:"allowed_tools"`
	TerminalTools []string                `json:"terminal_tools"`
	Decision      UnexpectedEventDecision `json:"decision"`
	StopReason    string                  `json:"stop_reason,omitempty"`
	Error         string                  `json:"error,omitempty"`
	Limits        agent.LoopLimits        `json:"limits"`
}

type InvestigationTraceSummary struct {
	ID          string    `json:"id"`
	EventID     string    `json:"event_id"`
	AttemptID   string    `json:"attempt_id"`
	Status      string    `json:"status"`
	StepCount   int       `json:"step_count"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Action      string    `json:"action,omitempty"`
	StopReason  string    `json:"stop_reason,omitempty"`
}

type InvestigationReplay struct {
	TraceID       string                  `json:"trace_id"`
	Matched       bool                    `json:"matched"`
	ReplayedTurns int                     `json:"replayed_turns"`
	ReplayedTools int                     `json:"replayed_tools"`
	StopReason    string                  `json:"stop_reason"`
	Decision      UnexpectedEventDecision `json:"decision"`
	Error         string                  `json:"error,omitempty"`
}

type investigationTraceRecorder struct {
	store            InvestigationTraceRepository
	trace            InvestigationTrace
	persistedSteps   int
	persistenceError string
}

func beginInvestigationTrace(store InvestigationTraceRepository, input UnexpectedEventInput, eventID, attemptID string, limits agent.LoopLimits, tools map[string]agent.ToolHandler, terminal map[string]bool) (*investigationTraceRecorder, error) {
	if store == nil {
		return nil, nil
	}
	now := time.Now().UTC()
	trace := InvestigationTrace{ID: domain.NewID("trace"), RunID: input.RunID, EventID: eventID, AttemptID: attemptID, GoalVersion: input.GoalVersion, Status: "recording", StartedAt: now, UpdatedAt: now, Steps: []agent.ReplayStep{}, AllowedTools: sortedToolNames(tools), TerminalTools: sortedTerminalNames(terminal), Limits: limits}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.CreateInvestigationTrace(ctx, &trace); err != nil {
		return nil, err
	}
	return &investigationTraceRecorder{store: store, trace: trace}, nil
}

func (r *investigationTraceRecorder) TraceID() string {
	if r == nil {
		return ""
	}
	return r.trace.ID
}

func (r *investigationTraceRecorder) RecordModelTurn(turn int, response agent.ModelTurn, err error) {
	step := agent.ReplayStep{Kind: "model", Turn: turn, ModelTurn: &response}
	if err != nil {
		step.Error = err.Error()
	}
	r.append(step)
}

func (r *investigationTraceRecorder) RecordToolResult(turn int, call agent.ToolCall, output agent.ToolOutput, durationMS int64, rejected bool) {
	step := agent.ReplayStep{Kind: "tool", Turn: turn, Call: &call, Output: &output, DurationMS: durationMS, Rejected: rejected}
	r.append(step)
}

func sortedToolNames(tools map[string]agent.ToolHandler) []string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedTerminalNames(terminal map[string]bool) []string {
	var names []string
	for name, enabled := range terminal {
		if enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (r *investigationTraceRecorder) append(step agent.ReplayStep) {
	if r == nil || r.persistenceError != "" {
		return
	}
	step = sanitizeReplayStep(step)
	step.Sequence = r.persistedSteps + 1
	r.trace.UpdatedAt = time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.store.AppendInvestigationTraceStep(ctx, r.trace.ID, step); err != nil {
		r.persistenceError = err.Error()
		log.Printf("append investigation trace failed: trace=%s run=%s step=%d error=%q", r.trace.ID, r.trace.RunID, step.Sequence, err)
		return
	}
	r.persistedSteps++
	r.trace.Steps = append(r.trace.Steps, step)
}

func (r *investigationTraceRecorder) Complete(decision UnexpectedEventDecision, stopReason string, runErr error) {
	if r == nil {
		return
	}
	r.trace.Decision, r.trace.StopReason, r.trace.Status = decision, stopReason, "complete"
	if runErr != nil {
		r.trace.Error = runErr.Error()
		r.trace.Status = "incomplete"
	}
	if r.persistenceError != "" {
		r.trace.Error = "trace persistence failed: " + r.persistenceError
		r.trace.Status = "incomplete"
	}
	r.trace.CompletedAt, r.trace.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.store.CompleteInvestigationTrace(ctx, r.trace.ID, decision, stopReason, r.trace.Error); err != nil {
		log.Printf("complete investigation trace failed: trace=%s run=%s error=%q", r.trace.ID, r.trace.RunID, err)
	}
}

func sanitizeReplayStep(step agent.ReplayStep) agent.ReplayStep {
	if step.Output == nil {
		return step
	}
	output := *step.Output
	if modelContent, ok := output.Content.([]ModelContent); ok {
		items := make([]map[string]any, 0, len(modelContent))
		for _, item := range modelContent {
			entry := map[string]any{}
			if item.Text != "" {
				entry["text"] = item.Text
			}
			if item.Image != nil {
				hash := sha256.Sum256([]byte(item.Image.Data))
				entry["image"] = map[string]any{"mime_type": item.Image.MIMEType, "sha256": hex.EncodeToString(hash[:]), "encoded_bytes": len(item.Image.Data)}
			}
			items = append(items, entry)
		}
		output.Content = items
	} else {
		output.Content = boundedTraceJSON(output.Content)
	}
	output.Value = boundedTraceValue(output.Value)
	step.Output = &output
	return step
}

func boundedTraceValue(value any) any {
	if value == nil {
		return nil
	}
	return boundedTraceJSON(value)
}

func boundedTraceJSON(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"record_error": "could not encode tool result"}
	}
	if len(encoded) <= maxTraceContentBytes {
		return json.RawMessage(encoded)
	}
	hash := sha256.Sum256(encoded)
	return map[string]any{"truncated": true, "bytes": len(encoded), "sha256": hex.EncodeToString(hash[:])}
}

func (s *Service) ListInvestigationTraces(ctx context.Context, runID string) ([]InvestigationTraceSummary, error) {
	if _, err := s.repository.Get(ctx, runID); err != nil {
		return nil, err
	}
	store, ok := s.repository.(InvestigationTraceRepository)
	if !ok {
		return nil, errors.New("investigation trace storage is unavailable")
	}
	return store.ListInvestigationTraces(ctx, runID)
}

func (s *Service) ReplayInvestigation(ctx context.Context, runID, traceID string) (InvestigationReplay, error) {
	store, ok := s.repository.(InvestigationTraceRepository)
	if !ok {
		return InvestigationReplay{}, errors.New("investigation trace storage is unavailable")
	}
	trace, err := store.GetInvestigationTrace(ctx, runID, traceID)
	if err != nil {
		return InvestigationReplay{}, err
	}
	tools := make(map[string]agent.ToolHandler, len(trace.AllowedTools))
	for _, name := range trace.AllowedTools {
		tools[name] = func(context.Context, agent.ToolCall, int) agent.ToolOutput { return agent.ToolOutput{} }
	}
	terminal := make(map[string]bool, len(trace.TerminalTools))
	for _, name := range trace.TerminalTools {
		terminal[name] = true
	}
	loop := agent.Loop{Limits: trace.Limits, Tools: tools, TerminalTools: terminal}
	result, replayErr := loop.Replay(ctx, trace.Steps)
	replay := InvestigationReplay{TraceID: trace.ID, ReplayedTurns: result.Turns, ReplayedTools: len(result.Observations), StopReason: result.StopReason, Decision: trace.Decision}
	if replayErr != nil {
		replay.Error = replayErr.Error()
	}
	replay.Matched = replay.StopReason == trace.StopReason && ((trace.Error == "" && replayErr == nil) || (trace.Error != "" && replayErr != nil && strings.TrimSpace(trace.Error) == strings.TrimSpace(replayErr.Error())))
	if replayErr == nil && trace.Error == "" {
		encodedReplay, marshalErr := json.Marshal(result.Value)
		var replayedDecision UnexpectedEventDecision
		if marshalErr != nil || json.Unmarshal(encodedReplay, &replayedDecision) != nil {
			replay.Matched = false
		} else {
			want, _ := json.Marshal(trace.Decision)
			got, _ := json.Marshal(replayedDecision)
			replay.Matched = replay.Matched && string(want) == string(got)
		}
	}
	return replay, nil
}

func validateInvestigationTrace(trace *InvestigationTrace) error {
	if trace == nil || trace.ID == "" || trace.RunID == "" || trace.EventID == "" || trace.AttemptID == "" {
		return errors.New("trace identity fields are required")
	}
	if len(trace.Steps) > 256 {
		return fmt.Errorf("trace has too many steps")
	}
	return nil
}
