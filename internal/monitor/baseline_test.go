package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fixedBaselineAnalyzer struct {
	mu     sync.Mutex
	calls  int
	goal   string
	text   string
	image  string
	result LiveBaseline
	err    error
}

func (a *fixedBaselineAnalyzer) AnalyzeBaseline(_ context.Context, goal string, _ MonitoringPlan, evidence SignalEvent, imagePath string) (LiveBaseline, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.goal, a.text, a.image = goal, evidence.Text, imagePath
	return a.result, a.err
}

func (a *fixedBaselineAnalyzer) snapshot() (int, string, string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls, a.goal, a.text, a.image
}

func TestInitialBaselineUsesFirstTemporaryFrameAndUpdatesPlan(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	goal := "新闻直播里每条新播出的新闻都提醒我"
	now := time.Now().UTC()
	run := &Run{ID: "baseline-run", Goal: goal, GoalVersion: 1, GoalUpdatedAt: now,
		Plan: normalizeMonitoringPlan(defaultGoalPlan(goal, "test"), goal), SourceMode: "browser_ocr", Status: RunRunning,
		Rooms: []Room{{ID: "room", Health: SourceWaiting}}, StartedAt: now, UpdatedAt: now}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	frame := filepath.Join(t.TempDir(), "first-frame.png")
	if err := os.WriteFile(frame, []byte("temporary frame"), 0600); err != nil {
		t.Fatal(err)
	}
	analyzer := &fixedBaselineAnalyzer{result: LiveBaseline{Status: BaselineReady, StreamType: "新闻", Summary: "主播正在播报新闻，右侧是滚动评论区", VisibleRegions: []string{"主播字幕", "滚动评论区"}, SuggestedReviewIntervalSeconds: 15, RecommendVision: true, RecommendASR: true}}
	s.SetBaselineAnalyzer(analyzer)
	evidence := SignalEvent{RoomID: "room", GoalVersion: 1, Source: SignalOCR, Text: "晚间新闻直播", ObservedAt: now}

	if err := s.runInitialBaseline(context.Background(), run.ID, evidence, frame); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls, gotGoal, gotText, gotImage := analyzer.snapshot()
	if calls != 1 || gotGoal != goal || gotText != evidence.Text || gotImage != frame {
		t.Fatalf("baseline did not receive first frame context: calls=%d goal=%q text=%q image=%q", calls, gotGoal, gotText, gotImage)
	}
	if updated.Baseline == nil || updated.Baseline.Status != BaselineReady || updated.Baseline.Summary == "" || updated.Baseline.StreamType != "新闻" {
		t.Fatalf("baseline result not persisted: %+v", updated.Baseline)
	}
	if updated.Plan.ReviewIntervalSeconds != 15 || periodicVision(updated.Plan) || updated.Plan.RecommendASR != true || !updated.Baseline.RecommendVision {
		t.Fatalf("baseline recommendations did not update the bounded plan: %+v", updated.Plan)
	}
	if len(updated.Plan.UserConsent) == 0 {
		t.Fatal("ASR recommendation must still require user consent")
	}
}

