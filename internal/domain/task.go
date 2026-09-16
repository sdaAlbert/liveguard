package domain

import "time"

type Status string

const (
	StatusQueued     Status = "queued"
	StatusPlanning   Status = "planning"
	StatusRunning    Status = "running"
	StatusNeedsHuman Status = "needs_human"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
)

type CheckStatus string

const (
	CheckPending    CheckStatus = "pending"
	CheckPassed     CheckStatus = "passed"
	CheckFailed     CheckStatus = "failed"
	CheckUnverified CheckStatus = "unverified"
	CheckNeedsHuman CheckStatus = "needs_human"
)

type CheckSpec struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Kind        string   `json:"kind"`
	Terms       []string `json:"terms,omitempty"`
	Description string   `json:"description,omitempty"`
}

type CheckResult struct {
	CheckSpec
	Status   CheckStatus `json:"status"`
	Observed string      `json:"observed"`
	Evidence []string    `json:"evidence,omitempty"`
}

type Event struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

type Report struct {
	Title       string         `json:"title"`
	FinalURL    string         `json:"final_url"`
	Screenshot  string         `json:"screenshot,omitempty"`
	Planner     string         `json:"planner"`
	Verdict     string         `json:"verdict"`
	Summary     string         `json:"summary"`
	Checks      []CheckResult  `json:"checks"`
	ToolCalls   []ToolCall     `json:"tool_calls,omitempty"`
	Decision    *DecisionAudit `json:"decision,omitempty"`
	CompletedAt time.Time      `json:"completed_at"`
}

type DecisionAudit struct {
	Source        string `json:"source"`
	Model         string `json:"model,omitempty"`
	ResponseID    string `json:"response_id,omitempty"`
	CallID        string `json:"call_id,omitempty"`
	RequestedTool string `json:"requested_tool,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Approved      bool   `json:"approved"`
	PolicyReason  string `json:"policy_reason"`
	LatencyMS     int64  `json:"latency_ms"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	ModelError    string `json:"model_error,omitempty"`
}

type ToolCall struct {
	Name       string `json:"name"`
	Reason     string `json:"reason"`
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	Output     string `json:"output"`
	DurationMS int64  `json:"duration_ms"`
	Cleaned    bool   `json:"cleaned"`
	TraceID    string `json:"trace_id,omitempty"`
}

type Task struct {
	ID                      string      `json:"id"`
	URL                     string      `json:"url"`
	Objective               string      `json:"objective"`
	ExpectedTexts           []string    `json:"expected_texts,omitempty"`
	ExpectedLiveStatus      string      `json:"expected_live_status,omitempty"`
	UseAuthenticatedSession bool        `json:"use_authenticated_session,omitempty"`
	ParentTaskID            string      `json:"parent_task_id,omitempty"`
	Status                  Status      `json:"status"`
	Error                   string      `json:"error,omitempty"`
	Plan                    []CheckSpec `json:"plan,omitempty"`
	Events                  []Event     `json:"events,omitempty"`
	Report                  *Report     `json:"report,omitempty"`
	CreatedAt               time.Time   `json:"created_at"`
	UpdatedAt               time.Time   `json:"updated_at"`
	Version                 int64       `json:"version"`
	TraceID                 string      `json:"trace_id,omitempty"`
	TraceParent             string      `json:"trace_parent,omitempty"`
	TraceState              string      `json:"trace_state,omitempty"`
}

func (t *Task) AddEvent(kind, message string) {
	t.Events = append(t.Events, Event{
		ID:      NewID("evt"),
		Type:    kind,
		Message: message,
		At:      time.Now().UTC(),
	})
	t.UpdatedAt = time.Now().UTC()
	t.Version++
}
