package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"liveguard/internal/domain"
)

func TestOpenAIToolDeciderUsesStrictFunctionSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		tools, ok := request["tools"].([]any)
		if !ok || len(tools) != 1 || tools[0].(map[string]any)["strict"] != true || request["tool_choice"] != "auto" {
			t.Fatalf("missing strict tool contract: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","model":"test-model","status":"completed","output":[{"type":"function_call","name":"diagnose_live_page","arguments":"{\"reason\":\"activity evidence is missing\"}","call_id":"call_test"}],"usage":{"input_tokens":120,"output_tokens":20}}`))
	}))
	defer server.Close()

	decider := OpenAIToolDecider{APIKey: "test-only", Model: "test-model", Client: server.Client(), Endpoint: server.URL}
	result, err := decider.Decide(context.Background(), DecisionInput{Objective: "检查活动入口", PageText: "入口未加载"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision == nil || result.Decision.Name != "diagnose_live_page" || result.Decision.CallID != "call_test" {
		t.Fatalf("tool call was not parsed: %#v", result)
	}
	if result.Audit.ResponseID != "resp_test" || result.Audit.InputTokens != 120 || result.Audit.OutputTokens != 20 {
		t.Fatalf("decision audit was not captured: %#v", result.Audit)
	}
}

type alwaysFailDecider struct{}

func (alwaysFailDecider) Decide(context.Context, DecisionInput) (DecisionResult, error) {
	return DecisionResult{}, errors.New("provider unavailable")
}

func TestResilientToolDeciderRecordsFallback(t *testing.T) {
	input := DecisionInput{Objective: "检查活动入口", Checks: []domain.CheckResult{{CheckSpec: domain.CheckSpec{Kind: "activity_entry"}, Status: domain.CheckUnverified}}}
	result, err := (ResilientToolDecider{OpenAI: alwaysFailDecider{}, Deterministic: DeterministicToolDecider{}}).Decide(context.Background(), input)
	if err != nil || result.Decision == nil || result.Audit.Source != "deterministic_fallback" || result.Audit.ModelError == "" {
		t.Fatalf("fallback audit missing: %#v err=%v", result, err)
	}
}

func TestAgentEvalCoversRoutingAndPolicy(t *testing.T) {
	report := RunEval(context.Background(), DeterministicToolDecider{}, ToolPolicy{})
	if report.Total != 6 || report.Passed != report.Total || report.PassRate != 1 {
		t.Fatalf("unexpected eval report: %#v", report)
	}
}
