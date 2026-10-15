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

type intermittentRepository struct {
	Repository
	failNext atomic.Bool
}

type recoveringCapturer struct{ calls atomic.Int32 }

func (c *recoveringCapturer) Capture(_ context.Context, _, _, path string) error {
	if c.calls.Add(1) <= 4 {
		return errors.New("temporary DevTools disconnect")
	}
	return os.WriteFile(path, []byte("frame"), 0600)
}
func (*recoveringCapturer) Close(string) {}

func TestCaptureRecoversAfterMoreThanThreeTemporaryFailures(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	c := &recoveringCapturer{}
	s.SetFrameCapturer(c)
	s.SetTextRecognizer(&numberedRecognizer{})
	s.SetCaptureInterval(time.Millisecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注公告", SourceMode: "browser_ocr"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, _ := s.Get(context.Background(), run.ID)
		if c.calls.Load() > 4 && current.Rooms[0].Health == SourceHealthy && current.Rooms[0].LastError == "" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("temporary failures permanently stopped capture: calls=%d", c.calls.Load())
}

func TestSnapshotReplacementRetriesSharingFailure(t *testing.T) {
	calls := 0
	err := replaceSnapshot("temporary", "snapshot", func(_, _ string) error {
		calls++
		if calls < 4 {
			return &os.LinkError{Op: "rename", Old: "temporary", New: "snapshot", Err: os.ErrPermission}
		}
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("transient snapshot lock not retried: calls=%d err=%v", calls, err)
	}
}

func TestBrowserReviewRefreshesTriggerAfterGlobalSlotWait(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	r := &pendingProbeReviewer{inputs: make(chan UnexpectedEventInput, 5), release: make(chan struct{})}
	close(r.release)
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注公告", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.repository.Update(context.Background(), run.ID, func(run *Run) error { run.SourceMode = "browser_ocr"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	// Occupy every global slot so the worker has popped its original trigger
	// but has not yet reached the model. New OCR must replace that trigger.
	for i := 0; i < maxConcurrentReviews; i++ {
		s.reviewSlots <- struct{}{}
	}
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "old", Source: SignalOCR, Text: "先前画面"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		waiting := len(s.reviewWaiters)
		s.mu.Unlock()
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not wait for global slot")
		}
		time.Sleep(time.Millisecond)
	}
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "fresh", Source: SignalOCR, Text: "当前新画面"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxConcurrentReviews; i++ {
		<-s.reviewSlots
	}
	if input := waitInput(t, r); input.RecentEvidence[0].ID != "fresh" {
		t.Fatal("global wait launched review using obsolete OCR")
	}
}

func (r *intermittentRepository) Update(ctx context.Context, id string, change func(*Run) error) (*Run, error) {
	if r.failNext.Load() {
		before, err := r.Repository.Get(ctx, id)
		if err == nil {
			probe := cloneRun(before)
			if change(probe) == nil && len(probe.Rooms) > 0 && probe.Rooms[0].Health == SourceHealthy && before.Rooms[0].Health != SourceHealthy && probe.EventSequence == before.EventSequence && r.failNext.Swap(false) {
				return nil, errors.New("rename snapshot: Access is denied")
			}
		}
	}
	return r.Repository.Update(ctx, id, change)
}

type recoveryRecognizer struct {
	repo  *intermittentRepository
	calls atomic.Int32
}

func (r *recoveryRecognizer) Recognize(context.Context, VideoFrame) (SignalEvent, error) {
	if r.calls.Add(1) == 1 {
		r.repo.failNext.Store(true)
	}
	return SignalEvent{Source: SignalOCR, Text: "画面正常", Confidence: .9}, nil
}

func TestCaptureSurvivesTransientStateWriteFailure(t *testing.T) {
	repo := &intermittentRepository{Repository: NewMemoryRepository()}
	s := NewService(repo, nil)
	defer s.StopAll()
	c := &captureProbe{ticks: make(chan int, 1000)}
	r := &recoveryRecognizer{repo: repo}
	s.SetFrameCapturer(c)
	s.SetTextRecognizer(r)
	s.SetCaptureInterval(5 * time.Millisecond)
	_, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注画面", SourceMode: "browser_ocr"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r.calls.Load() >= 3 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("capture silently stopped after a transient state write: captures=%d recognitions=%d", c.count.Load(), r.calls.Load())
}

