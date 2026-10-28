package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"liveguard/internal/agent"
)

// MessagesModel translates only the native MiniMax Messages protocol.
type MessagesModel struct {
	APIKey   string
	Model    string
	Endpoint string
	Client   *http.Client
	Vision   bool
}

func (m *MessagesModel) ModelID() string { return m.Model }
func (m *MessagesModel) Capabilities() ModelCapabilities {
	return ModelCapabilities{ToolCalling: true, Vision: m.Vision}
}
func (m *MessagesModel) NewSession(config ModelSessionConfig) ModelSession {
	content := any(config.Initial)
	if len(config.InitialContent) > 0 {
		content = messagesToolContent(config.InitialContent)
	}
	initial := map[string]any{"role": "user", "content": content}
	return &messagesSession{model: m, config: config, prefix: []map[string]any{initial}, messages: []map[string]any{initial}}
}

type messagesSession struct {
	model    *MessagesModel
	config   ModelSessionConfig
	prefix   []map[string]any
	messages []map[string]any
}

func (s *messagesSession) CompactContext(memory string) bool {
	if len(s.messages) <= len(s.prefix)+1 {
		return false
	}
	latest := s.messages[len(s.messages)-1]
	if latest["role"] != "assistant" {
		return false
	}
	compacted := append([]map[string]any(nil), s.prefix...)
	if strings.TrimSpace(memory) != "" {
		compacted = append(compacted, map[string]any{"role": "user", "content": memory})
	}
	compacted = append(compacted, latest)
	s.messages = compacted
	return true
}

type miniMaxContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Text  string          `json:"text,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	raw   json.RawMessage
}

// Preserve every provider-returned block (including thinking/signature fields)
// when sending assistant history back; typed fields are only for dispatch.
func (b *miniMaxContentBlock) UnmarshalJSON(data []byte) error {
	type alias miniMaxContentBlock
	if err := json.Unmarshal(data, (*alias)(b)); err != nil {
		return err
	}
	b.raw = append(b.raw[:0], data...)
	return nil
}

func (b miniMaxContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.raw) > 0 {
		return b.raw, nil
	}
	type alias miniMaxContentBlock
	return json.Marshal(alias(b))
}

type miniMaxMessageResponse struct {
	TransportRetries int                   `json:"-"`
	Model            string                `json:"model"`
	StopReason       string                `json:"stop_reason"`
	Content          []miniMaxContentBlock `json:"content"`
	Usage            struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

func (s *messagesSession) Next(ctx context.Context, previous []agent.Observation, requiredTool, feedback string) (reply ModelReply, err error) {
	started := time.Now()
	trace := modelCallTraceFromContext(ctx)
	statusCode, retries := 0, 0
	clientTimeout := time.Duration(0)
	deadlineBudgetMS := int64(0)
	if deadline, ok := ctx.Deadline(); ok {
		deadlineBudgetMS = time.Until(deadline).Milliseconds()
	}
	defer func() {
		if reply.Model == "" {
			reply.Model = s.model.Model
		}
		outcome := "completed"
		timeoutSource := modelTimeoutSource(ctx, trace, clientTimeout, err)
		errorText := ""
		if err != nil {
			outcome = "failed"
			errorText = err.Error()
		}
		log.Printf("model request %s: provider=minimax model=%s review_id=%q attempt_id=%q turn=%d http_status=%d retries=%d latency_ms=%d context_budget_ms=%d client_timeout_ms=%d timeout_source=%s input_tokens=%d output_tokens=%d error=%q", outcome, reply.Model, trace.ReviewID, trace.AttemptID, trace.Turn, statusCode, retries, time.Since(started).Milliseconds(), deadlineBudgetMS, clientTimeout.Milliseconds(), timeoutSource, reply.InputTokens, reply.OutputTokens, errorText)
	}()
	if len(previous) > 0 {
		results := make([]map[string]any, 0, len(previous))
		for _, observation := range previous {
			results = append(results, map[string]any{
				"type": "tool_result", "tool_use_id": observation.Call.ID,
				"content":  messagesToolContent(observation.Output.Content),
				"is_error": observation.Output.Failed,
			})
		}
		s.messages = append(s.messages, map[string]any{"role": "user", "content": results})
	}
	if feedback != "" {
		s.messages = append(s.messages, map[string]any{"role": "user", "content": feedback})
	}
	definitions := make([]map[string]any, 0, len(s.config.Tools))
	for _, tool := range s.config.Tools {
		definitions = append(definitions, map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": tool.Schema})
	}
	choice := map[string]string{"type": "auto"}
	if s.config.RequireToolCall {
		choice = map[string]string{"type": "any"}
	}
	if requiredTool != "" {
		choice = map[string]string{"type": "tool", "name": requiredTool}
	}
	body, err := json.Marshal(map[string]any{
		"model": s.model.Model, "max_tokens": s.config.MaxOutputTokens,
		"system": s.config.System, "tools": definitions, "tool_choice": choice, "messages": s.messages,
	})
	if err != nil {
		return reply, err
	}
	endpoint := strings.TrimSpace(s.model.Endpoint)
	if endpoint == "" {
		endpoint = "https://api.minimaxi.com/anthropic/v1/messages"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	request.Header.Set("x-api-key", s.model.APIKey)
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	client := s.model.Client
	if client == nil {
		client = agent.DefaultHTTPClient()
	}
	clientTimeout = client.Timeout
	response, requestRetries, requestErr := agent.DoModelRequest(client, request)
	retries = requestRetries
	err = requestErr
	reply.TransportRetries = retries
	if err != nil {
		return reply, fmt.Errorf("Messages model request: %w", err)
	}
	defer response.Body.Close()
	statusCode = response.StatusCode
	if response.StatusCode/100 != 2 {
		var apiError miniMaxMessageResponse
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&apiError)
		return reply, messagesProviderError(response.StatusCode, apiError)
	}
	var envelope miniMaxMessageResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&envelope); err != nil {
		return reply, fmt.Errorf("decode Messages model reply: %w", err)
	}
	reply.Model, reply.InputTokens, reply.OutputTokens = envelope.Model, envelope.Usage.InputTokens, envelope.Usage.OutputTokens
	if envelope.Error != nil {
		return reply, messagesProviderError(response.StatusCode, envelope)
	}
	if envelope.BaseResp != nil && envelope.BaseResp.StatusCode != 0 {
		return reply, messagesProviderError(response.StatusCode, envelope)
	}
	if envelope.StopReason == "max_tokens" {
		return reply, fmt.Errorf("model response truncated at token limit")
	}

	for _, block := range envelope.Content {
		if block.Type == "tool_use" {
			reply.Calls = append(reply.Calls, agent.ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input})
		}
		if block.Type == "text" {
			reply.Text += block.Text
		}
	}
	if call := ensureRequiredTool(&reply, requiredTool, s.config.EnsureRequiredTool); call != nil {
		envelope.Content = append(envelope.Content, miniMaxContentBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: call.Arguments})
	}
	s.messages = append(s.messages, map[string]any{"role": "assistant", "content": envelope.Content})
	return reply, nil
}

func messagesToolContent(content any) any {
	blocks, ok := content.([]ModelContent)
	if !ok {
		return toolText(content)
	}
	result := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		if block.Text != "" {
			result = append(result, map[string]any{"type": "text", "text": block.Text})
		}
		if block.Image != nil {
			result = append(result, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": block.Image.MIMEType, "data": block.Image.Data,
			}})
		}
	}
	return result
}

func classifyMiniMaxAPIError(status int, providerError *struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}) string {
	message := ""
	if providerError != nil {
		message = strings.ToLower(providerError.Type + " " + providerError.Message)
	}
	switch {
	case status == http.StatusTooManyRequests || status == http.StatusPaymentRequired:
		return "limit_or_quota_error"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_or_permission_error"
	case strings.Contains(message, "quota"), strings.Contains(message, "credit"), strings.Contains(message, "balance"), strings.Contains(message, "rate limit"), strings.Contains(message, "余额"), strings.Contains(message, "限额"), strings.Contains(message, "额度"):
		return "limit_or_quota_error"
	default:
		return "provider_error"
	}
}

func messagesProviderError(status int, envelope miniMaxMessageResponse) error {
	code, kind, message := "", "", ""
	if envelope.Error != nil {
		code, kind, message = providerErrorCode(envelope.Error.Code), envelope.Error.Type, envelope.Error.Message
	}
	if envelope.BaseResp != nil && envelope.BaseResp.StatusCode != 0 {
		code = strconv.Itoa(envelope.BaseResp.StatusCode)
		message = envelope.BaseResp.StatusMsg
	}
	return modelProviderError(status, code, kind, message)
}
