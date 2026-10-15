package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testFrameCapturer struct {
	captured  atomic.Int32
	closed    atomic.Int32
	closedAll atomic.Int32
}

type testUnexpectedReviewer struct {
	decision UnexpectedEventDecision
	err      error
	calls    atomic.Int32
}

type blockingReviewer struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

type blockingTranscriber struct{ started chan struct{} }

type noSpeechTranscriber struct{}

func (noSpeechTranscriber) Transcribe(context.Context, AudioChunk) (SignalEvent, error) {
	return SignalEvent{}, ErrNoSpeech
}

func (t blockingTranscriber) Transcribe(ctx context.Context, _ AudioChunk) (SignalEvent, error) {
	close(t.started)
	<-ctx.Done()
	return SignalEvent{}, ctx.Err()
}

func (r *blockingReviewer) Review(ctx context.Context, _ UnexpectedEventInput) (UnexpectedEventDecision, error) {
	if r.calls.Add(1) == 1 {
		close(r.started)
	}
	select {
	case <-ctx.Done():
		return UnexpectedEventDecision{}, ctx.Err()
	case <-r.release:
		return UnexpectedEventDecision{Action: "ignore", Category: "other", Confidence: .9}, nil
	}
}

type fixedGoalPlanner struct{ plan MonitoringPlan }

func (p fixedGoalPlanner) Plan(context.Context, string) (MonitoringPlan, error) { return p.plan, nil }

func (r *testUnexpectedReviewer) Review(_ context.Context, input UnexpectedEventInput) (UnexpectedEventDecision, error) {
	r.calls.Add(1)
	decision := r.decision
	// Keep this fake aligned with the real reviewer contract: alert decisions
	// cite the event that caused the review. Tests that exercise a rejection can
	// still provide incomplete evidence explicitly.
	if decision.Action == "alert" && len(decision.UsedEvidence) == 0 && len(input.RecentEvidence) > 0 {
		decision.EvidenceBasis = "live"
		decision.UsedEvidence = []Evidence{evidenceFromSignal(input.RecentEvidence[0])}
		if decision.VisualArchiveID != "" {
			decision.UsedEvidence[0].FrameArchiveID = decision.VisualArchiveID
		}
	}
	if decision.VisualArchiveID != "" && decision.VisualObservedAt.IsZero() && len(input.RecentEvidence) > 0 {
		decision.VisualObservedAt = input.RecentEvidence[0].ObservedAt
	}
	return decision, r.err
}

func (c *testFrameCapturer) Capture(_ context.Context, _, _, path string) error {
	c.captured.Add(1)
	return os.WriteFile(path, []byte("frame"), 0o600)
}

func (c *testFrameCapturer) Close(string) { c.closed.Add(1) }
func (c *testFrameCapturer) CloseAll()    { c.closedAll.Add(1) }

