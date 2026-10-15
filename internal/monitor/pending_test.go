package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type pendingProbeReviewer struct {
	inputs  chan UnexpectedEventInput
	release chan struct{}
	calls   atomic.Int32
	frames  chan string
}

type captureProbe struct {
	ticks chan int
	count atomic.Int32
	err   error
}

func (c *captureProbe) Capture(ctx context.Context, _, _, path string) error {
	n := int(c.count.Add(1))
	select {
	case c.ticks <- n:
	case <-ctx.Done():
		return ctx.Err()
	}
	if c.err != nil {
		return c.err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("frame-%d", n)), 0600)
}
func (*captureProbe) Close(string) {}

type numberedRecognizer struct{ count atomic.Int32 }

func (r *numberedRecognizer) Recognize(context.Context, VideoFrame) (SignalEvent, error) {
	number := r.count.Add(1)
	labels := []string{"一", "二", "三", "四", "五", "六", "七", "八", "九", "十"}
	return SignalEvent{Source: SignalOCR, Text: fmt.Sprintf("新消息%s", labels[(number-1)%int32(len(labels))]), Confidence: .9}, nil
}

func waitInput(t *testing.T, r *pendingProbeReviewer) UnexpectedEventInput {
	t.Helper()
	select {
	case input := <-r.inputs:
		return input
	case <-time.After(2 * time.Second):
		t.Fatal("review did not start")
		return UnexpectedEventInput{}
	}
}
func waitQueue(t *testing.T, q *signalQueue) {
	t.Helper()
	select {
	case <-q.done:
	case <-time.After(2 * time.Second):
		t.Fatal("queue worker did not exit")
	}
}

func TestCaptureContinuesDuringSlowModelAndStopReleasesFrames(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	c := &captureProbe{ticks: make(chan int, 30)}
	r := &pendingProbeReviewer{inputs: make(chan UnexpectedEventInput, 30), release: make(chan struct{})}
	s.SetFrameCapturer(c)
	s.SetTextRecognizer(&numberedRecognizer{})
	s.SetCaptureInterval(2 * time.Millisecond)
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注新消息", SourceMode: "browser_ocr"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.StopAll()
	first := waitInput(t, r)
	for n := 0; n < 3; n++ {
		select {
		case <-c.ticks:
		case <-time.After(time.Second):
			t.Fatal("capture blocked behind model")
		}
	}
	if r.calls.Load() != 1 {
		t.Fatal("model calls overlapped within one monitor")
	}
	if first.TriggerFrame != "" {
		t.Fatalf("OCR review must not receive an automatically retained screenshot: %q", first.TriggerFrame)
	}
	s.mu.Lock()
	q := s.pending[run.ID]
	s.mu.Unlock()
	if _, err = s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	waitQueue(t, q)
	s.mu.Lock()
	retained := s.retainedFrameBytes
	s.mu.Unlock()
	if retained != 0 {
		t.Fatalf("retained %d evidence bytes after stop", retained)
	}
	current, _ := s.Get(context.Background(), run.ID)
	if len(current.Alerts) != 0 || current.Status != RunStopped {
		t.Fatal("stop allowed a late alert")
	}
}

func TestQueuedFrameSurvivesCallerDeletionAndDuplicateSubmission(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	r := &pendingProbeReviewer{inputs: make(chan UnexpectedEventInput, 5), release: make(chan struct{}), frames: make(chan string, 5)}
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注新消息", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.repository.Update(context.Background(), run.ID, func(run *Run) error { run.Sampling.ReviewIntervalSeconds = 0; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "one", Source: SignalOCR, Text: "first"}); err != nil {
		t.Fatal(err)
	}
	waitInput(t, r)
	<-r.frames
	path := filepath.Join(t.TempDir(), "caller.png")
	if err = os.WriteFile(path, []byte("original-frame"), 0600); err != nil {
		t.Fatal(err)
	}
	event := SignalEvent{ID: "two", Source: SignalOCR, Text: "new event", FramePath: path}
	if err = s.SubmitSignal(context.Background(), run.ID, event); err != nil {
		t.Fatal(err)
	}
	if err = s.SubmitSignal(context.Background(), run.ID, event); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	close(r.release)
	input := waitInput(t, r)
	if data := <-r.frames; data != "" {
		t.Fatalf("queued OCR review unexpectedly received a screenshot: %q", data)
	}
	if input.RecentEvidence[0].ID != "two" || input.TriggerFrame != "" {
		t.Fatal("lost trigger or passed the caller's image into the OCR review")
	}
	current, _ := s.Get(context.Background(), run.ID)
	if len(current.Events) != 2 || r.calls.Load() != 2 {
		t.Fatal("duplicate submission produced extra work")
	}
}

