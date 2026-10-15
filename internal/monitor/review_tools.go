package monitor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"liveguard/internal/agent"
)

type reviewToolResult struct {
	Content any
	Outcome string
	IsError bool
}

type reviewState struct {
	readEvidenceTurn    int
	historyEvidence     []SignalEvent
	historySearches     int
	contextBudget       *reviewContextBudget
	searchedHistory     bool
	searchedCategory    string
	searchAlertsTurn    int
	duplicateFound      bool
	frameInspected      bool
	frameSavedForReview bool
	frameInspectTurn    int
	frameObservedAt     time.Time
	frameEventID        string
	frameArchiveID      string
	frameAsset          *FrameAsset
	frameAssetOwned     bool
}

const maxObservationSearchPages = 6

// goalRequiresFrameInspection is a conservative fast path for explicit visual
// asks. For open-ended monitoring goals, the model chooses whether the image is
// useful through the normal auto tool policy.
func goalRequiresFrameInspection(goal string) bool {
	goal = strings.ToLower(strings.TrimSpace(goal))
	if goalIsExplicitTextOnly(goal) {
		return false
	}
	for _, marker := range []string{
		"图片里", "图像里", "截图中", "画面中的", "画面里", "看一下画面", "看看画面", "颜色", "什么颜色", "长什么样", "外观", "谁在", "是谁", "屏幕上显示", "包装", "衣服", "表情", "手势", "背景里", "logo", "商品长相", "钓鱼", "鱼上钩", "中鱼", "钓到鱼", "鱼出水", "足球", "开赛", "开球",
		"what does", "what color", "what is shown", "who is", "visible", "on screen", "in the image", "in the frame", "screenshot",
	} {
		if strings.Contains(goal, marker) {
			return true
		}
	}
	return false
}

func reviewToolsForPlan(plan MonitoringPlan) []map[string]any {
	categorySchema := map[string]any{"type": "string", "enum": []string{"major_announcement", "giveaway", "product_event", "game_moment", "urgent_notice", "chat_match", "custom_goal", "other"}}
	tools := []map[string]any{
		{"name": "request_user_input", "description": "Ask the user one concise question only when the answer depends on a preference or rule that cannot be inferred from evidence. Do not use this for ordinary missing evidence; keep monitoring and wait. Never use it for a clear time-sensitive opportunity. This ends only the current investigation, not the monitor. Provide up to four short suggested answers; the user can also type a custom answer.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"question": map[string]any{"type": "string", "minLength": 4, "maxLength": 240}, "options": map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 60}}}, "required": []string{"question"}, "additionalProperties": false}},
		{"name": "search_observations", "description": "Search this monitor's persisted OCR, ASR, frame and visual observations. scope=recent reads live context; scope=all searches history. text_any is an OR-list of up to 8 phrases, so related wording can be searched in one request. total_matches is the exact number of matching observation records for the filters (not distinct real-world events), independent of the page cursor. Each page returns at most 12 records and provides a cursor when older matches remain; pass all cursor fields back to continue.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"scope": map[string]any{"type": "string", "enum": []string{"recent", "all"}}, "text": map[string]any{"type": "string", "maxLength": 120}, "text_any": map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 120}}, "source": map[string]any{"type": "string", "enum": []string{"ocr", "asr", "frame", "vision"}}, "room_id": map[string]any{"type": "string", "maxLength": 80}, "from": map[string]any{"type": "string", "format": "date-time"}, "until": map[string]any{"type": "string", "format": "date-time"}, "before": map[string]any{"type": "string", "format": "date-time"}, "before_sequence": map[string]any{"type": "integer"}, "before_id": map[string]any{"type": "string", "maxLength": 128}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 12}}, "additionalProperties": false}},
		{"name": "search_recent_alerts", "description": "Check whether this monitor has already alerted on the same event category.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"category": categorySchema}, "required": []string{"category"}, "additionalProperties": false}},
		{"name": "inspect_live_frame", "description": "Inspect the current screenshot, or set observation_id to inspect an archived screenshot returned by search_observations. Use only when image evidence can resolve an important question.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"observation_id": map[string]any{"type": "string", "maxLength": 128}}, "additionalProperties": false}},
		{"name": "save_frame_evidence", "description": "Keep the exact screenshot you just inspected available only for this investigation. Call only when the image materially supports an alert or conclusion. The image stays in a temporary file and is deleted when this investigation ends; it is never copied to runtime or persisted. When selected, only its OCR text and your text finding may be recorded.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
		{"name": "create_alert", "description": "Request a user alert after evidence review and duplicate check. For chat_match, inspect the current image and confirm the target text is located in the visible chat area; OCR alone is insufficient. If you inspected the image, include only visible, relevant details in visual_finding. The server validates confidence and deduplication before writing it.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"category": categorySchema, "summary": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string"}, "confidence": map[string]any{"type": "number"}, "visual_finding": map[string]any{"type": "string"}}, "required": []string{"category", "summary", "reason", "confidence", "visual_finding"}, "additionalProperties": false}},
	}
	return tools
}

func (r *AgentReviewer) toolsForPlan(plan MonitoringPlan) []map[string]any {
	tools := reviewToolsForPlan(plan)
	if r.Model != nil && !r.Capabilities().Vision {
		filtered := tools[:0]
		for _, tool := range tools {
			if tool["name"] != "inspect_live_frame" && tool["name"] != "save_frame_evidence" {
				filtered = append(filtered, tool)
			}
		}
		tools = filtered
	}
	for _, tool := range tools {
		if tool["name"] == "create_alert" {
			schema := tool["input_schema"].(map[string]any)
			properties := schema["properties"].(map[string]any)
			properties["evidence_ids"] = map[string]any{"type": "array", "minItems": 1, "maxItems": 6, "items": map[string]any{"type": "string"}}
			properties["evidence_basis"] = map[string]any{"type": "string", "enum": []string{"live", "history"}}
			schema["required"] = append(schema["required"].([]string), "evidence_ids", "evidence_basis")
		}
		if tool["name"] == "create_alert" {
			tool["input_schema"].(map[string]any)["properties"].(map[string]any)["sampling"] = map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"mode": map[string]any{"type": "string", "enum": []string{"observing", "stable_wait", "candidate"}}, "reason": map[string]any{"type": "string", "maxLength": 200}, "next_event_at": map[string]any{"type": "string", "format": "date-time"},
			}, "required": []string{"mode", "reason"}}
		}
	}
	return tools
}

