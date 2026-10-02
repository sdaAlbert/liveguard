package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestMiniMaxRepairsNarrativeFinalWithAStructuredTool(t *testing.T) {
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		turns++
		if turns == 1 {
			_, _ = w.Write([]byte(`{"model":"fixture","content":[{"type":"text","text":"This is routine conversation, no alert."}]}`))
			return
		}
		messages := input["messages"].([]any)
		if len(messages) != 3 {
			t.Errorf("format correction history not preserved: %d messages", len(messages))
		}
		_, _ = w.Write([]byte(`{"model":"fixture","content":[{"type":"tool_use","id":"finish","name":"finish_review","input":{"action":"ignore","category":"other","reason":"日常闲聊","confidence":0.95}}]}`))
	}))
	defer server.Close()
	r := MiniMaxUnexpectedEventReviewer{Model: "fixture", Endpoint: server.URL, Client: server.Client()}
	decision, err := r.Review(context.Background(), UnexpectedEventInput{Goal: "重要消息提醒", RecentEvidence: []SignalEvent{{Text: "今天午饭吃了什么", Confidence: .9}}})
	if err != nil || decision.Action != "ignore" || decision.Repairs != 1 || len(decision.ToolCalls) != 1 || decision.ToolCalls[0].Tool != "finish_review" {
		t.Fatalf("format repair failed or invented tool calls: %#v %v", decision, err)
	}
}

func TestMiniMaxPreservesCompleteProviderBlocksInToolHistory(t *testing.T) {
	raw := []byte(`[{"type":"thinking","thinking":"fixture reasoning","signature":"provider-field"},{"type":"tool_use","id":"read","name":"read_recent_evidence","input":{}}]`)
	var blocks []miniMaxContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	_ = json.Unmarshal(raw, &before)
	_ = json.Unmarshal(encoded, &after)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("provider content fields were lost in assistant history")
	}
}

func TestFinishReviewCannotBeUsedToCreateAnAlert(t *testing.T) {
	block := miniMaxContentBlock{ID: "finish", Name: "finish_review", Input: json.RawMessage(`{"action":"alert","category":"custom_goal","reason":"bypass alert checks","confidence":0.99}`)}
	result, _, terminal := runMiniMaxTool(block, UnexpectedEventInput{}, nil, &miniMaxReviewState{}, 0)
	if !result.IsError || terminal {
		t.Fatal("finish_review bypassed the alert policy")
	}
}

func TestVisualGoalCannotAlertWithoutAnAvailableInspectedFrame(t *testing.T) {
	input := UnexpectedEventInput{Goal: "主播展示红色包装时提醒", RecentEvidence: []SignalEvent{{Text: "主播介绍产品", Confidence: .95}}}
	state := miniMaxReviewState{readEvidenceTurn: 0, searchAlertsTurn: 0, searchedCategory: "product_event"}
	block := miniMaxContentBlock{ID: "create", Name: "create_alert", Input: json.RawMessage(`{"category":"product_event","summary":"红色包装出现","reason":"模型猜测","confidence":0.99,"visual_finding":""}`)}
	result, _, terminal := runMiniMaxTool(block, input, nil, &state, 1)
	if !result.IsError || terminal {
		t.Fatal("visual goal was answered without visual evidence")
	}
}