func TestPersonalMonitorsRunIndependentlyWithDifferentGoals(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	service.SetReplayGap(time.Millisecond)
	coupon, err := service.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/1001", Goal: "出现 50 元以上优惠券时提醒我"})
	if err != nil {
		t.Fatal(err)
	}
	game, err := service.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/2002", Goal: "游戏进入决胜局或关键团战时提醒我"})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		coupon, _ = service.Get(context.Background(), coupon.ID)
		game, _ = service.Get(context.Background(), game.ID)
		if len(coupon.Events) == 6 && len(game.Events) == 6 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(coupon.Events) != 6 || len(game.Events) != 6 {
		t.Fatalf("signal replays did not finish: coupon=%d game=%d", len(coupon.Events), len(game.Events))
	}
	if coupon.Status != RunRunning || game.Status != RunRunning {
		t.Fatalf("personal monitors must remain active after the sample burst")
	}
	if len(coupon.Alerts) != 1 || coupon.Alerts[0].EventType != "coupon_drop" || len(game.Alerts) != 1 || game.Alerts[0].EventType != "game_moment" {
		t.Fatalf("goals should produce different alerts: coupon=%#v game=%#v", coupon.Alerts, game.Alerts)
	}

	statuses := map[string]int{}
	toolPaths := map[string]bool{}
	for _, investigation := range coupon.Investigations {
		statuses[investigation.Status]++
		path := ""
		for _, step := range investigation.Steps {
			path += step.Tool + ">"
		}
		toolPaths[path] = true
	}
	if statuses[InvestigationConfirmed] != 1 || statuses[InvestigationSuppressed] != 2 {
		t.Fatalf("unexpected decisions: %#v", statuses)
	}
	if len(toolPaths) < 3 {
		t.Fatalf("expected dynamic tool paths, got %#v", toolPaths)
	}
	if _, err := service.Stop(context.Background(), coupon.ID); err != nil {
		t.Fatalf("completed sample replay should remain cancellable: %v", err)
	}
	if _, err := service.Stop(context.Background(), game.ID); err != nil {
		t.Fatalf("second sample replay should remain cancellable: %v", err)
	}
	service.StopAll()
}

func TestUserCanUpdateGoalAndPlanWhileMonitorRuns(t *testing.T) {
	service := NewService(NewMemoryRepository(), nil)
	run, err := service.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/change-goal", Goal: "看到重要消息时提醒我"})
	if err != nil {
		t.Fatal(err)
	}
	service.SetGoalPlanner(fixedGoalPlanner{plan: MonitoringPlan{Summary: "视觉检查", CaptureIntervalSeconds: 12, ReviewIntervalSeconds: 30, UseOCR: true, VisionPolicy: "goal_required"}})
	updated, err := service.UpdateGoal(context.Background(), run.ID, "聊天区出现鱼上钩时提醒我")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Goal != "聊天区出现鱼上钩时提醒我" || updated.Plan.VisionPolicy != "agent_decides" || periodicVision(updated.Plan) || updated.GoalVersion != run.GoalVersion+1 || updated.Version <= run.Version {
		t.Fatalf("user-edited goal and its new bounded plan should be committed together: %#v", updated)
	}
	if _, err := service.UpdateGoal(context.Background(), run.ID, "x"); err == nil {
		t.Fatal("invalid goal should not be committed")
	}
	if current, _ := service.Get(context.Background(), run.ID); current.Goal != updated.Goal {
		t.Fatalf("invalid edit changed stored goal: %#v", current)
	}
	if _, err := service.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateGoal(context.Background(), run.ID, "继续关注鱼上钩"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stopped monitor should reject edits, got %v", err)
	}
}

func TestConcurrentSignalsForOneMonitorUseSingleFlightAndStopCancels(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	reviewer := &blockingReviewer{started: make(chan struct{}), release: make(chan struct{})}
	s.SetUnexpectedEventReviewer(reviewer, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/single-flight", Goal: "重要消息提醒", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		first <- s.Ingest(context.Background(), run.ID, SignalEvent{ID: "slow-1", Source: SignalOCR, Text: "第一条有效证据"})
	}()
	select {
	case <-reviewer.started:
	case <-time.After(time.Second):
		t.Fatal("first review did not start")
	}
	if err := s.Ingest(context.Background(), run.ID, SignalEvent{ID: "slow-2", Source: SignalASR, Text: "第二条并发证据"}); err != nil {
		t.Fatal(err)
	}
	if reviewer.calls.Load() != 1 {
		t.Fatalf("same monitor had overlapping reviews: %d", reviewer.calls.Load())
	}
	if _, err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatalf("repeated stop should be idempotent: %v", err)
	}
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop did not cancel review: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("review remained blocked after stop")
	}
	close(reviewer.release)
	stored, _ := repo.Get(context.Background(), run.ID)
	if stored.Status != RunStopped || len(stored.Alerts) != 0 || len(stored.Events) != 2 {
		t.Fatalf("unexpected stop race result: %#v", stored)
	}
}