func (r *AgentReviewer) AdditionalToolNames() []string {
	return []string{"search_observations"}
}

func (r *AgentReviewer) executeTool(ctx context.Context, block agent.ToolCall, input UnexpectedEventInput, evidence []map[string]any, state *reviewState, turn int) (reviewToolResult, UnexpectedEventDecision, bool) {
	result := reviewToolResult{}
	var params map[string]json.RawMessage
	allowed := map[string]bool{}
	switch block.Name {
	case "search_observations":
		for _, key := range []string{"scope", "text", "text_any", "source", "room_id", "from", "until", "before", "before_sequence", "before_id", "limit"} {
			allowed[key] = true
		}
	case "search_recent_alerts":
		allowed["category"] = true
	case "inspect_live_frame":
		allowed["observation_id"] = true
	case "save_frame_evidence":
	case "request_user_input":
		allowed["question"], allowed["options"] = true, true
	case "create_alert":
		for _, key := range []string{"category", "summary", "reason", "confidence", "visual_finding", "sampling", "evidence_ids", "evidence_basis"} {
			allowed[key] = true
		}
	}
	if json.Unmarshal(block.Arguments, &params) != nil || params == nil {
		result.IsError, result.Content, result.Outcome = true, `{"error":"tool arguments must be an object"}`, "工具参数格式无效"
		return result, UnexpectedEventDecision{}, false
	}
	for key := range params {
		if !allowed[key] {
			encoded, _ := json.Marshal(map[string]string{"error": "unexpected tool parameter: " + key + "; remove it and use only fields in the tool schema"})
			result.IsError, result.Content, result.Outcome = true, string(encoded), "工具包含未允许的参数"
			return result, UnexpectedEventDecision{}, false
		}
	}
	if raw, exists := params["sampling"]; exists {
		var proposal SamplingProposal
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&proposal); err != nil {
			result.IsError, result.Content, result.Outcome = true, `{"error":"invalid sampling proposal"}`, "采样建议结构无效"
			return result, UnexpectedEventDecision{}, false
		}
	}
	if block.Name == "search_observations" {
		if state.contextBudget != nil && state.contextBudget.exhausted {
			result.IsError, result.Content, result.Outcome = true, `{"error":"investigation evidence context budget exhausted; stop searching and decide from evidence already returned"}`, "本次调查的历史证据上下文已满，停止继续检索"
			return result, UnexpectedEventDecision{}, false
		}
		if state.historySearches >= maxObservationSearchPages {
			result.IsError, result.Content, result.Outcome = true, `{"error":"history search budget exhausted"}`, "本次调查的全程证据查询次数已达上限"
			return result, UnexpectedEventDecision{}, false
		}
		var args struct {
			Scope          string       `json:"scope"`
			Text           string       `json:"text"`
			TextAny        []string     `json:"text_any"`
			Source         SignalSource `json:"source"`
			RoomID         string       `json:"room_id"`
			From           string       `json:"from"`
			Until          string       `json:"until"`
			Before         string       `json:"before"`
			BeforeSequence uint64       `json:"before_sequence"`
			BeforeID       string       `json:"before_id"`
			Limit          int          `json:"limit"`
		}
		if json.Unmarshal(block.Arguments, &args) != nil || args.Scope != "" && args.Scope != "recent" && args.Scope != "all" || utf8.RuneCountInString(args.Text) > 120 || len(args.TextAny) > 8 || utf8.RuneCountInString(args.RoomID) > 80 || args.Source != "" && args.Source != SignalOCR && args.Source != SignalASR && args.Source != SignalFrame && args.Source != SignalVision {
			result.IsError, result.Content, result.Outcome = true, `{"error":"invalid evidence query"}`, "证据查询条件无效"
			return result, UnexpectedEventDecision{}, false
		}
		for _, term := range args.TextAny {
			if utf8.RuneCountInString(term) > 120 {
				result.IsError, result.Content, result.Outcome = true, `{"error":"search phrase is too long"}`, "搜索词过长"
				return result, UnexpectedEventDecision{}, false
			}
		}
		if args.Scope == "recent" || args.Scope == "" {
			if args.Limit == 0 {
				args.Limit = 6
			}
			if args.From == "" && args.Until == "" {
				args.From = time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)
			}
		}
		query, err := observationQuery(args.From, args.Until, args.Limit)
		if err != nil {
			result.IsError, result.Content, result.Outcome = true, `{"error":"invalid time range"}`, "证据时间范围无效"
			return result, UnexpectedEventDecision{}, false
		}
		if args.Before != "" {
			query.Before, err = time.Parse(time.RFC3339, args.Before)
			if err != nil {
				result.IsError, result.Content, result.Outcome = true, `{"error":"invalid before cursor"}`, "观测分页游标无效"
				return result, UnexpectedEventDecision{}, false
			}
			query.BeforeSequence, query.BeforeEventID = args.BeforeSequence, args.BeforeID
		}
		query.Text, query.TextAny, query.Source, query.RoomID = args.Text, args.TextAny, args.Source, args.RoomID
		if !input.EvidenceAt.IsZero() && (query.Until.IsZero() || query.Until.After(input.EvidenceAt)) {
			query.Until = input.EvidenceAt
		}
		if query.RoomID == "" {
			query.RoomID = input.RecentEvidence[0].RoomID
		}
		var page ObservationPage
		if input.SearchObservations == nil && args.Scope != "all" {
			page = searchRecentInput(input.RecentEvidence, query)
		} else if input.SearchObservations == nil {
			result.IsError, result.Content, result.Outcome = true, `{"error":"persistent evidence search is unavailable"}`, "当前仓储没有提供全程证据查询"
			return result, UnexpectedEventDecision{}, false
		} else {
			page, err = input.SearchObservations(ctx, query)
			if err != nil {
				result.IsError, result.Content, result.Outcome = true, `{"error":"evidence query failed"}`, "全程证据查询失败"
				return result, UnexpectedEventDecision{}, false
			}
		}
		state.readEvidenceTurn = turn
		state.historySearches++
		if args.Scope == "all" {
			state.searchedHistory = true
		}
		modelEvents, acceptedEvents, contextTruncated := state.contextBudget.compactObservationPage(page.Events)
		contextTruncated = contextTruncated || state.contextBudget.exhausted && page.HasMore
		state.historyEvidence = append(state.historyEvidence, acceptedEvents...)
		var nextBefore, nextBeforeSequence, nextBeforeID any
		if page.HasMore && !contextTruncated {
			nextBefore, nextBeforeSequence, nextBeforeID = page.NextBefore, page.NextBeforeSequence, page.NextBeforeEventID
		}
		encoded, _ := json.Marshal(map[string]any{"observations": modelEvents, "count_returned": len(modelEvents), "total_matches": page.TotalMatches, "count_semantics": "matching observation records, not distinct real-world events", "has_more": page.HasMore || contextTruncated, "context_budget_exhausted": contextTruncated, "next_before": nextBefore, "next_before_sequence": nextBeforeSequence, "next_before_id": nextBeforeID, "limit": query.Limit})
		outcome := fmt.Sprintf("从%s观测记录中找到 %d 条", map[bool]string{true: "整场", false: "近期"}[args.Scope == "all"], len(modelEvents))
		if contextTruncated {
			outcome += "；本次调查上下文已满，停止继续检索"
		} else if page.HasMore {
			outcome += "，仍有更早结果"
		}
		result.Content, result.Outcome = string(encoded), outcome
		return result, UnexpectedEventDecision{}, false
	}
	if block.Name == "inspect_live_frame" {
		if state.frameInspected {
			result.IsError = true
			result.Content = `{"error":"only one frame inspection is allowed per review"}`
			result.Outcome = "本次判断的看图次数已达上限"
			return result, UnexpectedEventDecision{}, false
		}
		framePath := strings.TrimSpace(input.TriggerFrame)
		state.frameObservedAt = input.TriggerFrameObservedAt
		state.frameEventID = input.RecentEvidence[0].ID
		state.frameArchiveID = input.RecentEvidence[0].FrameArchiveID
		if framePath != "" && state.frameArchiveID == "" {
			asset := FrameAsset{Path: framePath, ObservationID: state.frameEventID, ObservedAt: state.frameObservedAt}
			state.frameAsset = &asset
		}
		owned := false
		var args struct {
			ObservationID string `json:"observation_id"`
		}
		_ = json.Unmarshal(block.Arguments, &args)
		if strings.TrimSpace(args.ObservationID) != "" && input.ArchivedFrame != nil {
			known := input.RecentEvidence[0].ID == strings.TrimSpace(args.ObservationID)
			for _, observed := range state.historyEvidence {
				if observed.ID == strings.TrimSpace(args.ObservationID) {
					known = true
				}
			}
			if !known {
				result.IsError, result.Content, result.Outcome = true, `{"error":"search for this observation before inspecting its image"}`, "请先搜索该历史观测再回看截图"
				return result, UnexpectedEventDecision{}, false
			}
			asset, err := input.ArchivedFrame(ctx, strings.TrimSpace(args.ObservationID))
			if err != nil {
				result.IsError, result.Content, result.Outcome = true, `{"error":"archived screenshot is unavailable"}`, "该历史观测没有可读取的归档截图"
				return result, UnexpectedEventDecision{}, false
			}
			framePath, state.frameObservedAt = asset.Path, asset.ObservedAt
			state.frameEventID, state.frameArchiveID = asset.ObservationID, asset.ArchiveID
		}
		if framePath == "" && input.CaptureFrame != nil {
			asset, err := input.CaptureFrame(ctx)
			if err != nil {
				result.IsError = true
				result.Content = `{"error":"could not capture a current frame from this monitor tab"}`
				result.Outcome = "无法从当前监控窗口获取最新画面"
				return result, UnexpectedEventDecision{}, false
			}
			framePath = asset.Path
			state.frameObservedAt = asset.ObservedAt
			state.frameEventID, state.frameArchiveID = asset.ObservationID, asset.ArchiveID
			state.frameAsset = &asset
			state.frameAssetOwned = true
			if asset.OCR != nil {
				state.historyEvidence = append(state.historyEvidence, *asset.OCR)
			}
			owned = true
		}
		if framePath == "" {
			result.IsError = true
			result.Content = `{"error":"no current screenshot is available"}`
			result.Outcome = "当前没有可供检查的直播画面"
			return result, UnexpectedEventDecision{}, false
		}
		imageBlock, err := prepareFrameImage(framePath)
		if owned && err != nil {
			_ = os.Remove(framePath)
		}
		if err != nil {
			result.IsError = true
			result.Content = `{"error":"current frame could not be prepared for image review"}`
			result.Outcome = "直播画面无法准备为图片证据"
			return result, UnexpectedEventDecision{}, false
		}
		state.frameInspected = true
		state.frameInspectTurn = turn
		metadata, _ := json.Marshal(map[string]any{"observation_id": state.frameEventID, "frame_archive_id": state.frameArchiveID, "observed_at": state.frameObservedAt})
		toolText := "这是监控证据截图，来源：" + string(metadata) + "。请只描述图中可直接确认且与候选事件有关的内容。"
		if state.frameEventID != "" {
			for _, observed := range state.historyEvidence {
				if observed.ParentEventID == state.frameEventID && observed.Source == SignalOCR {
					toolText += " 同一截图的 OCR 文字证据 ID=" + observed.ID + "：" + boundedReviewText(observed.Text, 600)
				}
			}
		}
		result.Content = []ModelContent{{Text: toolText}, {Image: &imageBlock}}
		result.Outcome = "已将监控截图作为图片证据交给模型"
		return result, UnexpectedEventDecision{}, false
	}
	if block.Name == "request_user_input" {
		var args struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		}
		if json.Unmarshal(block.Arguments, &args) != nil {
			result.IsError, result.Content, result.Outcome = true, `{"error":"invalid clarification question"}`, "求助问题格式无效"
			return result, UnexpectedEventDecision{}, false
		}
		decision := UnexpectedEventDecision{Action: "ask_user", Category: "other", Confidence: .9, Reason: "Agent 需要用户补充一条判断规则", UserQuestion: strings.TrimSpace(args.Question), UserOptions: args.Options}
		if err := validateUnexpectedDecision(decision); err != nil {
			result.IsError, result.Content, result.Outcome = true, `{"error":"question must be 4-240 characters, with at most four short options"}`, "求助内容不符合长度限制"
			return result, UnexpectedEventDecision{}, false
		}
		decision.VisualEvidenceID, decision.VisualArchiveID, decision.VisualObservedAt = state.frameEventID, state.frameArchiveID, state.frameObservedAt
		result.Content, result.Outcome = map[string]any{"question": decision.UserQuestion, "options": decision.UserOptions, "monitoring_continues": true}, "已向用户提出问题；本次调查等待回复，监控继续运行"
		return result, decision, true
	}
	if block.Name == "save_frame_evidence" {
		if !state.frameInspected || state.frameInspectTurn >= turn {
			result.IsError, result.Content, result.Outcome = true, `{"error":"inspect the frame and receive the model result before saving it"}`, "必须先看图并接收判断结果，才能保存图片"
			return result, UnexpectedEventDecision{}, false
		}
		if state.frameSavedForReview {
			result.Content, result.Outcome = `{"saved_temporarily":true,"persisted":false}`, "图片仅临时保留到本次调查结束；不会写入项目目录"
			return result, UnexpectedEventDecision{}, false
		}
		if !state.frameInspected || state.frameAsset == nil {
			result.IsError, result.Content, result.Outcome = true, `{"error":"inspect the current frame before selecting it"}`, "请先查看当前画面，再选择是否保留作本次判断依据"
			return result, UnexpectedEventDecision{}, false
		}
		if state.frameInspectTurn >= turn {
			result.IsError, result.Content, result.Outcome = true, `{"error":"wait for the image result before selecting it"}`, "必须先收到看图结果，才能选择这张图片"
			return result, UnexpectedEventDecision{}, false
		}
		if input.RecordFrameEvidence != nil {
			if err := input.RecordFrameEvidence(ctx, *state.frameAsset); err != nil {
				result.IsError, result.Content, result.Outcome = true, `{"error":"text evidence could not be recorded"}`, "无法记录这张截图对应的文字证据"
				return result, UnexpectedEventDecision{}, false
			}
		}
		state.frameSavedForReview = true
		encoded, _ := json.Marshal(map[string]any{"saved_temporarily": true, "persisted": false, "observation_id": state.frameEventID, "observed_at": state.frameObservedAt})
		result.Content, result.Outcome = string(encoded), "图片只在本次调查中临时保留；调查结束后删除，文字结论会随调查记录保存"
		return result, UnexpectedEventDecision{}, false
	}
	return runReviewTool(block, input, evidence, state, turn)
}