func TestInitialBaselineRecognizesLoginPageWithoutCallingModel(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	now := time.Now().UTC()
	goal := "直播有优惠券时提醒我"
	if err := repo.Create(context.Background(), &Run{ID: "login-run", Goal: goal, GoalVersion: 1, GoalUpdatedAt: now,
		Plan: normalizeMonitoringPlan(defaultGoalPlan(goal, "test"), goal), SourceMode: "browser_ocr", Status: RunRunning,
		Rooms: []Room{{ID: "room", Health: SourceWaiting}}, StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	analyzer := &fixedBaselineAnalyzer{result: LiveBaseline{Status: BaselineReady, Summary: "should not be used"}}
	s.SetBaselineAnalyzer(analyzer)
	if err := s.runInitialBaseline(context.Background(), "login-run", SignalEvent{RoomID: "room", GoalVersion: 1, Source: SignalOCR, Text: "请完成安全验证"}, "unused-frame.png"); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Get(context.Background(), "login-run")
	if err != nil {
		t.Fatal(err)
	}
	calls, _, _, _ := analyzer.snapshot()
	if calls != 0 || updated.Baseline == nil || updated.Baseline.Status != BaselineNeedsLogin || updated.Rooms[0].Health != SourceNeedsLogin {
		t.Fatalf("login page was not routed to human: calls=%d baseline=%+v room=%+v", calls, updated.Baseline, updated.Rooms[0])
	}
}

func TestInitialBaselineFailureDoesNotPauseTextMonitoring(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	now := time.Now().UTC()
	goal := "主播公布重要消息时提醒我"
	if err := repo.Create(context.Background(), &Run{ID: "unavailable-run", Goal: goal, GoalVersion: 1, GoalUpdatedAt: now,
		Plan: normalizeMonitoringPlan(defaultGoalPlan(goal, "test"), goal), SourceMode: "browser_ocr", Status: RunRunning,
		Rooms: []Room{{ID: "room", Health: SourceWaiting}}, StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// A missing analyzer is a supported deployment mode; OCR monitoring continues.
	if err := s.runInitialBaseline(context.Background(), "unavailable-run", SignalEvent{RoomID: "room", GoalVersion: 1, Source: SignalOCR, Text: "当前节目"}, "unused-frame.png"); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Get(context.Background(), "unavailable-run")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != RunRunning || updated.Baseline == nil || updated.Baseline.Status != BaselineUnavailable || updated.Rooms[0].Health == SourceFailed {
		t.Fatalf("baseline unavailability incorrectly stopped monitoring: status=%s baseline=%+v room=%+v", updated.Status, updated.Baseline, updated.Rooms[0])
	}
}

func TestInitialBaselineTokenTruncationIsExplainedAndDoesNotPauseMonitoring(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	now := time.Now().UTC()
	goal := "主播公布重要消息时提醒我"
	if err := repo.Create(context.Background(), &Run{ID: "truncated-baseline", Goal: goal, GoalVersion: 1, GoalUpdatedAt: now,
		Plan: normalizeMonitoringPlan(defaultGoalPlan(goal, "test"), goal), SourceMode: "browser_ocr", Status: RunRunning,
		Rooms: []Room{{ID: "room", Health: SourceWaiting}}, StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s.SetBaselineAnalyzer(&fixedBaselineAnalyzer{err: errors.New("model response truncated at token limit")})
	if err := s.runInitialBaseline(context.Background(), "truncated-baseline", SignalEvent{RoomID: "room", GoalVersion: 1, Source: SignalOCR, Text: "当前节目"}, "unused-frame.png"); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Get(context.Background(), "truncated-baseline")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != RunRunning || updated.Baseline == nil || updated.Baseline.Status != BaselineUnavailable {
		t.Fatalf("truncated baseline must remain unavailable without stopping monitoring: status=%s baseline=%+v", updated.Status, updated.Baseline)
	}
	if updated.Baseline.Error != "初看回复超过模型输出上限，本次未采用初看结果；OCR 监控继续运行。" {
		t.Fatalf("unexpected truncation message: %q", updated.Baseline.Error)
	}
}

func TestBaselineChangesMinimumReviewCadenceWithoutEnablingPeriodicVision(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	now := time.Now().UTC()
	goal := "主播开始唱歌时提醒我"
	plan := normalizeMonitoringPlan(defaultGoalPlan(goal, "test"), goal)
	if err := repo.Create(context.Background(), &Run{ID: "cadence-run", Goal: goal, GoalVersion: 1, GoalUpdatedAt: now,
		Plan: plan, Sampling: initialSampling(plan, now), SourceMode: "browser_ocr", Status: RunRunning,
		Rooms: []Room{{ID: "room", Health: SourceWaiting}}, StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s.SetBaselineAnalyzer(&fixedBaselineAnalyzer{result: LiveBaseline{Status: BaselineReady, Summary: "主播正在闲聊", SuggestedReviewIntervalSeconds: 30, RecommendVision: true}})
	if err := s.runInitialBaseline(context.Background(), "cadence-run", SignalEvent{RoomID: "room", GoalVersion: 1, Source: SignalOCR, Text: "欢迎来到直播间"}, "temporary.png"); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Get(context.Background(), "cadence-run")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Plan.ReviewIntervalSeconds != 30 || updated.Sampling.ReviewIntervalSeconds != 30 || periodicVision(updated.Plan) {
		t.Fatalf("initial inspection did not update the minimum cadence safely: plan=%+v sampling=%+v", updated.Plan, updated.Sampling)
	}
}

func TestBrowserCaptureRunsOneTimeBaselineBeforeContinuingOCR(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	s.SetFrameCapturer(&testFrameCapturer{})
	s.SetTextRecognizer(StaticTextRecognizer{Text: "当前直播节目", Confidence: .9})
	s.SetCaptureInterval(5 * time.Millisecond)
	analyzer := &fixedBaselineAnalyzer{result: LiveBaseline{Status: BaselineReady, StreamType: "聊天直播", Summary: "主播正在和观众聊天", SuggestedReviewIntervalSeconds: 30}}
	s.SetBaselineAnalyzer(analyzer)
	defer s.StopAll()
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/room", Goal: "主播开始唱歌时提醒我", SourceMode: "browser_ocr"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	var current *Run
	for time.Now().Before(deadline) {
		current, err = s.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Baseline != nil && current.Baseline.Status == BaselineReady && len(current.Events) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	calls, _, _, image := analyzer.snapshot()
	if current.Baseline == nil || current.Baseline.Status != BaselineReady || calls != 1 || len(current.Events) == 0 {
		t.Fatalf("first browser frame must be profiled once before regular OCR continues: calls=%d baseline=%+v events=%d", calls, current.Baseline, len(current.Events))
	}
	cleanupDeadline := time.Now().Add(time.Second)
	for time.Now().Before(cleanupDeadline) {
		if _, err := os.Stat(image); os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("initial screenshot should be deleted after inspection")
}
