package monitor

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

const (
	maxPendingSignals           = 8
	pendingSignalTTL            = 2 * time.Minute
	maxPendingFrameBytes  int64 = 4 << 20
	maxRetainedFrameBytes int64 = 64 << 20
)

var ErrPendingFull = errors.New("AI 判断繁忙，待处理消息已满，本条没有进入调查")
var ErrFrameBudget = errors.New("证据截图超过暂存上限，本条没有进入调查")
var ErrInputPaused = errors.New("监控采集已暂停，请处理页面状态后重新启动")

// PendingStatus describes this process's work, not recoverable checkpoints.
type PendingStatus struct {
	Processed uint64 `json:"processed"`
	Failed    uint64 `json:"failed"`
	Waiting   int    `json:"waiting"`
	Reviewing bool   `json:"reviewing"`
	Dropped   uint64 `json:"dropped"`
	Expired   uint64 `json:"expired"`
	Coalesced uint64 `json:"coalesced"`
	LastIssue string `json:"last_issue,omitempty"`
}

type pendingSignal struct {
	live       bool
	priority   int
	enqueuedAt time.Time
	event      SignalEvent
	version    uint64
	expires    time.Time
	frameBytes []byte
	cleanup    func()
}

// One small, bounded queue per monitor. The existing beginReview gate still
// owns global concurrency and cadence; this module owns waiting evidence.
type signalQueue struct {
	mu           sync.Mutex
	admissionMu  sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	wake         chan struct{}
	done         chan struct{}
	items        []pendingSignal
	version      uint64
	closed       bool
	activeCancel context.CancelFunc
	activeEvent  SignalEvent
	status       PendingStatus
	now          func() time.Time
	process      func(context.Context, pendingSignal) error
	changed      func()
	durable      DurablePendingQueue
	runID        string
}

func newSignalQueue(ctx context.Context, version uint64, process func(context.Context, pendingSignal) error, changed func(), durable DurablePendingQueue, runID string) *signalQueue {
	ctx, cancel := context.WithCancel(ctx)
	q := &signalQueue{ctx: ctx, cancel: cancel, version: version, wake: make(chan struct{}, 1), done: make(chan struct{}), now: time.Now, process: process, changed: changed, durable: durable, runID: runID}
	go q.loop()
	return q
}

func (q *signalQueue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *signalQueue) notifyAccepted(event SignalEvent) {
	q.mu.Lock()
	if q.activeCancel != nil && !event.ObservedAt.Before(q.activeEvent.ObservedAt) {
		priority := pendingPriority(event)
		activePriority := pendingPriority(q.activeEvent)
		if (priority > activePriority && priority > 0) || numericRevision(q.activeEvent, event) {
			q.activeCancel()
		}
	}
	q.mu.Unlock()
	q.notify()
}
func (q *signalQueue) stats() PendingStatus {
	q.mu.Lock()
	s := q.status
	s.Waiting = len(q.items)
	durable, runID := q.durable, q.runID
	q.mu.Unlock()
	if durable != nil {
		if waiting, reviewing, err := durable.PendingCounts(context.Background(), runID); err == nil {
			s.Waiting, s.Reviewing = waiting, reviewing
		}
	}
	return s
}
func disposeSignals(items []pendingSignal) {
	for _, item := range items {
		if item.cleanup != nil {
			item.cleanup()
		}
	}
}