func TestRejectedFullQueueCanRetrySameSignalID(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	r := &pendingProbeReviewer{inputs: make(chan UnexpectedEventInput, 16), release: make(chan struct{})}
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注新消息", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.repository.Update(context.Background(), run.ID, func(run *Run) error {
		run.Plan.ReviewIntervalSeconds = 0
		run.Sampling.ReviewIntervalSeconds = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "active", Source: SignalOCR, Text: "正在判断"}); err != nil {
		t.Fatal(err)
	}
	waitInput(t, r)
	for i := 0; i < maxPendingSignals; i++ {
		labels := []string{"甲", "乙", "丙", "丁", "戊", "己", "庚", "辛"}
		if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: fmt.Sprintf("queued-%d", i), Source: SignalOCR, Text: "排队消息" + labels[i]}); err != nil {
			t.Fatalf("queue item %d: %v", i, err)
		}
	}
	retry := SignalEvent{ID: "retry-after-full", Source: SignalOCR, Text: "队列满时被拒绝的消息"}
	if err = s.SubmitSignal(context.Background(), run.ID, retry); !errors.Is(err, ErrPendingFull) {
		t.Fatalf("expected full queue, got %v", err)
	}
	current, err := s.repository.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range current.Events {
		if event.ID == retry.ID {
			t.Fatal("rejected signal remained persisted")
		}
	}
	for _, receipt := range current.EventReceipts {
		if receipt.ID == retry.ID {
			t.Fatal("rejected signal retained idempotency receipt")
		}
	}
	close(r.release)
	for i := 0; i < maxPendingSignals; i++ {
		input := waitInput(t, r)
		if input.RecentEvidence[0].ID == retry.ID {
			t.Fatal("rejected signal was unexpectedly processed")
		}
	}
	if err = s.SubmitSignal(context.Background(), run.ID, retry); err != nil {
		t.Fatalf("retry after capacity became available: %v", err)
	}
	if input := waitInput(t, r); input.RecentEvidence[0].ID != retry.ID {
		t.Fatal("retry with same idempotency key was not processed")
	}
}

func TestOCRDeduperDoesNotCommitWhenQueueRejects(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注新消息", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.StopAll()
	deduper := &OCRLineDeduper{}
	calls := 0
	submit := func(context.Context, string, SignalEvent) error {
		calls++
		if calls == 1 {
			return ErrPendingFull
		}
		return nil
	}
	event := SignalEvent{ID: "same-ocr", Source: SignalOCR, Text: "主播公布新消息", ObservedAt: time.Now()}
	if err = s.handleCapturedFrame(context.Background(), run.ID, event, deduper, submit); !errors.Is(err, ErrPendingFull) {
		t.Fatalf("first submit should expose backpressure, got %v", err)
	}
	if err = s.handleCapturedFrame(context.Background(), run.ID, event, deduper, submit); err != nil {
		t.Fatalf("same OCR should retry after admission failure: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected OCR retry, got %d submissions", calls)
	}
}

