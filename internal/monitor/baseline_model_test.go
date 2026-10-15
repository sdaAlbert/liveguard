package monitor

import (
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAgentBaselineAnalyzerSendsFirstFrameAndRequiresStructuredPlan(t *testing.T) {
	framePath := filepath.Join(t.TempDir(), "first.png")
	file, err := os.Create(framePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode baseline request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"MiniMax-M3","stop_reason":"tool_use","content":[{"type":"tool_use","id":"baseline","name":"set_live_baseline","input":{"stream_type":"新闻直播","summary":"主播正在播报晚间新闻","visible_regions":["主播字幕","评论区"],"suggested_review_interval_seconds":15,"recommend_vision":false,"recommend_asr":true,"needs_login":false,"reason":"关注每条播报需要语音内容"}}],"usage":{"input_tokens":120,"output_tokens":35}}`))
	}))
	defer server.Close()

	analyzer := &AgentBaselineAnalyzer{Model: &MessagesModel{APIKey: "test", Model: "MiniMax-M3", Endpoint: server.URL, Client: server.Client(), Vision: true}}
	baseline, err := analyzer.AnalyzeBaseline(context.Background(), "每播出一条新闻就提醒我", defaultGoalPlan("每播出一条新闻就提醒我", "test"), SignalEvent{Text: "晚间新闻"}, framePath)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Status != BaselineReady || baseline.Summary != "主播正在播报晚间新闻" || !baseline.RecommendASR || baseline.Model != "MiniMax-M3" {
		t.Fatalf("unexpected baseline result: %+v", baseline)
	}
	if request["max_tokens"] != float64(baselineMaxOutputTokens) || baselineMaxOutputTokens != 4096 {
		t.Fatalf("baseline output budget should be 4096 tokens, got %#v", request["max_tokens"])
	}
	choice := request["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "set_live_baseline" {
		t.Fatalf("baseline must be a required structured tool call: %#v", request)
	}
	messages := request["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 2 || content[0].(map[string]any)["type"] != "text" || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("first live frame was not sent as a multimodal initial observation: %#v", content)
	}
}

func TestAgentBaselineAnalyzerRejectsTruncatedStructuredResponse(t *testing.T) {
	framePath := filepath.Join(t.TempDir(), "first.png")
	file, err := os.Create(framePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"MiniMax-M3","stop_reason":"max_tokens","content":[],"usage":{"input_tokens":120,"output_tokens":2048}}`))
	}))
	defer server.Close()

	analyzer := &AgentBaselineAnalyzer{Model: &MessagesModel{APIKey: "test", Model: "MiniMax-M3", Endpoint: server.URL, Client: server.Client(), Vision: true}}
	baseline, err := analyzer.AnalyzeBaseline(context.Background(), "每播出一条新闻就提醒我", defaultGoalPlan("每播出一条新闻就提醒我", "test"), SignalEvent{Text: "晚间新闻"}, framePath)
	if err == nil {
		t.Fatalf("truncated baseline response must not be accepted: %+v", baseline)
	}
	if baseline.Status == BaselineReady {
		t.Fatalf("truncated baseline response must never become ready: %+v", baseline)
	}
}