func (q *signalQueue) offer(item pendingSignal) error {
	item.priority = pendingPriority(item.event)
	var discarded []pendingSignal
	q.mu.Lock()
	if q.closed || q.ctx.Err() != nil || item.version < q.version {
		q.mu.Unlock()
		return ErrInputPaused
	}
	if !q.now().Before(item.expires) {
		q.status.Expired++
		q.status.LastIssue = "观测到达时已超过执行有效期；历史仍保留"
		q.mu.Unlock()
		disposeSignals([]pendingSignal{item})
		q.notify()
		q.changed()
		return nil
	}
	if item.version > q.version {
		discarded, q.items = q.items, nil
		q.version = item.version
		if q.activeCancel != nil {
			q.activeCancel()
		}
	}
	if !item.live && numericRevision(q.activeEvent, item.event) {
		item.priority = 2
	}
	if q.activeCancel != nil && !item.event.ObservedAt.Before(q.activeEvent.ObservedAt) && ((item.priority > pendingPriority(q.activeEvent) && item.priority > 0) || (!item.live && numericRevision(q.activeEvent, item.event))) {
		q.activeCancel()
	}
	kept := q.items[:0]
	for _, old := range q.items {
		if !q.now().Before(old.expires) {
			discarded = append(discarded, old)
			q.status.Expired++
		} else if old.event.Source == SignalFrame || samePendingObservation(old.event, item.event) || numericRevision(old.event, item.event) || (item.live && old.live && old.priority == 0 && item.priority == 0 && old.event.RoomID == item.event.RoomID && old.event.Source == SignalOCR && item.event.Source == SignalOCR) {
			if !item.live && numericRevision(old.event, item.event) {
				item.priority = 2
			}
			if samePendingObservation(old.event, item.event) && item.enqueuedAt.IsZero() {
				item.enqueuedAt = old.enqueuedAt
			}
			// A new background frame replaces the old background; a text
			// candidate also takes precedence over a waiting background tick.
			discarded = append(discarded, old)
			q.status.Coalesced++
		} else {
			kept = append(kept, old)
		}
	}
	q.items = kept
	if len(q.items) >= maxPendingSignals && item.priority > 0 {
		worst := -1
		for i, queued := range q.items {
			score := queued.priority + int(max(0, q.now().Sub(queued.enqueuedAt))/(30*time.Second))
			if score < item.priority && (worst < 0 || queued.priority < q.items[worst].priority) {
				worst = i
			}
		}
		if worst >= 0 {
			discarded = append(discarded, q.items[worst])
			q.items = append(q.items[:worst], q.items[worst+1:]...)
			q.status.Dropped++
			q.status.LastIssue = "队列已满，较低优先级任务让位于转折更新；原观测仍保留"
		}
	}
	var err error
	if len(q.items) >= maxPendingSignals {
		q.status.Dropped++
		q.status.LastIssue = ErrPendingFull.Error()
		err = ErrPendingFull
	} else {
		if item.enqueuedAt.IsZero() {
			item.enqueuedAt = q.now()
		}
		q.items = append(q.items, item)
	}
	q.mu.Unlock()
	disposeSignals(discarded)
	q.notify()
	if err != nil {
		q.changed()
	}
	return err
}

func (q *signalQueue) invalidate(version uint64, closeQueue bool) {
	q.admissionMu.Lock()
	defer q.admissionMu.Unlock()
	q.mu.Lock()
	if version < q.version && !closeQueue {
		q.mu.Unlock()
		return
	}
	q.version = version
	items := q.items
	q.items = nil
	if q.activeCancel != nil {
		q.activeCancel()
	}
	if closeQueue {
		q.closed = true
		q.cancel()
	}
	q.mu.Unlock()
	disposeSignals(items)
	q.notify()
	q.changed()
}

func (q *signalQueue) accepting() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.closed && q.ctx.Err() == nil
}

// Cancellation/correction gets a small head start. Every 30 seconds of waiting
// adds one rank; ties use the earliest deadline then original arrival order.
func (q *signalQueue) nextIndex(now time.Time) int {
	best := 0
	score := func(item pendingSignal) int {
		return item.priority + int(max(0, now.Sub(item.enqueuedAt))/(30*time.Second))
	}
	for i := 1; i < len(q.items); i++ {
		if score(q.items[i]) > score(q.items[best]) || (score(q.items[i]) == score(q.items[best]) && q.items[i].expires.Before(q.items[best].expires)) {
			best = i
		}
	}
	return best
}

func (q *signalQueue) loop() {
	defer close(q.done)
	if q.durable != nil {
		q.durableLoop()
		return
	}
	for {
		q.mu.Lock()
		if q.closed || q.ctx.Err() != nil {
			q.mu.Unlock()
			return
		}
		if len(q.items) == 0 {
			q.mu.Unlock()
			select {
			case <-q.ctx.Done():
				return
			case <-q.wake:
			}
			continue
		}
		index := q.nextIndex(q.now())
		item := q.items[index]
		q.items = append(q.items[:index], q.items[index+1:]...)
		if !q.now().Before(item.expires) {
			q.status.Expired++
			q.status.LastIssue = "消息等待过久，已跳过"
			q.mu.Unlock()
			disposeSignals([]pendingSignal{item})
			q.changed()
			continue
		}
		// Cadence/global-slot waiting and model execution have separate
		// deadlines, so a newer coalesced observation can replace the trigger.
		ctx, cancel := context.WithCancel(q.ctx)
		q.activeCancel = cancel
		q.activeEvent = item.event
		q.status.Reviewing = true
		q.mu.Unlock()
		q.changed()
		err := q.process(ctx, item)
		cancel()
		disposeSignals([]pendingSignal{item})
		q.mu.Lock()
		q.activeCancel = nil
		q.status.Reviewing = false
		if err == nil {
			q.status.Processed++
		}
		if errors.Is(err, context.DeadlineExceeded) {
			q.status.Expired++
			q.status.LastIssue = "消息已过期，未生成新提醒"
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrGoalChanged) && !errors.Is(err, context.DeadlineExceeded) {
			q.status.Failed++
			q.status.LastIssue = "本次判断未完成，继续观察新消息"
		}
		q.mu.Unlock()
		q.changed()
	}
}

