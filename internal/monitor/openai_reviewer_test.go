package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIUnexpectedReviewerUsesStrictBoundedSchema(t *testing.T) {
	var request struct {
		Model     string `json:"model"`
		Store     bool   `json:"store"`
		Input     string `json:"input"`
		MaxOutput int    `json:"max_output_tokens"`
		Text      struct {
			Format struct {
				Type   string `json:"type"`
				Name   string `json:"name"`
				Strict bool   `json:"strict"`
			} `json:"format"`
		} `json:"text"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("unexpected authorization header %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","model":"test-model","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"action\":\"alert\",\"category\":\"major_announcement\",\"summary\":\"直播间宣布新赛季上线时间\",\"reason\":\"这是目标之外但具有明确时效的重大公告\",\"confidence\":0.91}"}]}]}`))
	}))
	defer server.Close()

	reviewer := OpenAIUnexpectedEventReviewer{APIKey: "test-key", Model: "test-model", Endpoint: server.URL, Client: server.Client()}
	now := time.Now().UTC()
	decision, err := reviewer.Review(context.Background(), UnexpectedEventInput{Goal: "抽奖开始时提醒我", RecentEvidence: []SignalEvent{{Source: "ocr", Text: "正式官宣新赛季今晚上线", Confidence: .9, ObservedAt: now}}})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "alert" || decision.Category != "major_announcement" || decision.Confidence < .72 {
		t.Fatalf("unexpected decision: %#v", decision)
	}
	if request.Model != "test-model" || request.Store || request.MaxOutput == 0 || request.Text.Format.Type != "json_schema" || !request.Text.Format.Strict {
		t.Fatalf("request must use a bounded, strict, non-stored schema: %#v", request)
	}
	var input struct {
		Goal     string        `json:"user_goal"`
		Evidence []SignalEvent `json:"recent_evidence"`
	}
	if err := json.Unmarshal([]byte(request.Input), &input); err != nil {
		t.Fatal(err)
	}
	if input.Goal != "抽奖开始时提醒我" || len(input.Evidence) != 1 || input.Evidence[0].Text != "正式官宣新赛季今晚上线" {
		t.Fatalf("unexpected review input: %#v", input)
	}
}

func TestOpenAIUnexpectedReviewerRejectsUnsafeDecisionValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"action\":\"run_shell\",\"category\":\"other\",\"summary\":\"x\",\"reason\":\"x\",\"confidence\":0.99}"}]}]}`))
	}))
	defer server.Close()
	reviewer := OpenAIUnexpectedEventReviewer{APIKey: "test-key", Model: "test-model", Endpoint: server.URL, Client: server.Client()}
	if _, err := reviewer.Review(context.Background(), UnexpectedEventInput{Goal: "关注直播", RecentEvidence: []SignalEvent{{Text: "正式官宣"}}}); err == nil {
		t.Fatal("unsupported actions must be rejected")
	}
}

func TestOpenAIUnexpectedReviewerPreservesAPIErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"quota unavailable"}}`))
	}))
	defer server.Close()
	reviewer := OpenAIUnexpectedEventReviewer{APIKey: "test-key", Model: "test-model", Endpoint: server.URL, Client: server.Client()}
	_, err := reviewer.Review(context.Background(), UnexpectedEventInput{Goal: "关注直播", RecentEvidence: []SignalEvent{{Text: "重要公告"}}})
	if err == nil || !strings.Contains(err.Error(), "insufficient_quota") {
		t.Fatalf("expected actionable API error code, got %v", err)
	}
}
