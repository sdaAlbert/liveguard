package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuleGoalPlannerSelectsSignalsAndIntervalsFromGoal(t *testing.T) {
	plan, err := (ruleGoalPlanner{}).Plan(context.Background(), "请实时监控主播说出的每一条新消息，出现时马上提醒我")
	if err != nil {
		t.Fatal(err)
	}
	plan = normalizeMonitoringPlan(plan, "请实时监控主播说出的每一条新消息，出现时马上提醒我")
	if !plan.UseOCR || !plan.RecommendASR || plan.CaptureIntervalSeconds != 6 || plan.ReviewIntervalSeconds != 15 {
		t.Fatalf("expected a fast multimodal plan for spoken time-sensitive goals: %#v", plan)
	}
	if !containsString(plan.Tools, "read_transcript_window") || len(plan.UserConsent) == 0 {
		t.Fatalf("the plan must select transcript reading and require explicit audio consent: %#v", plan)
	}
	if goalRequiresFrameInspection("主播每说出一条新消息就尽量实时提醒我，不要只识别画面字幕") {
		t.Fatal("mentioning captions while asking for speech must not force visual analysis")
	}
}

func TestNormalizePlanEnforcesSafetyAndSupportedIntervals(t *testing.T) {
	plan := normalizeMonitoringPlan(MonitoringPlan{
		Summary: "看画面和口播", CaptureIntervalSeconds: 1, ReviewIntervalSeconds: 1,
		RecommendASR: true, VisionPolicy: "unknown", Tools: []string{"run_shell", "read_transcript_window"},
	}, "判断商品包装是什么颜色并听主播说了什么")
	if !plan.UseOCR || plan.CaptureIntervalSeconds != 6 || plan.ReviewIntervalSeconds != 60 {
		t.Fatalf("unsupported model-selected rates must clamp to safe defaults: %#v", plan)
	}
	if plan.VisionPolicy != "goal_required" || !containsString(plan.Tools, "inspect_live_frame") || !containsString(plan.Tools, "read_transcript_window") {
		t.Fatalf("explicit image/audio goals must get their necessary tools: %#v", plan)
	}
	if containsString(plan.Tools, "run_shell") || len(plan.UserConsent) == 0 {
		t.Fatalf("disallowed tools or missing consent instructions escaped validation: %#v", plan)
	}
	spoken := normalizeMonitoringPlan(MonitoringPlan{VisionPolicy: "goal_required"}, "主播说出重要消息时提醒我")
	if spoken.VisionPolicy != "agent_decides" {
		t.Fatalf("spoken-only goals must not force image uploads: %#v", spoken)
	}
}

func TestASRToolIsOnlyOfferedWhenGoalPlanRecommendsASR(t *testing.T) {
	withoutASR := miniMaxToolsForPlan(MonitoringPlan{})
	withASR := miniMaxToolsForPlan(MonitoringPlan{RecommendASR: true})
	if len(withoutASR) != 4 || len(withASR) != 5 {
		t.Fatalf("expected transcript tool to be added only when planned: without=%d with=%d", len(withoutASR), len(withASR))
	}
	if !containsString([]string{"read_transcript_window"}, withASR[len(withASR)-1]["name"].(string)) {
		t.Fatal("ASR-enabled plan must expose transcript reading as a callable tool")
	}
}

func TestMiniMaxGoalPlannerUsesStructuredToolAndClampsPlan(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "plan-test-key" {
			t.Errorf("planner did not pass API auth")
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode planner request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"MiniMax-M3","stop_reason":"tool_use","content":[{"type":"tool_use","id":"plan_1","name":"set_monitoring_plan","input":{"summary":"持续听口播并按新内容提醒","capture_interval_seconds":6,"review_interval_seconds":15,"recommend_asr":true,"vision_policy":"agent_decides","tools":["read_recent_evidence","read_transcript_window","search_recent_alerts","create_alert"]}}]}`))
	}))
	defer server.Close()
	planner := MiniMaxGoalPlanner{APIKey: "plan-test-key", Model: "MiniMax-M3", Endpoint: server.URL, Client: server.Client()}
	plan, err := planner.Plan(context.Background(), "每当主播说出一条新消息就提醒我")
	if err != nil {
		t.Fatal(err)
	}
	choice := request["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "set_monitoring_plan" {
		t.Fatalf("goal planning must use a structured tool call: %#v", request)
	}
	if !plan.RecommendASR || plan.ReviewIntervalSeconds != 15 || !containsString(plan.Tools, "read_transcript_window") || len(plan.UserConsent) == 0 {
		t.Fatalf("unexpected normalized plan: %#v", plan)
	}
	if !strings.Contains(request["system"].(string), "bounded plan") {
		t.Fatal("the system prompt must ask for a bounded plan")
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