func TestStopCancelsAudioTranscription(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	transcriber := blockingTranscriber{started: make(chan struct{})}
	s.SetTranscriber(transcriber)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/asr-cancel", Goal: "重要消息提醒", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.IngestAudio(context.Background(), run.ID, AudioChunk{Path: "mock.webm", EventID: "asr-slow"})
	}()
	select {
	case <-transcriber.started:
	case <-time.After(time.Second):
		t.Fatal("transcription did not start")
	}
	if _, err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop should cancel ASR: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("transcription did not stop")
	}
	stored, _ := repo.Get(context.Background(), run.ID)
	if stored.Status != RunStopped || len(stored.Events) != 0 {
		t.Fatalf("cancelled ASR must not commit a signal: %#v", stored)
	}
}

func TestNoSpeechAudioChunkIsNormalAndDoesNotCreateAnEvent(t *testing.T) {
	repo := NewMemoryRepository()
	service := NewService(repo, nil)
	service.SetTranscriber(noSpeechTranscriber{})
	run, err := service.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/silent-room", Goal: "主播说重要消息时提醒我", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.IngestAudio(context.Background(), run.ID, AudioChunk{Path: "silent.webm", EventID: "silent-1"}); err != nil {
		t.Fatalf("a silent chunk should not fail the listening session: %v", err)
	}
	stored, err := repo.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Events) != 0 || len(stored.Investigations) != 0 {
		t.Fatalf("silent chunk must not create false evidence: events=%#v investigations=%#v", stored.Events, stored.Investigations)
	}
	service.StopAll()
}

func TestReviewConcurrencyLimitReportsBackpressure(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	var releases []func()
	for i := 0; i < maxConcurrentReviews; i++ {
		release, allowed, overloaded := s.beginReview(fmt.Sprintf("monitor-%d", i), time.Now().Add(time.Duration(i)*time.Second), 1)
		if !allowed || overloaded {
			t.Fatalf("review slot %d should be granted (allowed=%v overloaded=%v)", i, allowed, overloaded)
		}
		releases = append(releases, release)
	}
	if release, allowed, overloaded := s.beginReview("overflow", time.Now().Add(time.Minute), 1); allowed || !overloaded || release != nil {
		t.Fatalf("full review budget must report overload (allowed=%v overloaded=%v)", allowed, overloaded)
	}
	for _, release := range releases {
		release()
	}
}

func TestReviewBudgetExhaustionIsNotReportedAsCompleted(t *testing.T) {
	if !reviewDecisionIncomplete(UnexpectedEventDecision{Action: "wait", StopReason: "turn_budget"}) {
		t.Fatal("budget-exhausted fallback must be classified as incomplete")
	}
	if reviewDecisionIncomplete(UnexpectedEventDecision{Action: "ignore", StopReason: "finished"}) {
		t.Fatal("a finished model decision must be classified as completed")
	}
}