func TestChatMatchNeedsVisibleOCREvidence(t *testing.T) {
	input := UnexpectedEventInput{RecentEvidence: []SignalEvent{{Source: SignalOCR, Text: "观众：你们最喜欢哪款？", Confidence: .9}}}
	state := miniMaxReviewState{readEvidenceTurn: 0, searchAlertsTurn: 0, searchedCategory: "chat_match", frameInspected: true}
	valid := miniMaxContentBlock{ID: "create", Name: "create_alert", Input: json.RawMessage(`{"category":"chat_match","summary":"聊天区出现关注的商品型号","reason":"可见评论提到用户关注的型号","confidence":0.91,"visual_finding":"该内容位于画面右侧的聊天区域"}`)}
	result, decision, terminal := (&MiniMaxUnexpectedEventReviewer{}).executeTool(context.Background(), valid, input, nil, &state, 1)
	if result.IsError || !terminal || decision.Category != "chat_match" {
		t.Fatalf("valid chat match alert was rejected: result=%#v decision=%#v terminal=%v", result, decision, terminal)
	}

	input.RecentEvidence[0].Source = SignalASR
	result, _, terminal = (&MiniMaxUnexpectedEventReviewer{}).executeTool(context.Background(), valid, input, nil, &state, 1)
	if !result.IsError || terminal {
		t.Fatal("chat match alert without visible OCR evidence must be rejected")
	}
	input.RecentEvidence[0].Source = SignalOCR
	state.frameInspected = false
	result, _, terminal = (&MiniMaxUnexpectedEventReviewer{}).executeTool(context.Background(), valid, input, nil, &state, 1)
	if !result.IsError || terminal {
		t.Fatal("chat match alert without visual confirmation must be rejected")
	}
}

type cancellingReviewer struct{ started chan struct{} }

func (r cancellingReviewer) Review(ctx context.Context, _ UnexpectedEventInput) (UnexpectedEventDecision, error) {
	close(r.started)
	<-ctx.Done()
	return UnexpectedEventDecision{}, ctx.Err()
}

func TestStoppingMonitorCancelsAnHTTPIngestReviewAndPreventsLateAlerts(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	started := make(chan struct{})
	s.SetUnexpectedEventReviewer(cancellingReviewer{started}, time.Second)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.douyin.com/test", Goal: "Go 垃圾回收开讲时提醒", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.Ingest(context.Background(), run.ID, SignalEvent{Source: "asr", Text: "现在正式开讲 Go 垃圾回收"})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("review not started")
	}
	if _, err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("review not cancelled: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stopped monitor still has a pending model request")
	}
	stored, _ := repo.Get(context.Background(), run.ID)
	if stored.Status != RunStopped || len(stored.Alerts) != 0 {
		t.Fatalf("late alert after stop: %#v", stored)
	}
}

func TestConfiguredModelHandlesKeywordMatchesWithoutRulesBypassingConditions(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	r := &testUnexpectedReviewer{decision: UnexpectedEventDecision{Action: "ignore", Category: "other", Confidence: .9}}
	s.SetUnexpectedEventReviewer(r, time.Second)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.douyin.com/test", Goal: "优惠券超过100元才提醒", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.StopAll()
	if err := s.Ingest(context.Background(), run.ID, SignalEvent{Source: SignalOCR, Text: "限量优惠券50元，点击领取", Confidence: .99}); err != nil {
		t.Fatal(err)
	}
	stored, _ := repo.Get(context.Background(), run.ID)
	if r.calls.Load() != 1 || len(stored.Alerts) != 0 {
		t.Fatal("keyword match bypassed the user's amount condition")
	}
}

func TestSemanticDedupeAllowsDifferentNewsWithinSameCategory(t *testing.T) {
	now := time.Now().UTC()
	alerts := []Alert{{RoomID: "r", EventType: "proactive_major_announcement", Summary: "今晚发布产品A", CreatedAt: now, Evidence: []Evidence{{Text: "产品A今晚发布"}}}}
	if hasRecentSemanticAlert(alerts, SignalEvent{RoomID: "r", Text: "产品B明天发布", ObservedAt: now.Add(time.Second)}, "proactive_major_announcement", "明天发布产品B") {
		t.Fatal("different news was blocked solely by category")
	}
	if !hasRecentSemanticAlert(alerts, SignalEvent{RoomID: "r", Text: "产品 A 今晚发布。", ObservedAt: now.Add(time.Second)}, "proactive_major_announcement", "") {
		t.Fatal("same news was not deduplicated")
	}
	if hasRecentSemanticAlert(alerts, SignalEvent{RoomID: "r", Text: "产品A今晚发布", ObservedAt: now.Add(3 * time.Minute)}, "proactive_major_announcement", "") {
		t.Fatal("old announcement remained blocked forever")
	}
}