func TestPendingBoundsExpiryBackgroundCoalescingAndGoalInvalidation(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &signalQueue{ctx: ctx, cancel: cancel, version: 1, wake: make(chan struct{}, 1), now: func() time.Time { return now }, changed: func() {}}
	var released atomic.Int32
	item := func(id string, source SignalSource) pendingSignal {
		return pendingSignal{event: SignalEvent{ID: id, RoomID: id, Source: source, Text: id}, version: 1, expires: now.Add(time.Minute), cleanup: func() { released.Add(1) }}
	}
	if err := q.offer(item("frame-old", SignalFrame)); err != nil {
		t.Fatal(err)
	}
	if err := q.offer(item("frame-new", SignalFrame)); err != nil {
		t.Fatal(err)
	}
	if q.stats().Waiting != 1 || released.Load() != 1 {
		t.Fatal("background frames did not coalesce")
	}
	for i := 0; i < maxPendingSignals; i++ {
		if err := q.offer(item(fmt.Sprint(i), SignalOCR)); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.offer(item("overflow", SignalASR)); !errors.Is(err, ErrPendingFull) {
		t.Fatalf("unbounded queue: %v", err)
	}
	if q.stats().Waiting != maxPendingSignals || q.stats().Dropped != 1 {
		t.Fatal("wrong bounded admission state")
	}
	now = now.Add(2 * time.Minute)
	if err := q.offer(item("after-expiry", SignalOCR)); err != nil {
		t.Fatal(err)
	}
	if q.stats().Expired != maxPendingSignals {
		t.Fatal("expired items remained queued")
	}
	q.invalidate(2, false)
	if q.stats().Waiting != 0 {
		t.Fatal("old goal work survived invalidation")
	}
	if err := q.offer(item("old-goal", SignalOCR)); !errors.Is(err, ErrInputPaused) {
		t.Fatal("late old-goal admission succeeded")
	}
}

func TestAcceptedSignalDoesNotInvalidateUIUntilQueueStateChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var changed atomic.Int32
	q := &signalQueue{ctx: ctx, cancel: cancel, version: 1, wake: make(chan struct{}, 1), now: time.Now, changed: func() { changed.Add(1) }}
	item := func(id string) pendingSignal {
		return pendingSignal{event: SignalEvent{ID: id, Source: SignalOCR, Text: "文字" + id}, version: 1, expires: time.Now().Add(time.Minute)}
	}
	if err := q.offer(item("one")); err != nil {
		t.Fatal(err)
	}
	if changed.Load() != 0 {
		t.Fatal("accepted OCR signal should wake the worker without refreshing the UI")
	}
	q.mu.Lock()
	for i := 1; i < maxPendingSignals; i++ {
		q.items = append(q.items, item(fmt.Sprint(i)))
	}
	q.mu.Unlock()
	if err := q.offer(item("overflow")); !errors.Is(err, ErrPendingFull) {
		t.Fatalf("expected bounded queue rejection, got %v", err)
	}
	if changed.Load() != 1 {
		t.Fatalf("visible backpressure should invalidate UI once; got %d", changed.Load())
	}
}

func TestGoalChangeCancelsQueuedReviewAndClearsWaitingMessages(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	r := &pendingProbeReviewer{inputs: make(chan UnexpectedEventInput, 4), release: make(chan struct{})}
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注旧消息", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "old-first", Source: SignalOCR, Text: "old"}); err != nil {
		t.Fatal(err)
	}
	waitInput(t, r)
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "old-second", Source: SignalOCR, Text: "old pending"}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateGoal(context.Background(), run.ID, "关注新的产品发布")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	q := s.pending[run.ID]
	s.mu.Unlock()
	if q.stats().Waiting != 0 {
		t.Fatal("goal change kept pending old messages")
	}
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "late", Source: SignalOCR, Text: "late", GoalVersion: run.GoalVersion}); !errors.Is(err, ErrGoalChanged) {
		t.Fatalf("late capture accepted: %v", err)
	}
	if _, err = s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	waitQueue(t, q)
	current, _ := s.Get(context.Background(), run.ID)
	if current.GoalVersion != updated.GoalVersion || len(current.Alerts) != 0 {
		t.Fatal("late results affected changed goal")
	}
}

type allBlockingReviewer struct {
	started      chan struct{}
	release      chan struct{}
	active, peak atomic.Int32
}

func (r *allBlockingReviewer) Review(ctx context.Context, _ UnexpectedEventInput) (UnexpectedEventDecision, error) {
	n := r.active.Add(1)
	defer r.active.Add(-1)
	for {
		old := r.peak.Load()
		if n <= old || r.peak.CompareAndSwap(old, n) {
			break
		}
	}
	r.started <- struct{}{}
	select {
	case <-ctx.Done():
		return UnexpectedEventDecision{}, ctx.Err()
	case <-r.release:
		return UnexpectedEventDecision{Action: "ignore", Confidence: .9}, nil
	}
}

