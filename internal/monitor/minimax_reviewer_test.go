package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClassifyMiniMaxQuotaAndAuthErrors(t *testing.T) {
	quota := &struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{Type: "insufficient_balance", Message: "account balance is not enough"}
	if got := classifyMiniMaxAPIError(http.StatusBadRequest, quota); got != "limit_or_quota_error" {
		t.Fatalf("quota response should be identified even when provider uses HTTP 400, got %q", got)
	}
	if got := classifyMiniMaxAPIError(http.StatusTooManyRequests, nil); got != "limit_or_quota_error" {
		t.Fatalf("HTTP 429 should be identified as rate limit/quota, got %q", got)
	}
	if got := classifyMiniMaxAPIError(http.StatusUnauthorized, nil); got != "authentication_or_permission_error" {
		t.Fatalf("HTTP 401 should be identified as credential/permission failure, got %q", got)
	}
}

func TestMiniMaxReviewerRunsBoundedToolCallingLoop(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("unexpected MiniMax Anthropic auth/version headers")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"type":"message","model":"MiniMax-M3","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tool_evidence","name":"read_recent_evidence","input":{}},{"type":"tool_use","id":"tool_alerts","name":"search_recent_alerts","input":{"category":"major_announcement"}}],"usage":{"input_tokens":130,"output_tokens":44}}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","model":"MiniMax-M3","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tool_create","name":"create_alert","input":{"category":"major_announcement","summary":"主播宣布停播一年","reason":"这是明确且时效性强的重要消息","confidence":0.93,"visual_finding":""}}],"usage":{"input_tokens":80,"output_tokens":32}}`))
	}))
	defer server.Close()

	reviewer := MiniMaxUnexpectedEventReviewer{APIKey: "test-key", Model: "MiniMax-M3", Endpoint: server.URL + "/anthropic/v1/messages", Client: server.Client()}
	decision, err := reviewer.Review(context.Background(), UnexpectedEventInput{
		Goal:           "抽奖时提醒我",
		RecentEvidence: []SignalEvent{{RoomID: "room-1", Source: "ocr", Text: "主播宣布本场结束后将停播一年", Confidence: .94, ObservedAt: time.Now().UTC()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "alert" || decision.Category != "major_announcement" || decision.Model != "MiniMax-M3" || decision.Confidence != .93 {
		t.Fatalf("unexpected MiniMax decision: %#v", decision)
	}
	if decision.InputTokens != 210 || decision.OutputTokens != 76 || len(requests) != 2 {
		t.Fatalf("expected aggregated usage and two model turns, got decision=%#v requests=%d", decision, len(requests))
	}
	if len(decision.ToolCalls) != 3 || !decision.ToolCalls[0].Succeeded || !decision.ToolCalls[1].Succeeded || !decision.ToolCalls[2].Succeeded {
		t.Fatalf("expected three successful concrete tool calls: %#v", decision.ToolCalls)
	}
	if decision.ToolCalls[0].Outcome != "已读取 1 条近期字幕证据" || decision.ToolCalls[1].Outcome != "未发现同类提醒记录" || decision.ToolCalls[2].Outcome != "提醒请求通过服务端校验" {
		t.Fatalf("tool audit outcomes should be concise and readable: %#v", decision.ToolCalls)
	}
	first := requests[0]
	if first["model"] != "MiniMax-M3" || first["max_tokens"] != float64(512) || first["tool_choice"].(map[string]any)["type"] != "any" {
		t.Fatalf("request must enable bounded Anthropic tool use: %#v", first)
	}
	if len(first["tools"].([]any)) != 5 || !strings.Contains(first["system"].(string), "inspect_live_frame") {
		t.Fatalf("request must expose the four evidence/alert tools and structured finish tool: %#v", first)
	}
	initial := first["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(initial, "抽奖时提醒我") || !strings.Contains(initial, "停播一年") {
		t.Fatalf("request must include the user goal and triggering evidence: %s", initial)
	}
	secondMessages := requests[1]["messages"].([]any)
	if len(secondMessages) != 3 || secondMessages[2].(map[string]any)["role"] != "user" {
		t.Fatalf("tool results must be returned to the model before it chooses the next action: %#v", secondMessages)
	}
}

func TestMiniMaxReviewerRejectsAlertBeforeEvidenceAndDedupeTools(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"type":"message","model":"MiniMax-M3","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tool_create","name":"create_alert","input":{"category":"major_announcement","summary":"重大公告","reason":"证据充分","confidence":0.99,"visual_finding":""}}],"usage":{"input_tokens":10,"output_tokens":10}}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","model":"MiniMax-M3","stop_reason":"end_turn","content":[{"type":"text","text":"{\"action\":\"ignore\",\"category\":\"other\",\"summary\":\"\",\"reason\":\"服务端拒绝了未完成证据检查的提醒请求\",\"confidence\":0.99}"}],"usage":{"input_tokens":10,"output_tokens":10}}`))
	}))
	defer server.Close()
	reviewer := MiniMaxUnexpectedEventReviewer{APIKey: "test-key", Model: "MiniMax-M3", Endpoint: server.URL, Client: server.Client()}
	decision, err := reviewer.Review(context.Background(), UnexpectedEventInput{
		Goal: "抽奖开始提醒我", RecentEvidence: []SignalEvent{{RoomID: "room-1", Text: "宣布重大消息", Source: "ocr", Confidence: .9}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "ignore" || len(decision.ToolCalls) != 1 || decision.ToolCalls[0].Succeeded || !strings.Contains(decision.ToolCalls[0].Outcome, "查重结果") {
		t.Fatalf("the server must reject an alert before evidence and dedupe checks: %#v", decision)
	}
}

func TestMiniMaxCreateAlertRequiresMatchingNonDuplicateSearch(t *testing.T) {
	input := UnexpectedEventInput{RecentEvidence: []SignalEvent{{RoomID: "room-1", Text: "重要公告"}}}
	alertInput := []byte(`{"category":"giveaway","summary":"开始抽奖","reason":"字幕明确显示抽奖开始","confidence":0.9,"visual_finding":""}`)
	state := miniMaxReviewState{readEvidenceTurn: 0, searchedCategory: "major_announcement", searchAlertsTurn: 1, frameInspectTurn: -1}
	result, _, didAlert := runMiniMaxTool(miniMaxContentBlock{ID: "1", Name: "create_alert", Input: alertInput}, input, nil, &state, 2)
	if didAlert || !result.IsError {
		t.Fatal("a search for another category must not authorize an alert")
	}
	state = miniMaxReviewState{readEvidenceTurn: 0, searchedCategory: "giveaway", searchAlertsTurn: 1, duplicateFound: true, frameInspectTurn: -1}
	result, _, didAlert = runMiniMaxTool(miniMaxContentBlock{ID: "2", Name: "create_alert", Input: alertInput}, input, nil, &state, 2)
	if didAlert || !result.IsError || !strings.Contains(result.Outcome, "同类提醒") {
		t.Fatal("a duplicate search result must block alert creation")
	}
}

func TestMiniMaxCreateAlertMustWaitForToolResultsRoundTrip(t *testing.T) {
	input := UnexpectedEventInput{RecentEvidence: []SignalEvent{{RoomID: "room-1", Text: "重要消息"}}}
	alertInput := []byte(`{"category":"major_announcement","summary":"发布重要消息","reason":"近期字幕明确说明发布时间","confidence":0.9,"visual_finding":""}`)
	state := miniMaxReviewState{readEvidenceTurn: 0, searchedCategory: "major_announcement", searchAlertsTurn: 0, frameInspectTurn: -1}
	result, _, didAlert := runMiniMaxTool(miniMaxContentBlock{ID: "1", Name: "create_alert", Input: alertInput}, input, nil, &state, 0)
	if didAlert || !result.IsError {
		t.Fatal("the model must receive evidence and dedupe results before deciding to create an alert")
	}
	state.searchAlertsTurn = 1
	result, decision, didAlert := runMiniMaxTool(miniMaxContentBlock{ID: "2", Name: "create_alert", Input: alertInput}, input, nil, &state, 2)
	if !didAlert || result.IsError || decision.Action != "alert" {
		t.Fatalf("a later alert request may proceed after the model has received tool results: result=%#v decision=%#v", result, decision)
	}
}

func TestMiniMaxReviewerCanInspectAndGroundAnAlertInARealImageBlock(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		switch len(requests) {
		case 1:
			_, _ = w.Write([]byte(`{"model":"MiniMax-M3","stop_reason":"tool_use","content":[{"type":"tool_use","id":"inspect","name":"inspect_live_frame","input":{}}],"usage":{"input_tokens":20,"output_tokens":10}}`))
		case 2:
			_, _ = w.Write([]byte(`{"model":"MiniMax-M3","stop_reason":"tool_use","content":[{"type":"tool_use","id":"read","name":"read_recent_evidence","input":{}},{"type":"tool_use","id":"search","name":"search_recent_alerts","input":{"category":"product_event"}}],"usage":{"input_tokens":50,"output_tokens":20}}`))
		default:
			_, _ = w.Write([]byte(`{"model":"MiniMax-M3","stop_reason":"tool_use","content":[{"type":"tool_use","id":"create","name":"create_alert","input":{"category":"product_event","summary":"主播展示了红色包装产品","reason":"直播画面显示该产品包装，结合主播字幕确认正在介绍新品","confidence":0.9,"visual_finding":"画面中央展示一个红白配色包装盒，正面可见产品名"}}],"usage":{"input_tokens":40,"output_tokens":20}}`))
		}
	}))
	defer server.Close()

	imagePath := t.TempDir() + "/frame.png"
	file, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reviewer := MiniMaxUnexpectedEventReviewer{APIKey: "test-key", Model: "MiniMax-M3", Endpoint: server.URL, Client: server.Client()}
	decision, err := reviewer.Review(context.Background(), UnexpectedEventInput{
		Goal: "主播展示产品时，判断包装是什么颜色并提醒我", TriggerFrame: imagePath,
		RecentEvidence: []SignalEvent{{RoomID: "room-1", Source: "ocr", Text: "主播正在介绍一款产品", Confidence: .8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "alert" || decision.VisualFinding == "" || len(decision.ToolCalls) != 4 || len(requests) != 3 {
		t.Fatalf("image-backed alert should complete the bounded loop: %#v requests=%d", decision, len(requests))
	}
	firstChoice := requests[0]["tool_choice"].(map[string]any)
	if firstChoice["type"] != "tool" || firstChoice["name"] != "inspect_live_frame" {
		t.Fatalf("explicit visual goals must route to the image tool first: %#v", firstChoice)
	}
	toolMessages := requests[1]["messages"].([]any)
	toolResults := toolMessages[2].(map[string]any)["content"].([]any)
	blocks := toolResults[0].(map[string]any)["content"].([]any)
	imageBlock := blocks[1].(map[string]any)
	if imageBlock["type"] != "image" || imageBlock["source"].(map[string]any)["media_type"] != "image/jpeg" {
		t.Fatalf("inspect_live_frame must return an actual compressed image block to MiniMax: %#v", imageBlock)
	}
}

func TestMiniMaxReviewerKeepsToolChoiceAutomaticForGeneralGoals(t *testing.T) {
	if goalRequiresFrameInspection("抽奖开始时提醒我") {
		t.Fatal("a general event-monitoring goal should let M3 choose whether the frame helps")
	}
	if !goalRequiresFrameInspection("主播展示产品时，判断包装是什么颜色并提醒我") {
		t.Fatal("a goal that depends on a visible property must route to image inspection")
	}
}

func TestInspectFrameCanCaptureTheLatestFrameOnDemandAndCleansIt(t *testing.T) {
	observedAt := time.Now().UTC()
	captured := 0
	framePath := t.TempDir() + "/fresh.png"
	input := UnexpectedEventInput{RecentEvidence: []SignalEvent{{RoomID: "room-1", Source: SignalASR, Text: "主播说正在展示商品"}}, CaptureFrame: func(context.Context) (FrameAsset, error) {
		captured++
		file, err := os.Create(framePath)
		if err != nil {
			return FrameAsset{}, err
		}
		err = png.Encode(file, image.NewRGBA(image.Rect(0, 0, 8, 8)))
		_ = file.Close()
		if err != nil {
			return FrameAsset{}, err
		}
		return FrameAsset{Path: framePath, ObservedAt: observedAt}, nil
	}}
	r := MiniMaxUnexpectedEventReviewer{}
	state := miniMaxReviewState{readEvidenceTurn: 0, searchAlertsTurn: 0, searchedCategory: "custom_goal", frameInspectTurn: -1}
	block := miniMaxContentBlock{ID: "inspect", Name: "inspect_live_frame", Input: json.RawMessage(`{}`)}
	result, _, terminal := r.executeTool(context.Background(), block, input, nil, &state, 1)
	if result.IsError || terminal || !state.frameInspected || !state.frameObservedAt.Equal(observedAt) || captured != 1 {
		t.Fatalf("on-demand frame capture failed: %#v state=%#v calls=%d", result, state, captured)
	}
	if _, err := os.Stat(framePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary frame was not removed: %v", err)
	}
	blocks, ok := result.Content.([]map[string]any)
	if !ok || len(blocks) != 2 || blocks[1]["type"] != "image" {
		t.Fatalf("tool did not return image evidence: %#v", result.Content)
	}
	failedInput := UnexpectedEventInput{CaptureFrame: func(context.Context) (FrameAsset, error) { return FrameAsset{}, errors.New("chrome disconnected") }}
	failed, _, _ := r.executeTool(context.Background(), block, failedInput, nil, &miniMaxReviewState{readEvidenceTurn: 0, frameInspectTurn: -1}, 1)
	if !failed.IsError || !strings.Contains(failed.Content.(string), "could not capture") {
		t.Fatalf("capture failure must be returned as a tool error: %#v", failed)
	}
}

func TestMiniMaxReviewerEnforcesPlannedImageToolIfModelReturnsText(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"model":"MiniMax-M3","stop_reason":"end_turn","content":[{"type":"text","text":"{\"action\":\"ignore\",\"category\":\"other\",\"summary\":\"\",\"reason\":\"先看当前画面\",\"confidence\":0.8}"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"MiniMax-M3","stop_reason":"end_turn","content":[{"type":"text","text":"{\"action\":\"ignore\",\"category\":\"other\",\"summary\":\"\",\"reason\":\"画面没有目标商品\",\"confidence\":0.9}"}]}`))
	}))
	defer server.Close()
	imagePath := t.TempDir() + "/frame.png"
	file, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reviewer := MiniMaxUnexpectedEventReviewer{APIKey: "test-key", Model: "MiniMax-M3", Endpoint: server.URL, Client: server.Client()}
	decision, err := reviewer.Review(context.Background(), UnexpectedEventInput{
		Goal: "判断当前画面中的包装是什么颜色", TriggerFrame: imagePath,
		RecentEvidence: []SignalEvent{{RoomID: "room-1", Source: "ocr", Text: "商品正在展示"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "ignore" || len(decision.ToolCalls) != 1 || decision.ToolCalls[0].Tool != "inspect_live_frame" || !decision.ToolCalls[0].Succeeded {
		t.Fatalf("explicit visual plan should execute the image tool before accepting a text decision: %#v", decision)
	}
	secondMessages := requests[1]["messages"].([]any)
	toolResultBlocks := secondMessages[2].(map[string]any)["content"].([]any)
	imageResult := toolResultBlocks[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	if imageResult["type"] != "image" {
		t.Fatalf("the model must receive the planned frame as an image tool result: %#v", imageResult)
	}
}

func TestMiniMaxReviewerMapsUnsupportedFinalActionToSafeIgnore(t *testing.T) {
	decision, err := parseMiniMaxDecision([]miniMaxContentBlock{{Type: "text", Text: `{"action":"notify_everyone","category":"other","summary":"x","reason":"x","confidence":0.99}`}})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "ignore" || decision.Confidence != 0 {
		t.Fatalf("unsupported model action must fail closed to ignore: %#v", decision)
	}
}