func TestStartPersistsNormalizedGoalPlan(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	service.SetGoalPlanner(fixedGoalPlanner{plan: MonitoringPlan{
		Summary: "持续监听主播口播", CaptureIntervalSeconds: 6, ReviewIntervalSeconds: 15,
		RecommendASR: true, VisionPolicy: "agent_decides", Tools: []string{"search_observations"}, Planner: "MiniMax-M3",
	}})
	run, err := service.Start(context.Background(), StartInput{
		TargetURL: "https://live.example.com/room-plan", Goal: "主播说出重要消息时实时提醒", SourceMode: "ocr_upload",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Plan.Planner != "MiniMax-M3" || stored.Plan.CaptureIntervalSeconds != 1 || !stored.Plan.RecommendASR {
		t.Fatalf("monitor should persist the model-generated normalized plan: %#v", stored.Plan)
	}
	if !containsString(stored.Plan.Tools, "search_recent_alerts") || len(stored.Plan.UserConsent) == 0 {
		t.Fatalf("plan must include core tools and audio authorization requirement: %#v", stored.Plan)
	}
	service.StopAll()
}

func TestAmbiguousSignalWaitsForCrossSourceEvidence(t *testing.T) {
	repository := NewMemoryRepository()
	now := time.Now().UTC()
	run := &Run{ID: "monitor-test", Name: "test", Goal: "出现优惠券时提醒我", Status: RunRunning, StartedAt: now, UpdatedAt: now, Version: 1, Rooms: []Room{{ID: "room", Name: "room", Status: "listening"}}}
	if err := repository.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil)
	if err := service.Ingest(context.Background(), run.ID, SignalEvent{RoomID: "room", RoomName: "room", Source: "asr", Text: "等一下给大家一波福利", Confidence: .78}); err != nil {
		t.Fatal(err)
	}
	current, _ := repository.Get(context.Background(), run.ID)
	if current.Investigations[0].Status != InvestigationWaiting || len(current.Alerts) != 0 {
		t.Fatalf("ambiguous signal must wait: %#v", current.Investigations[0])
	}
	if err := service.Ingest(context.Background(), run.ID, SignalEvent{RoomID: "room", RoomName: "room", Source: "ocr", Text: "限量优惠券 50元", Confidence: .81}); err != nil {
		t.Fatal(err)
	}
	current, _ = repository.Get(context.Background(), run.ID)
	if current.Investigations[0].Status != InvestigationConfirmed || len(current.Alerts) != 1 {
		t.Fatalf("cross-source evidence must confirm: %#v", current.Investigations[0])
	}
}

func TestOCRFrameFlowsIntoAgentDecision(t *testing.T) {
	repository := NewMemoryRepository()
	now := time.Now().UTC()
	run := &Run{ID: "monitor-ocr", Name: "ocr", Goal: "新品正式发布时提醒我", Status: RunRunning, StartedAt: now, UpdatedAt: now, Version: 1, Rooms: []Room{{ID: "room", Name: "room", Status: "listening"}}}
	if err := repository.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil)
	service.SetTextRecognizer(StaticTextRecognizer{Text: "新品正式发布，今晚八点开启预售", Confidence: .93})
	if err := service.IngestFrame(context.Background(), run.ID, VideoFrame{RoomID: "room", Path: "frame.png"}); err != nil {
		t.Fatal(err)
	}
	current, _ := repository.Get(context.Background(), run.ID)
	if len(current.Events) != 1 || current.Events[0].Source != "ocr" {
		t.Fatalf("expected OCR event to be persisted: %#v", current.Events)
	}
	if len(current.Alerts) != 1 || current.Alerts[0].EventType != "major_announcement" {
		t.Fatalf("expected OCR signal to produce Agent alert: %#v", current.Alerts)
	}
}