func TestQueuedMonitorsRespectGlobalLimitAndResumeAfterSlotRelease(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	r := &allBlockingReviewer{started: make(chan struct{}, 8), release: make(chan struct{})}
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	var queues []*signalQueue
	for i := 0; i < 5; i++ {
		run, err := s.Start(context.Background(), StartInput{TargetURL: fmt.Sprintf("https://example.com/%d", i), Goal: "关注重要公告", SourceMode: "ocr_upload"})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "event", Source: SignalOCR, Text: "公告"}); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		queues = append(queues, s.pending[run.ID])
		s.mu.Unlock()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-r.started:
		case <-time.After(time.Second):
			t.Fatal("available review slots were not used")
		}
	}
	if r.peak.Load() != 4 {
		t.Fatalf("global peak=%d", r.peak.Load())
	}
	close(r.release)
	select {
	case <-r.started:
	case <-time.After(time.Second):
		t.Fatal("fifth monitor did not proceed after slot release")
	}
	s.StopAll()
	for _, q := range queues {
		waitQueue(t, q)
	}
	if r.peak.Load() > 4 {
		t.Fatal("global model concurrency limit exceeded")
	}
}

func TestCaptureFailureRetriesAndLoginWaitsForUser(t *testing.T) {
	for _, login := range []bool{false, true} {
		t.Run(fmt.Sprint(login), func(t *testing.T) {
			repo := NewMemoryRepository()
			ready := make(chan struct{})
			var once sync.Once
			s := NewService(repo, func() {
				runs, _ := repo.List(context.Background())
				if len(runs) > 0 && len(runs[0].Rooms) > 0 {
					room := runs[0].Rooms[0]
					if (login && room.Health == SourceNeedsLogin) || (!login && room.Health == SourceDelayed) {
						once.Do(func() { close(ready) })
					}
				}
			})
			defer s.StopAll()
			errMessage := "temporary capture failure"
			if login {
				errMessage = "browser needs login"
			}
			c := &captureProbe{ticks: make(chan int, 8), err: errors.New(errMessage)}
			s.SetFrameCapturer(c)
			s.SetTextRecognizer(&numberedRecognizer{})
			s.SetCaptureInterval(time.Millisecond)
			run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注公告", SourceMode: "browser_ocr"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("capture fault was not exposed")
			}
			current, _ := s.Get(context.Background(), run.ID)
			if current.Rooms[0].LastError == "" || (login && current.Rooms[0].Health != SourceNeedsLogin) {
				t.Fatal("fault shown as healthy")
			}
			if !login && current.Rooms[0].Health != SourceDelayed {
				t.Fatalf("temporary failure should retry, got %#v", current.Rooms[0])
			}
			if _, err = s.Stop(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func (r *pendingProbeReviewer) Review(ctx context.Context, input UnexpectedEventInput) (UnexpectedEventDecision, error) {
	call := r.calls.Add(1)
	if r.frames != nil {
		data, _ := os.ReadFile(input.TriggerFrame)
		r.frames <- string(data)
	}
	r.inputs <- input
	if call == 1 {
		select {
		case <-r.release:
		case <-ctx.Done():
			return UnexpectedEventDecision{}, ctx.Err()
		}
	}
	return UnexpectedEventDecision{Action: "ignore", Confidence: .9}, nil
}

// This reproduces the old loss: a second signal was received during a review,
// but releasing the model did not cause it to be reviewed without new input.
func TestBusySignalGetsAReviewWithoutAnotherInput(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	r := &pendingProbeReviewer{inputs: make(chan UnexpectedEventInput, 2), release: make(chan struct{})}
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "有重要消息提醒我", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.StopAll()
	// A zero review interval here isolates admission from intentional cooldown.
	_, err = s.repository.Update(context.Background(), run.ID, func(run *Run) error {
		run.Plan.ReviewIntervalSeconds = 0
		run.Sampling.ReviewIntervalSeconds = 0
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		first <- s.Ingest(context.Background(), run.ID, SignalEvent{ID: "first", Source: SignalOCR, Text: "第一条消息"})
	}()
	select {
	case <-r.inputs:
	case <-time.After(time.Second):
		t.Fatal("first review did not start")
	}
	if err = s.SubmitSignal(context.Background(), run.ID, SignalEvent{ID: "second", Source: SignalOCR, Text: "只出现一次的新消息"}); err != nil {
		t.Fatal(err)
	}
	close(r.release)
	if err = <-first; err != nil {
		t.Fatal(err)
	}
	// Move past the cadence boundary: this regression checks catch-up.
	s.mu.Lock()
	s.lastUnexpectedReview[run.ID] = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	select {
	case input := <-r.inputs:
		if input.RecentEvidence[0].ID != "second" {
			t.Fatal("queued trigger was lost")
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("accepted busy signal was never reviewed after the first model call finished")
	}
}