func searchRecentInput(events []SignalEvent, query ObservationQuery) ObservationPage {
	query = normalizeObservationQuery(query)
	matches := make([]SignalEvent, 0, len(events))
	var totalMatches int64
	for _, event := range events {
		if query.EventID != "" && event.ID != query.EventID || query.RoomID != "" && event.RoomID != query.RoomID || query.Source != "" && event.Source != query.Source {
			continue
		}
		if !query.From.IsZero() && event.ObservedAt.Before(query.From) || !query.Until.IsZero() && event.ObservedAt.After(query.Until) {
			continue
		}
		if !observationMatchesText(event, query.TextAny) {
			continue
		}
		totalMatches++
		if !observationBeforeCursor(event, query) {
			continue
		}
		matches = append(matches, event)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].ObservedAt.Equal(matches[j].ObservedAt) {
			if matches[i].Sequence == matches[j].Sequence {
				return matches[i].ID > matches[j].ID
			}
			return matches[i].Sequence > matches[j].Sequence
		}
		return matches[i].ObservedAt.After(matches[j].ObservedAt)
	})
	page := ObservationPage{Events: matches, TotalMatches: totalMatches}
	if len(matches) > query.Limit {
		page.HasMore = true
		page.Events = matches[:query.Limit]
		last := page.Events[len(page.Events)-1]
		page.NextBefore = last.ObservedAt
		page.NextBeforeSequence = last.Sequence
		page.NextBeforeEventID = last.ID
	}
	return page
}