func (q *signalQueue) durableLoop() {
	for {
		q.mu.Lock()
		if q.closed || q.ctx.Err() != nil {
			q.mu.Unlock()
			return
		}
		version := q.version
		q.mu.Unlock()
		item, found, err := q.durable.ClaimPending(q.ctx, q.runID, version, q.now())
		if err != nil {
			if q.ctx.Err() != nil {
				return
			}
			log.Printf("claim durable pending signal failed: monitor=%s error=%q", q.runID, err)
			if !wait(q.ctx, 250*time.Millisecond) {
				return
			}
			continue
		}
		if !found {
			select {
			case <-q.ctx.Done():
				return
			case <-q.wake:
			}
			continue
		}
		ctx, cancel := context.WithCancel(q.ctx)
		q.mu.Lock()
		q.activeCancel, q.activeEvent, q.status.Reviewing = cancel, item.event, true
		q.mu.Unlock()
		q.changed()
		processErr := q.process(ctx, item)
		cancel()
		if !(errors.Is(processErr, context.Canceled) && q.ctx.Err() != nil) {
			if finishErr := q.durable.FinishPending(context.Background(), q.runID, item.event.ID, false); finishErr != nil {
				log.Printf("finish durable pending signal failed: monitor=%s signal=%s error=%q", q.runID, item.event.ID, finishErr)
			}
		}
		q.mu.Lock()
		q.activeCancel = nil
		q.status.Reviewing = false
		if processErr == nil {
			q.status.Processed++
		}
		if errors.Is(processErr, context.DeadlineExceeded) {
			q.status.Expired++
			q.status.LastIssue = "消息已过期，未生成新提醒"
		}
		if processErr != nil && !errors.Is(processErr, context.Canceled) && !errors.Is(processErr, ErrGoalChanged) && !errors.Is(processErr, context.DeadlineExceeded) {
			q.status.Failed++
			q.status.LastIssue = "本次判断未完成，继续观察新消息"
		}
		q.mu.Unlock()
		q.changed()
	}
}

func (q *signalQueue) latestLiveTrigger(event SignalEvent, version uint64) (SignalEvent, func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := len(q.items) - 1; i >= 0; i-- {
		item := q.items[i]
		if item.live && item.version == version && item.priority == 0 && item.event.Source == SignalOCR && item.event.RoomID == event.RoomID && !item.event.ObservedAt.Before(event.ObservedAt) {
			q.items = append(q.items[:i], q.items[i+1:]...)
			q.activeEvent = item.event
			q.status.Coalesced++
			return item.event, item.cleanup
		}
	}
	return event, nil
}

// SubmitSignal returns after admission, not model completion. Its accepted
// work survives an HTTP client disconnect and is cancelled by the monitor.
func (s *Service) SubmitSignal(ctx context.Context, runID string, event SignalEvent) error {
	_, reviewer, _ := s.agentConfig()
	if reviewer == nil {
		return s.Ingest(ctx, runID, event)
	}
	snapshot, err := s.repository.Get(ctx, runID)
	if err != nil {
		return err
	}
	if classify(event.Text, snapshot.Goal) == "human_verification" {
		err = s.Ingest(ctx, runID, event)
		if err == nil {
			s.closePending(runID)
		}
		return err
	}
	s.mu.Lock()
	q := s.pending[runID]
	s.mu.Unlock()
	if q == nil {
		return ErrInputPaused
	}
	q.admissionMu.Lock()
	defer q.admissionMu.Unlock()
	if !q.accepting() {
		return ErrInputPaused
	}
	current, err := s.repository.Get(ctx, runID)
	if err != nil {
		return err
	}
	if current.Status != RunRunning {
		return ErrInvalidTransition
	}
	if current.GoalVersion != snapshot.GoalVersion {
		return ErrGoalChanged
	}
	if len(current.Rooms) > 0 && (current.Rooms[0].Health == SourceNeedsLogin || current.Rooms[0].Health == SourceFailed) {
		return ErrInputPaused
	}
	if _, ok := s.repository.(DurablePendingQueue); ok {
		frameBytes, frameErr := readPendingFrameBytes(event.FramePath)
		if frameErr != nil {
			return frameErr
		}
		item := &pendingSignal{
			version: current.GoalVersion, expires: signalDeadline(event),
			enqueuedAt: time.Now().UTC(), live: current.SourceMode == "browser_ocr", frameBytes: frameBytes,
		}
		_, accepted, duplicate, err := s.acceptSignalAndEnqueue(ctx, runID, event, item)
		if err != nil || duplicate {
			return err
		}
		q.notifyAccepted(accepted)
		return nil
	}
	snapshot, event, duplicate, err := s.acceptSignal(ctx, runID, event)
	if err != nil || duplicate {
		return err
	}
	if snapshot.GoalVersion > 1 && event.ObservedAt.Before(snapshot.GoalUpdatedAt) {
		s.rollbackSignal(runID, event)
		return ErrGoalChanged
	}
	cleanup, err := s.retainFrame(&event)
	if err != nil {
		q.mu.Lock()
		q.status.Dropped++
		q.status.LastIssue = err.Error()
		q.mu.Unlock()
		s.rollbackSignal(runID, event)
		s.onUpdate()
		return err
	}
	expires := signalDeadline(event)
	if err = q.offer(pendingSignal{event: event, version: snapshot.GoalVersion, expires: expires, cleanup: cleanup, live: snapshot.SourceMode == "browser_ocr"}); err != nil {
		cleanup()
		s.rollbackSignal(runID, event)
	}
	return err
}

