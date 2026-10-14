package monitor

import (
	"context"
	"encoding/json"

	"liveguard/internal/agent"
	"liveguard/internal/domain"
)

// ToolModel is the protocol seam. The Agent owns policies, tools and budgets;
// adapters own native messages and preserve provider reasoning/signature fields.
type ToolModel interface {
	ModelID() string
	Capabilities() ModelCapabilities
	NewSession(ModelSessionConfig) ModelSession
}

type ModelCapabilities struct {
	ToolCalling bool `json:"tool_calling"`
	Vision      bool `json:"vision"`
}

type ModelSessionConfig struct {
	System             string
	Initial            string
	Tools              []ToolDefinition
	MaxOutputTokens    int
	RequireToolCall    bool
	EnsureRequiredTool bool
}

type ToolDefinition struct {
	Name        string
	Description string
	Schema      map[string]any
}

type ModelSession interface {
	Next(ctx context.Context, previous []agent.Observation, requiredTool string, feedback string) (ModelReply, error)
}

type ModelReply struct {
	Calls            []agent.ToolCall
	Text             string
	Model            string
	InputTokens      int64
	OutputTokens     int64
	TransportRetries int
}

// ModelContent is protocol-neutral tool output. Images are never placed inside
// model-generated JSON or represented as a filesystem path supplied by a model.
type ModelContent struct {
	Text  string
	Image *ModelImage
}
type ModelImage struct {
	MIMEType string
	Data     string
}

func toolDefinitions(definitions []map[string]any) []ToolDefinition {
	result := make([]ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, ToolDefinition{
			Name:        definition["name"].(string),
			Description: definition["description"].(string),
			Schema:      definition["input_schema"].(map[string]any),
		})
	}
	return result
}

func toolText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	encoded, _ := json.Marshal(content)
	return string(encoded)
}

func reviewRequiresVision(input UnexpectedEventInput) bool {
	return input.FrameCheck || input.Plan.VisionPolicy == "goal_required" ||
		goalRequiresFrameInspection(input.Goal) ||
		input.Plan.AlertScope == "chat" || goalNeedsChatRegion(input.Goal)
}

// ensureRequiredTool preserves the server-planned visual check when a provider
// ignores forced tool choice. Adapters append this call to native history before
// returning it, so its observation has a matching assistant call on the wire.
func ensureRequiredTool(reply *ModelReply, required string, ensure bool) *agent.ToolCall {
	if !ensure || required == "" {
		return nil
	}
	for _, call := range reply.Calls {
		if call.Name == required {
			return nil
		}
	}
	call := agent.ToolCall{ID: domain.NewID("planned_tool"), Name: required, Arguments: json.RawMessage("{}")}
	reply.Calls = append(reply.Calls, call)
	return &call
}