func runReviewTool(block agent.ToolCall, input UnexpectedEventInput, evidence []map[string]any, state *reviewState, turn int) (reviewToolResult, UnexpectedEventDecision, bool) {
	result := reviewToolResult{}
	switch block.Name {
	case "search_recent_alerts":
		var args struct {
			Category string `json:"category"`
		}
		if err := json.Unmarshal(block.Arguments, &args); err != nil || !validUnexpectedCategory(args.Category) {
			result.IsError, result.Content = true, `{"error":"invalid category"}`
			result.Outcome = "事件类别无效，服务端拒绝查询"
			return result, UnexpectedEventDecision{}, false
		}
		roomID := input.RecentEvidence[0].RoomID
		matches := make([]map[string]any, 0, 2)
		for i := len(input.RecentAlerts) - 1; i >= 0 && len(matches) < 5; i-- {
			alert := input.RecentAlerts[i]
			age := input.RecentEvidence[0].ObservedAt.Sub(alert.CreatedAt)
			if alert.RoomID == roomID && alert.EventType == "proactive_"+args.Category && age >= 0 && age <= alertDedupeWindow {
				matches = append(matches, map[string]any{"summary": boundedReviewText(alert.Summary, 200), "created_at": alert.CreatedAt})
			}
		}
		state.searchedCategory = args.Category
		state.searchAlertsTurn = turn
		state.duplicateFound = hasRecentSemanticAlert(input.RecentAlerts, input.RecentEvidence[0], "proactive_"+args.Category, "")
		encoded, _ := json.Marshal(map[string]any{"category": args.Category, "duplicate": state.duplicateFound, "matches": matches, "note": "same category alone does not mean the same event; compare the actual content"})
		result.Content = string(encoded)
		if state.duplicateFound {
			result.Outcome = "发现同类提醒记录，避免重复打扰"
		} else if len(matches) > 0 {
			result.Outcome = "存在同类历史，但当前内容不同，交给 Agent 比较"
		} else {
			result.Outcome = "未发现同类提醒记录"
		}
		return result, UnexpectedEventDecision{}, false
	case "create_alert":
		if reviewRequiresVision(input) && !state.frameInspected {
			result.IsError, result.Content, result.Outcome = true, `{"error":"this goal requires an inspected frame; text alone is insufficient"}`, "该目标依赖画面，尚无可核验的看图证据"
			return result, UnexpectedEventDecision{}, false
		}
		var args UnexpectedEventDecision
		if err := json.Unmarshal(block.Arguments, &args); err != nil {
			result.IsError, result.Content = true, `{"error":"invalid alert arguments"}`
			result.Outcome = "提醒请求参数无效"
			return result, UnexpectedEventDecision{}, false
		}
		if state.readEvidenceTurn < 0 || state.readEvidenceTurn >= turn || state.searchAlertsTurn < 0 || state.searchAlertsTurn >= turn || state.searchedCategory != args.Category {
			result.IsError, result.Content = true, `{"error":"read evidence and search recent alerts before requesting an alert"}`
			result.Outcome = "需要先接收证据读取和查重结果，再请求提醒"
			return result, UnexpectedEventDecision{}, false
		}
		if state.frameInspected && state.frameInspectTurn >= turn {
			result.IsError, result.Content = true, `{"error":"the model must receive the image result before requesting an alert"}`
			result.Outcome = "看图结果尚未返回给 Agent，服务端拒绝提醒"
			return result, UnexpectedEventDecision{}, false
		}
		if state.frameInspected && strings.TrimSpace(args.VisualFinding) == "" {
			result.IsError, result.Content = true, `{"error":"visual finding is required after inspecting a frame"}`
			result.Outcome = "看图结果未形成可核验的文字结论，服务端拒绝提醒"
			return result, UnexpectedEventDecision{}, false
		}
		if state.frameInspected && strings.TrimSpace(args.VisualFinding) != "" && !state.frameSavedForReview {
			result.IsError, result.Content, result.Outcome = true, `{"error":"select the inspected screenshot with save_frame_evidence before using visual findings in an alert"}`, "提醒引用了看图结论；请先选择将该图作为本次判断依据"
			return result, UnexpectedEventDecision{}, false
		}
		if !state.frameInspected && strings.TrimSpace(args.VisualFinding) != "" {
			result.IsError, result.Content = true, `{"error":"visual finding is not allowed without frame inspection"}`
			result.Outcome = "未执行看图却提交了视觉结论，服务端拒绝提醒"
			return result, UnexpectedEventDecision{}, false
		}
		if state.duplicateFound {
			result.IsError, result.Content = true, `{"error":"duplicate alert already exists for this category"}`
			result.Outcome = "发现同类提醒，服务端拒绝再次发送"
			return result, UnexpectedEventDecision{}, false
		}
		if (args.Category == "chat_match" || input.Plan.AlertScope == "chat" || goalNeedsChatRegion(input.Goal)) && (!state.frameInspected || strings.TrimSpace(args.VisualFinding) == "") {
			result.IsError, result.Content, result.Outcome = true, `{"error":"chat matches require OCR evidence and visual confirmation of the chat region"}`, "聊天命中提醒需要 OCR 与看图确认聊天区域"
			return result, UnexpectedEventDecision{}, false
		}
		args.Action = "alert"
		cited, citeErr := resolveReviewCitations(input, state, args.EvidenceIDs, args.EvidenceBasis, time.Now().UTC())
		if citeErr != nil {
			result.IsError, result.Content, result.Outcome = true, `{"error":"cite 1-6 observed evidence IDs; live evidence must be fresh and from the current goal"}`, citeErr.Error()
			return result, UnexpectedEventDecision{}, false
		}
		credible := false
		for _, item := range cited {
			if item.Confidence == 0 || item.Confidence >= .5 {
				credible = true
			}
		}
		if !credible && !state.frameInspected {
			result.IsError, result.Content, result.Outcome = true, `{"error":"cited text confidence is too low; gather additional evidence"}`, "引用文字可信度过低，需要补充证据"
			return result, UnexpectedEventDecision{}, false
		}
		args.UsedEvidence = cited
		if args.Category == "chat_match" || input.Plan.AlertScope == "chat" || goalNeedsChatRegion(input.Goal) {
			matched := false
			for _, item := range cited {
				if item.Source != string(SignalOCR) {
					continue
				}
				if state.frameArchiveID != "" && item.FrameArchiveID == state.frameArchiveID {
					matched = true
				}
				for _, observed := range state.historyEvidence {
					if observed.ID == item.EventID && observed.ParentEventID == state.frameEventID {
						matched = true
					}
				}
			}
			if !matched {
				result.IsError, result.Content, result.Outcome = true, `{"error":"cite OCR from the same screenshot that was inspected"}`, "聊天文字与本次查看的画面没有匹配的 OCR 引用"
				return result, UnexpectedEventDecision{}, false
			}
		}
		if err := validateUnexpectedDecision(args); err != nil || args.Confidence < 0.72 {
			result.IsError, result.Content = true, `{"error":"alert failed server confidence or schema policy"}`
			result.Outcome = "未通过置信度或内容校验，服务端拒绝提醒"
			return result, UnexpectedEventDecision{}, false
		}
		if strings.TrimSpace(args.Summary) == "" || utf8.RuneCountInString(args.Summary) > 200 || utf8.RuneCountInString(args.Reason) > 300 {
			result.IsError, result.Content = true, `{"error":"alert text is empty or too long"}`
			result.Outcome = "提醒文案为空或超出长度限制"
			return result, UnexpectedEventDecision{}, false
		}
		if utf8.RuneCountInString(args.VisualFinding) > 400 {
			result.IsError, result.Content = true, `{"error":"visual finding is too long"}`
			result.Outcome = "看图结论过长，服务端拒绝提醒"
			return result, UnexpectedEventDecision{}, false
		}
		result.Content = `{"ok":true,"alert":"accepted by server policy"}`
		result.Outcome = "提醒请求通过服务端校验"
		return result, args, true
	default:
		result.IsError, result.Content = true, `{"error":"tool is not in the server allowlist"}`
		result.Outcome = "工具不在服务端允许列表中"
		return result, UnexpectedEventDecision{}, false
	}
}