func TestLongCapturedOCRPreservesTextWithoutSourceFailure(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	defer s.StopAll()
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注公告", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("主屏幕文字", 1000) + "\n" + strings.Repeat("聊天区消息", 1000)
	var deduper OCRLineDeduper
	err = s.ingestCapturedFrame(context.Background(), run.ID, SignalEvent{ID: "long-ocr", Source: SignalOCR, Text: text, Confidence: .9}, &deduper)
	if err != nil {
		t.Fatalf("valid long OCR became a source failure: %v", err)
	}
	page, err := repo.SearchObservations(context.Background(), run.ID, ObservationQuery{Limit: 12})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range page.Events {
		if len([]rune(e.Text)) > maxSignalTextRunes {
			t.Fatal("oversize signal persisted")
		}
		total += len([]rune(strings.ReplaceAll(e.Text, "\n", "")))
	}
	if total != len([]rune(strings.ReplaceAll(text, "\n", ""))) {
		t.Fatalf("OCR content lost: got %d runes", total)
	}
}

func TestBrowserOCRBacklogCoalescesWhileKeepingObservationLedger(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	defer s.StopAll()
	r := &pendingProbeReviewer{inputs: make(chan UnexpectedEventInput, 32), release: make(chan struct{})}
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注消息", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.Update(context.Background(), run.ID, func(run *Run) error {
		run.SourceMode = "browser_ocr"
		run.Plan.ReviewIntervalSeconds, run.Sampling.ReviewIntervalSeconds = 0, 0
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "first", Source: SignalOCR, Text: "初始消息"}); err != nil {
		t.Fatal(err)
	}
	waitInput(t, r)
	for i := 0; i < 20; i++ {
		err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: fmt.Sprintf("update-%d", i), Source: SignalOCR, Text: "新增消息" + strings.Repeat("字", i+1)})
		if err != nil {
			t.Fatalf("live OCR backlog rejected observation %d: %v", i, err)
		}
	}
	s.mu.Lock()
	q := s.pending[run.ID]
	s.mu.Unlock()
	if q.stats().Waiting > 1 {
		t.Fatalf("live snapshots accumulated instead of coalescing: %#v", q.stats())
	}
	close(r.release)
	s.mu.Lock()
	s.lastUnexpectedReview[run.ID] = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	input := waitInput(t, r)
	if input.RecentEvidence[0].ID != "update-19" {
		t.Fatal("review started with an obsolete trigger")
	}
	page, err := repo.SearchObservations(context.Background(), run.ID, ObservationQuery{Limit: 100})
	if err != nil || page.TotalMatches != 21 {
		t.Fatalf("coalescing lost searchable observations: %#v %v", page, err)
	}
}

func TestNearExpiredSignalDoesNotStartPaidReview(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	r := &testUnexpectedReviewer{decision: UnexpectedEventDecision{Action: "wait"}}
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注消息", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "stale", Source: SignalOCR, Text: "旧消息", ObservedAt: time.Now().Add(-119 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	q := s.pending[run.ID]
	s.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stats := q.stats()
		if stats.Expired > 0 {
			break
		}
		if r.calls.Load() > 0 {
			t.Fatal("paid review started with less than a second of signal lifetime")
		}
		time.Sleep(time.Millisecond)
	}
	if q.stats().Expired == 0 || r.calls.Load() != 0 {
		t.Fatalf("near-expiry admission: %#v calls=%d", q.stats(), r.calls.Load())
	}
}

func TestStopAllLeavesRecoverablePausedState(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注公告", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	s.StopAll()
	current, err := repo.Get(context.Background(), run.ID)
	if err != nil || current.Status != RunPaused || current.StopReason != "service_shutdown" || current.Rooms[0].Status != "paused" {
		t.Fatalf("shutdown left a phantom running source: %#v %v", current, err)
	}
	if _, err := s.Resume(context.Background(), run.ID); err != nil {
		t.Fatalf("shutdown state cannot be resumed: %v", err)
	}
	s.StopAll()
}

func TestSamplingTracksLocalProcessingCapacity(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	run := &Run{Goal: "关注公告", Plan: defaultGoalPlan("关注公告", "rules"), Rooms: []Room{{CaptureDurationMS: 1200, PerceptionDurationMS: 2300}}}
	if interval := s.effectiveSampling(run, time.Now()).CaptureIntervalSeconds; interval != 3 {
		t.Fatalf("sampling outpaces actual OCR: interval=%d", interval)
	}
	run.Rooms[0].CaptureDurationMS, run.Rooms[0].PerceptionDurationMS = 100, 200
	if interval := s.effectiveSampling(run, time.Now()).CaptureIntervalSeconds; interval != 1 {
		t.Fatalf("sampling failed to recover when load cleared: interval=%d", interval)
	}
}