func TestUnexpectedImportantEventCanAlertOutsideUserGoal(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	reviewer := &testUnexpectedReviewer{decision: UnexpectedEventDecision{
		Action: "alert", Category: "major_announcement", Summary: "直播间公布了新赛季上线时间",
		Reason: "这是目标之外但明确、即时的重要公告", Confidence: .91,
		ToolCalls: []ReviewToolCall{
			{Tool: "search_observations", Outcome: "已读取 1 条近期字幕证据", Succeeded: true},
			{Tool: "search_recent_alerts", Outcome: "未发现同类提醒记录", Succeeded: true},
			{Tool: "create_alert", Outcome: "提醒请求通过服务端校验", Succeeded: true},
		},
	}}
	service.SetUnexpectedEventReviewer(reviewer, time.Minute)
	run, err := service.Start(context.Background(), StartInput{
		TargetURL: "https://live.example.com/room-3", Goal: "抽奖开始时提醒我", SourceMode: "ocr_upload",
	})
	if err != nil {
		t.Fatal(err)
	}
	event := SignalEvent{RoomID: run.Rooms[0].ID, Source: "ocr", Text: "正式官宣新赛季今晚 20:00 上线", Confidence: .9}
	if err := service.Ingest(context.Background(), run.ID, event); err != nil {
		t.Fatal(err)
	}
	current, err := repository.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.calls.Load() != 1 || len(current.Alerts) != 1 {
		t.Fatalf("unexpected important event should be reviewed and alerted: calls=%d run=%#v", reviewer.calls.Load(), current)
	}
	alert := current.Alerts[0]
	if alert.EventType != "proactive_major_announcement" || alert.Summary != reviewer.decision.Summary || len(alert.Evidence) != 1 || alert.Evidence[0].Text != event.Text {
		t.Fatalf("proactive alert must carry its type, summary, and source evidence: %#v", alert)
	}
	if len(current.Investigations) != 1 || current.Investigations[0].Steps[0].Tool != "review_unexpected_event" || len(current.Investigations[0].Steps) != 4 {
		t.Fatalf("unexpected event judgment should be visible in the investigation: %#v", current.Investigations)
	}
	if current.Investigations[0].Steps[1].Outcome != "已读取 1 条近期字幕证据" || current.Investigations[0].Steps[2].Outcome != "未发现同类提醒记录" {
		t.Fatalf("executed tool outcomes should be persisted for the UI and replay: %#v", current.Investigations[0].Steps)
	}
}

func TestChatGoalMatchCreatesOCRBackedAlert(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	reviewer := &testUnexpectedReviewer{decision: UnexpectedEventDecision{
		Action: "alert", Category: "chat_match", Summary: "聊天区出现关注的新地图讨论",
		Reason: "可见评论正在讨论用户关注的新地图", VisualFinding: "评论内容位于画面右侧的聊天区域", Confidence: .9,
		VisualArchiveID: "archive-chat-success",
		ToolCalls: []ReviewToolCall{
			{Tool: "search_observations", Outcome: "已读取可见聊天文字", Succeeded: true},
			{Tool: "inspect_live_frame", Outcome: "已确认文字位于聊天区域", Succeeded: true},
			{Tool: "save_frame_evidence", Outcome: "已保存本次看图使用的截图", Succeeded: true},
			{Tool: "search_recent_alerts", Outcome: "未发现同类提醒", Succeeded: true},
			{Tool: "create_alert", Outcome: "已生成聊天命中提醒", Succeeded: true},
		},
	}}
	service.SetUnexpectedEventReviewer(reviewer, time.Minute)
	run, err := service.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/chat-room", Goal: "提醒我有适合参与的直播聊天", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.StopAll()
	if err := service.Ingest(context.Background(), run.ID, SignalEvent{RoomID: run.Rooms[0].ID, Source: SignalOCR, Text: "评论：大家觉得新地图哪个好玩？", Confidence: .9}); err != nil {
		t.Fatal(err)
	}
	stored, err := repository.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Alerts) != 1 || stored.Alerts[0].EventType != "proactive_chat_match" || stored.Alerts[0].Summary != reviewer.decision.Summary {
		t.Fatalf("chat goal match should create an evidence-backed alert without a draft: %#v", stored.Alerts)
	}
}