func prepareFrameImage(path string) (ModelImage, error) {
	file, err := os.Open(path)
	if err != nil {
		return ModelImage{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 15<<20 {
		return ModelImage{}, errors.New("frame image size is outside the supported range")
	}
	config, _, err := image.DecodeConfig(file)
	if err != nil {
		return ModelImage{}, err
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > 4096 || config.Height > 4096 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return ModelImage{}, errors.New("frame dimensions exceed the supported range")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ModelImage{}, err
	}
	decoded, _, err := image.Decode(file)
	if err != nil {
		return ModelImage{}, err
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, decoded, &jpeg.Options{Quality: 72}); err != nil {
		return ModelImage{}, err
	}
	if encoded.Len() > 9<<20 {
		return ModelImage{}, errors.New("compressed frame exceeds the image input limit")
	}
	return ModelImage{MIMEType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(encoded.Bytes())}, nil
}
func boundedEvidence(events []SignalEvent) []map[string]any {
	result := make([]map[string]any, 0, min(len(events), 6))
	for _, event := range events[:min(len(events), 6)] {
		text := strings.TrimSpace(event.Text)
		if runes := []rune(text); len(runes) > 600 {
			text = string(runes[:600])
		}
		// OCR area labels are already included in event text. Repeating all line
		// boxes here wastes prompt space without adding evidence for the model.
		result = append(result, map[string]any{"id": event.ID, "source": event.Source, "text": text, "confidence": event.Confidence, "observed_at": event.ObservedAt})
	}
	return result
}

func boundedTextRegions(regions []TextRegion) []TextRegion {
	result := append([]TextRegion(nil), regions[:min(len(regions), 12)]...)
	for i := range result {
		result[i].Text = boundedReviewText(result[i].Text, 120)
	}
	return result
}

func validUnexpectedCategory(category string) bool {
	switch category {
	case "major_announcement", "giveaway", "product_event", "game_moment", "urgent_notice", "chat_match", "custom_goal", "other":
		return true
	default:
		return false
	}
}

func hasOCRSource(events []SignalEvent) bool {
	for _, event := range events {
		if event.Source == SignalOCR {
			return true
		}
	}
	return false
}

func setReviewMetadata(decision *UnexpectedEventDecision, model string, inputTokens, outputTokens int64, started time.Time) {
	decision.Model = model
	decision.InputTokens = inputTokens
	decision.OutputTokens = outputTokens
	decision.LatencyMS = time.Since(started).Milliseconds()
}