func readPendingFrameBytes(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxPendingFrameBytes {
		return nil, ErrFrameBudget
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() {
		return nil, ErrFrameBudget
	}
	return data, nil
}

func (s *Service) newPendingQueue(runCtx context.Context, runID string, version uint64) *signalQueue {
	durable, _ := s.repository.(DurablePendingQueue)
	q := newSignalQueue(runCtx, version, func(workCtx context.Context, item pendingSignal) error {
		if len(item.frameBytes) > 0 {
			file, err := os.CreateTemp("", "liveguard-pending-frame-*.png")
			if err != nil {
				return err
			}
			path := file.Name()
			if _, err := file.Write(item.frameBytes); err != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return err
			}
			if err := file.Close(); err != nil {
				_ = os.Remove(path)
				return err
			}
			item.event.FramePath = path
			defer os.Remove(path)
		}
		current, err := s.repository.Get(workCtx, runID)
		if err != nil {
			return err
		}
		if current.Status != RunRunning || current.GoalVersion != item.version {
			return ErrGoalChanged
		}
		return s.processAccepted(workCtx, runID, current, item.event, true, item.priority)
	}, s.onUpdate, durable, runID)
	return q
}

// A rejected signal has no accepted receipt, so the same idempotency key can
// be retried after backpressure clears.
func (s *Service) rollbackSignal(runID string, event SignalEvent) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		if run.Status != RunRunning || run.GoalVersion != event.GoalVersion {
			return nil
		}
		receipts := run.EventReceipts[:0]
		for _, r := range run.EventReceipts {
			if r.ID != event.ID {
				receipts = append(receipts, r)
			}
		}
		run.EventReceipts = receipts
		events := run.Events[:0]
		for _, e := range run.Events {
			if e.ID != event.ID {
				events = append(events, e)
			}
		}
		run.Events = events
		return nil
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("failed to roll back unqueued signal: monitor=%s signal=%s err=%v", runID, event.ID, err)
	}
}

// Copy caller-owned files before returning. Never delete an uploaded file or
// capture path still owned by its caller; only this module's own temp copy.
func (s *Service) retainFrame(event *SignalEvent) (func(), error) {
	if event.FramePath == "" {
		return func() {}, nil
	}
	in, err := os.Open(event.FramePath)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxPendingFrameBytes {
		return nil, ErrFrameBudget
	}
	s.mu.Lock()
	if s.retainedFrameBytes+info.Size() > maxRetainedFrameBytes {
		s.mu.Unlock()
		return nil, ErrFrameBudget
	}
	s.retainedFrameBytes += info.Size()
	s.mu.Unlock()
	var once sync.Once
	var ownedPath string
	cleanup := func() {
		once.Do(func() {
			if ownedPath != "" {
				_ = os.Remove(ownedPath)
			}
			s.mu.Lock()
			s.retainedFrameBytes -= info.Size()
			s.mu.Unlock()
		})
	}
	out, err := os.CreateTemp("", "liveguard-pending-*.png")
	if err != nil {
		cleanup()
		return nil, err
	}
	ownedPath = out.Name()
	n, copyErr := io.Copy(out, io.LimitReader(in, info.Size()+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n != info.Size() {
		cleanup()
		return nil, ErrFrameBudget
	}
	event.FramePath = ownedPath
	return cleanup, nil
}

func (s *Service) awaitReview(ctx context.Context, runID string, seconds int) (func(), error) {
	return s.awaitFairReview(ctx, runID, seconds)
}

func (s *Service) closePending(runID string) {
	s.mu.Lock()
	q := s.pending[runID]
	s.mu.Unlock()
	if q != nil {
		q.mu.Lock()
		version := q.version
		q.mu.Unlock()
		q.invalidate(0, true)
		if q.durable != nil {
			if err := q.durable.RecoverPending(context.Background(), runID, version, q.now()); err != nil {
				log.Printf("requeue durable pending signals after queue close failed: monitor=%s error=%q", runID, err)
			}
		}
	}
}