func TestChatGoalMatchRequiresVisibleOCREvidence(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	defer service.StopAll()
	reviewer := &testUnexpectedReviewer{decision: UnexpectedEventDecision{
		Action: "alert", Category: "chat_match", Summary: "聊天区出现关注的地图话题", VisualFinding: "右侧聊天区域出现该内容",
		Confidence: .95, VisualArchiveID: "archive-chat-without-ocr",
		ToolCalls: []ReviewToolCall{{Tool: "inspect_live_frame", Outcome: "已查看截图", Succeeded: true}},
	}}
	service.SetUnexpectedEventReviewer(reviewer, time.Minute)
	run, err := service.Start(context.Background(), StartInput{
		TargetURL: "https://live.example.com/chat-room", Goal: "提醒我能参与的聊天", SourceMode: "ocr_upload",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Ingest(context.Background(), run.ID, SignalEvent{
		RoomID: run.Rooms[0].ID, Source: SignalASR, Text: "主播正在直播", Confidence: .9,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := repository.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Alerts) != 0 {
		t.Fatalf("interaction alert must not be created without visible OCR evidence: %#v", stored.Alerts)
	}
	if len(stored.Investigations) != 1 || !strings.Contains(stored.Investigations[0].Conclusion, "缺少 OCR 或看图证据") {
		t.Fatalf("rejected interaction should explain the missing evidence: %#v", stored.Investigations)
	}
}

func TestChatGoalMatchRequiresVisualRegionConfirmation(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	defer service.StopAll()
	reviewer := &testUnexpectedReviewer{decision: UnexpectedEventDecision{
		Action: "alert", Category: "chat_match", Summary: "聊天中出现目标词",
		Reason: "OCR 找到目标词", Confidence: .95,
	}}
	service.SetUnexpectedEventReviewer(reviewer, time.Minute)
	run, err := service.Start(context.Background(), StartInput{
		TargetURL: "https://live.example.com/chat-room", Goal: "聊天区出现目标词时提醒我", SourceMode: "ocr_upload",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Ingest(context.Background(), run.ID, SignalEvent{
		RoomID: run.Rooms[0].ID, Source: SignalOCR, Text: "主播口播：目标词", Confidence: .9,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := repository.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Alerts) != 0 {
		t.Fatalf("chat alert must not be created without confirming the text is in chat: %#v", stored.Alerts)
	}
}

func TestUnexpectedReviewIgnoresRoutineSignalsAndThrottlesCalls(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	reviewer := &testUnexpectedReviewer{decision: UnexpectedEventDecision{
		Action: "ignore", Category: "other", Summary: "", Reason: "普通内容，不需要提醒", Confidence: .95,
	}}
	service.SetUnexpectedEventReviewer(reviewer, time.Minute)
	run, err := service.Start(context.Background(), StartInput{
		TargetURL: "https://live.example.com/room-4", Goal: "抽奖开始时提醒我", SourceMode: "ocr_upload",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	routine := SignalEvent{RoomID: run.Rooms[0].ID, Source: "ocr", Text: "大家好欢迎来到直播间", Confidence: .9, ObservedAt: base}
	if err := service.Ingest(context.Background(), run.ID, routine); err != nil {
		t.Fatal(err)
	}
	if reviewer.calls.Load() != 1 {
		t.Fatal("the first out-of-goal OCR snapshot should be reviewed to discover unanticipated events")
	}
	for i, text := range []string{"直播间正式官宣新赛季", "直播间重大更新：新地图今晚开放"} {
		event := SignalEvent{RoomID: run.Rooms[0].ID, Source: "ocr", Text: text, Confidence: .9, ObservedAt: base.Add(time.Duration(i+1) * 20 * time.Second)}
		if err := service.Ingest(context.Background(), run.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	if reviewer.calls.Load() != 1 {
		t.Fatalf("expected one review within the per-monitor cadence, got %d", reviewer.calls.Load())
	}
	current, _ := repository.Get(context.Background(), run.ID)
	if len(current.Alerts) != 0 {
		t.Fatalf("ignored proactive candidate should not alert: %#v", current.Alerts)
	}
}

func TestUnexpectedReviewFailureIsVisibleAndDoesNotStopMonitoring(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	reviewer := &testUnexpectedReviewer{err: errors.New("provider unavailable"), decision: UnexpectedEventDecision{
		ToolCalls: []ReviewToolCall{{Tool: "search_observations", Outcome: "已读取 1 条近期字幕证据", Succeeded: true}},
	}}
	service.SetUnexpectedEventReviewer(reviewer, time.Minute)
	run, err := service.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/room-fail", Goal: "抽奖时提醒我", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Ingest(context.Background(), run.ID, SignalEvent{RoomID: run.Rooms[0].ID, Source: "ocr", Text: "欢迎来到直播间", Confidence: .9}); err != nil {
		t.Fatal(err)
	}
	current, err := repository.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != RunRunning || len(current.Alerts) != 0 || len(current.Investigations) != 1 {
		t.Fatalf("review failure should not alert or stop the monitor: %#v", current)
	}
	inv := current.Investigations[0]
	if inv.Status != InvestigationFailed || inv.Conclusion != "AI 暂时无法完成判断，监控会继续运行" || len(inv.Steps) != 2 {
		t.Fatalf("failure and partial tool execution should be visible in the audit: %#v", inv)
	}
}

func TestBrowserCaptureLoopFeedsOCRIntoAgentAndStops(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	capturer := &testFrameCapturer{}
	service.SetFrameCapturer(capturer)
	service.SetTextRecognizer(StaticTextRecognizer{Text: "抽奖倒计时 00:30", Confidence: .93})
	service.SetCaptureInterval(5 * time.Millisecond)
	run, err := service.Start(context.Background(), StartInput{
		TargetURL: "https://live.example.com/room-1", Goal: "抽奖开始前提醒我", SourceMode: "browser_ocr",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, getErr := service.Get(context.Background(), run.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if len(current.Alerts) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	current, err := service.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if capturer.captured.Load() == 0 || len(current.Events) == 0 || len(current.Alerts) != 1 {
		t.Fatalf("capture should flow through OCR and Agent alert: captured=%d run=%#v", capturer.captured.Load(), current)
	}
	if _, err := service.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if capturer.closed.Load() != 1 {
		t.Fatal("stopping monitoring should close this run's browser tab")
	}
	service.StopAll()
	if capturer.closedAll.Load() != 1 {
		t.Fatal("service shutdown should close retained monitor tabs")
	}
}

func TestBrowserCaptureRunsForTwoMonitorsIndependently(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	capturer := &testFrameCapturer{}
	service.SetFrameCapturer(capturer)
	service.SetTextRecognizer(StaticTextRecognizer{Text: "画面持续更新", Confidence: .9})
	service.SetCaptureInterval(5 * time.Millisecond)
	defer service.StopAll()

	first, err := service.Start(context.Background(), StartInput{
		TargetURL: "https://live.example.com/room-1", Goal: "抽奖开始时提醒我", SourceMode: "browser_ocr",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Start(context.Background(), StartInput{
		TargetURL: "https://live.example.com/room-2", Goal: "商品下架时提醒我", SourceMode: "browser_ocr",
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && capturer.captured.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	firstState, firstErr := service.Get(context.Background(), first.ID)
	secondState, secondErr := service.Get(context.Background(), second.ID)
	if firstErr != nil || secondErr != nil {
		t.Fatalf("get concurrent monitors: first=%v second=%v", firstErr, secondErr)
	}
	if capturer.captured.Load() < 2 || firstState.Status != RunRunning || secondState.Status != RunRunning {
		t.Fatalf("starting a second live monitor must leave both capture loops running: captures=%d first=%s second=%s", capturer.captured.Load(), firstState.Status, secondState.Status)
	}
}

func TestTurnBudgetMeansInvestigationIncompleteNotNoMatch(t *testing.T) {
	if !reviewExecutionIncomplete("turn_budget") {
		t.Fatal("turn budget must be represented as an incomplete investigation")
	}
	message := reviewIncompleteMessage("turn_budget")
	if !strings.Contains(message, "未判断目标是否出现") || !strings.Contains(message, "规则是否需要澄清") {
		t.Fatalf("incomplete message made a semantic claim: %q", message)
	}
	if reviewExecutionIncomplete("finished") || reviewExecutionIncomplete("model_error") {
		t.Fatal("completed decisions and provider failures must not be classified as budget interruptions")
	}
}
