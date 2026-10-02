package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"liveguard/internal/domain"
)

const (
	InvestigationWaiting    = "waiting_more_evidence"
	InvestigationConfirmed  = "confirmed"
	InvestigationSuppressed = "suppressed"
	InvestigationNeedsHuman = "needs_human"
	InvestigationFailed     = "failed"
	maxActiveMonitors       = 8
	maxConcurrentReviews    = 4
	maxEventReceipts        = 2048
	eventReceiptRetention   = 24 * time.Hour
	maxSignalTextRunes      = 4000
	maxSignalAge            = 10 * time.Minute
	maxFutureSignalSkew     = 2 * time.Minute
)

var (
	ErrEventIDConflict = errors.New("event id was already used with different content")
	ErrStaleSignal     = errors.New("signal is outside the accepted time window")
)

type StartInput struct {
	TargetURL  string `json:"target_url"`
	Goal       string `json:"goal"`
	SourceMode string `json:"source_mode"`
}

// FrameCapturer produces screenshots from a live browser tab. Keeping capture
// separate from OCR lets the monitor runtime be tested without Chrome.
type FrameCapturer interface {
	Capture(context.Context, string, string, string) error
	Close(string)
}

type Service struct {
	repository  Repository
	onUpdate    func()
	replayGap   time.Duration
	recognizer  TextRecognizer
	transcriber Transcriber
	capturer    FrameCapturer
	captureGap  time.Duration
	reviewer    UnexpectedEventReviewer
	reviewGap   time.Duration
	goalPlanner GoalPlanner

	mu                   sync.Mutex
	cancels              map[string]context.CancelFunc
	lastUnexpectedReview map[string]time.Time
	contexts             map[string]context.Context
	reviewInFlight       map[string]bool
	reviewSlots          chan struct{}
	reservations         int
}

func NewService(repository Repository, onUpdate func()) *Service {
	if onUpdate == nil {
		onUpdate = func() {}
	}
	return &Service{repository: repository, onUpdate: onUpdate, replayGap: 700 * time.Millisecond, reviewGap: 15 * time.Second, goalPlanner: ruleGoalPlanner{}, cancels: map[string]context.CancelFunc{}, contexts: map[string]context.Context{}, lastUnexpectedReview: map[string]time.Time{}, reviewInFlight: map[string]bool{}, reviewSlots: make(chan struct{}, maxConcurrentReviews)}
}

// SetReplayGap makes the deterministic signal source fast in tests. The local
// demo spaces events out so the control room visibly changes over time.
func (s *Service) SetReplayGap(gap time.Duration) { s.replayGap = gap }

// SetTextRecognizer connects an optional local or remote OCR provider to the
// same Ingest boundary used by replay and manual signals.
func (s *Service) SetTextRecognizer(recognizer TextRecognizer) { s.recognizer = recognizer }

func (s *Service) SetTranscriber(transcriber Transcriber) { s.transcriber = transcriber }

func (s *Service) SetGoalPlanner(planner GoalPlanner) {
	if planner != nil {
		s.goalPlanner = planner
	}
}

func (s *Service) SetFrameCapturer(capturer FrameCapturer) { s.capturer = capturer }

// SetUnexpectedEventReviewer lets the model spot high-salience moments outside
// the user's explicit goal. Calls are throttled per monitor to control cost.
func (s *Service) SetUnexpectedEventReviewer(reviewer UnexpectedEventReviewer, minInterval time.Duration) {
	s.reviewer = reviewer
	if minInterval > 0 {
		s.reviewGap = minInterval
	}
}

func (s *Service) SetCaptureInterval(interval time.Duration) {
	if interval > 0 {
		s.captureGap = interval
	}
}

func (s *Service) Start(ctx context.Context, input StartInput) (*Run, error) {
	input.TargetURL = strings.TrimSpace(input.TargetURL)
	input.Goal = strings.TrimSpace(input.Goal)
	input.SourceMode = strings.TrimSpace(input.SourceMode)
	if input.SourceMode == "" {
		input.SourceMode = "demo_replay"
	}
	if input.SourceMode != "demo_replay" && input.SourceMode != "ocr_upload" && input.SourceMode != "browser_ocr" {
		return nil, errors.New("source mode must be demo_replay, ocr_upload, or browser_ocr")
	}
	if input.SourceMode == "browser_ocr" && (s.capturer == nil || s.recognizer == nil) {
		return nil, errors.New("automatic live monitoring needs Chrome capture and Tesseract OCR configured")
	}
	parsed, err := url.Parse(input.TargetURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("target URL must be a valid http/https address")
	}
	if utf8.RuneCountInString(input.Goal) < 2 || utf8.RuneCountInString(input.Goal) > 240 {
		return nil, errors.New("monitoring goal must contain 2-240 characters")
	}
	s.mu.Lock()
	if len(s.cancels)+s.reservations >= maxActiveMonitors {
		s.mu.Unlock()
		return nil, ErrCapacity
	}
	s.reservations++
	s.mu.Unlock()
	reserved := true
	defer func() {
		if reserved {
			s.mu.Lock()
			s.reservations--
			s.mu.Unlock()
		}
	}()
	plan, planErr := s.goalPlanner.Plan(ctx, input.Goal)
	if planErr != nil {
		log.Printf("monitor goal planning failed; using bounded local plan: %v", planErr)
		plan = defaultGoalPlan(input.Goal, "本地规则兜底")
	}
	plan = normalizeMonitoringPlan(plan, input.Goal)
	if catalog, ok := s.reviewer.(interface{ AdditionalToolNames() []string }); ok {
		plan.Tools = append(plan.Tools, catalog.AdditionalToolNames()...)
	}
	if s.captureGap >= time.Second {
		plan.CaptureIntervalSeconds = durationSecondsCeil(s.captureGap)
	}
	if s.reviewGap > time.Duration(plan.ReviewIntervalSeconds)*time.Second {
		plan.ReviewIntervalSeconds = durationSecondsCeil(s.reviewGap)
	}
	plan.Summary = summarizeMonitoringPlan(plan)

	now := time.Now().UTC()
	roomID := domain.NewID("room")
	roomName := roomNameFromURL(parsed)
	run := &Run{
		ID: domain.NewID("monitor"), Name: roomName, TargetURL: input.TargetURL, Goal: input.Goal,
		Plan: plan, SourceMode: input.SourceMode, Status: RunRunning, StartedAt: now, UpdatedAt: now, Version: 1,
		Rooms: []Room{{ID: roomID, Name: roomName, Status: "listening", Health: SourceWaiting}},
	}
	if err := s.repository.Create(ctx, run); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.reservations--
	reserved = false
	s.cancels[run.ID] = cancel
	s.contexts[run.ID] = runCtx
	s.mu.Unlock()
	s.onUpdate()
	if input.SourceMode == "demo_replay" {
		go s.replay(runCtx, run.ID, demoSignals(run))
	} else if input.SourceMode == "browser_ocr" {
		go s.captureLoop(runCtx, run.ID, input.TargetURL, roomID)
	}
	return run, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Run, error) {
	run, err := s.repository.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if run.Status == RunRunning && run.SourceMode == "browser_ocr" && len(run.Rooms) > 0 {
		interval := time.Duration(run.Plan.CaptureIntervalSeconds) * time.Second
		if interval <= 0 {
			interval = 12 * time.Second
		}
		lastSignal := run.Rooms[0].LastSeen
		if lastSignal.IsZero() {
			lastSignal = run.StartedAt
		}
		if time.Since(lastSignal) > 2*interval && (run.Rooms[0].Health == SourceWaiting || run.Rooms[0].Health == SourceHealthy) {
			run.Rooms[0].Health = SourceDelayed
		}
	}
	return run, nil
}

// UpdateGoal applies an explicit user-edited monitoring goal and replans its
// bounded sensing policy. The model may suggest a plan, but only the user's
// submitted goal is committed; no model-generated message is auto-approved.
func (s *Service) UpdateGoal(ctx context.Context, id, goal string) (*Run, error) {
	goal = strings.TrimSpace(goal)
	if utf8.RuneCountInString(goal) < 2 || utf8.RuneCountInString(goal) > 240 {
		return nil, errors.New("monitoring goal must contain 2-240 characters")
	}
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Status != RunRunning {
		return nil, ErrInvalidTransition
	}
	plan, planErr := s.goalPlanner.Plan(ctx, goal)
	if planErr != nil {
		log.Printf("monitor goal update planning failed; using bounded local plan: %v", planErr)
		plan = defaultGoalPlan(goal, "本地规则兜底")
	}
	plan = normalizeMonitoringPlan(plan, goal)
	if catalog, ok := s.reviewer.(interface{ AdditionalToolNames() []string }); ok {
		plan.Tools = append(plan.Tools, catalog.AdditionalToolNames()...)
	}
	if s.captureGap >= time.Second {
		plan.CaptureIntervalSeconds = durationSecondsCeil(s.captureGap)
	}
	if s.reviewGap > time.Duration(plan.ReviewIntervalSeconds)*time.Second {
		plan.ReviewIntervalSeconds = durationSecondsCeil(s.reviewGap)
	}
	plan.Summary = summarizeMonitoringPlan(plan)
	updated, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status != RunRunning {
			return ErrInvalidTransition
		}
		run.Goal = goal
		run.Plan = plan
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
	return updated, err
}

func (s *Service) List(ctx context.Context) ([]Summary, error) {
	runs, err := s.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(runs))
	for _, run := range runs {
		summaries = append(summaries, run.Summary())
	}
	return summaries, nil
}

func (s *Service) Stop(ctx context.Context, id string) (*Run, error) {
	s.mu.Lock()
	cancel := s.cancels[id]
	runCtx := s.contexts[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	completedAt := time.Now().UTC()
	run, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status != RunRunning {
			return nil
		}
		run.Status = RunStopped
		run.CompletedAt = &completedAt
		for i := range run.Rooms {
			run.Rooms[i].Status = "stopped"
		}
		return nil
	})
	if err == nil {
		s.mu.Lock()
		if s.contexts[id] == runCtx {
			delete(s.cancels, id)
			delete(s.contexts, id)
			delete(s.lastUnexpectedReview, id)
			delete(s.reviewInFlight, id)
		}
		s.mu.Unlock()
	}
	if err == nil {
		s.onUpdate()
	}
	return run, err
}

func (s *Service) StopAll() {
	s.mu.Lock()
	var ids []string
	var cancels []context.CancelFunc
	for id, cancel := range s.cancels {
		ids = append(ids, id)
		cancels = append(cancels, cancel)
		delete(s.cancels, id)
		delete(s.contexts, id)
		delete(s.lastUnexpectedReview, id)
		delete(s.reviewInFlight, id)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if closer, ok := s.capturer.(interface{ CloseAll() }); ok {
		closer.CloseAll()
		return
	}
	if s.capturer != nil {
		for _, id := range ids {
			s.capturer.Close(id)
		}
	}
}

func (s *Service) finishRuntime(runID string, runCtx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.contexts[runID] != runCtx {
		return
	}
	delete(s.cancels, runID)
	delete(s.contexts, runID)
	delete(s.lastUnexpectedReview, runID)
	delete(s.reviewInFlight, runID)
}

func (s *Service) captureLoop(ctx context.Context, runID, targetURL, roomID string) {
	defer func() {
		s.finishRuntime(runID, ctx)
	}()
	captureInterval := s.captureGap
	if captureInterval <= 0 {
		if run, err := s.repository.Get(ctx, runID); err == nil && run.Plan.CaptureIntervalSeconds > 0 {
			captureInterval = time.Duration(run.Plan.CaptureIntervalSeconds) * time.Second
		} else {
			captureInterval = 12 * time.Second
		}
	}
	var ocrDeduper OCRLineDeduper
	var lastCaptureAt time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		captureStarted := time.Now().UTC()
		captureIntervalMS := int64(0)
		if !lastCaptureAt.IsZero() {
			captureIntervalMS = captureStarted.Sub(lastCaptureAt).Milliseconds()
		}
		lastCaptureAt = captureStarted
		frame, err := os.CreateTemp("", "liveguard-frame-*.png")
		if err != nil {
			s.setCaptureError(runID, err)
		} else {
			framePath := frame.Name()
			_ = frame.Close()
			_ = os.Remove(framePath)
			captureCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			captureStartedAt := time.Now()
			err = s.capturer.Capture(captureCtx, runID, targetURL, framePath)
			captureDuration := time.Since(captureStartedAt)
			if err == nil {
				var event SignalEvent
				perceptionStarted := time.Now()
				event, err = s.recognizer.Recognize(captureCtx, VideoFrame{RoomID: roomID, Path: framePath})
				perceptionDuration := time.Since(perceptionStarted)
				if err == nil {
					event.FramePath = framePath
					event.ObservedAt = captureStarted
					event.CaptureDurationMS = captureDuration.Milliseconds()
					event.PerceptionDurationMS = perceptionDuration.Milliseconds()
					novelText := ocrDeduper.Filter(event.Text, captureStarted)
					if novelText != "" {
						event.Text = novelText
						if ingestErr := s.Ingest(captureCtx, runID, event); ingestErr != nil && !errors.Is(ingestErr, context.Canceled) {
							err = ingestErr
						}
					}
				}
			}
			cancel()
			_ = os.Remove(framePath)
			s.recordCaptureMetrics(runID, captureStarted, captureIntervalMS, captureDuration.Milliseconds())
			if err != nil && !errors.Is(err, context.Canceled) {
				s.setCaptureError(runID, err)
			} else if err == nil {
				s.clearCaptureError(runID)
			}
		}
		if !wait(ctx, captureInterval) {
			return
		}
	}
}

func (s *Service) recordCaptureMetrics(runID string, capturedAt time.Time, intervalMS, durationMS int64) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		for i := range run.Rooms {
			if run.Rooms[i].LastCaptureAt.Before(capturedAt) {
				run.Rooms[i].LastCaptureAt = capturedAt
				run.Rooms[i].CaptureIntervalMS = intervalMS
				run.Rooms[i].CaptureDurationMS = durationMS
			}
		}
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
}

func (s *Service) setCaptureError(runID string, captureErr error) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		if len(run.Rooms) > 0 {
			message := captureErr.Error()
			run.Rooms[0].LastError = message
			if strings.Contains(strings.ToLower(message), "ocr returned no text") {
				run.Rooms[0].Health = SourceWaiting
				run.Rooms[0].LastError = "尚未识别到可用直播文字"
			} else if containsAny(strings.ToLower(message), []string{"login", "captcha", "验证", "登录"}) {
				run.Rooms[0].Health = SourceNeedsLogin
			} else {
				run.Rooms[0].Health = SourceFailed
			}
		}
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
}

func (s *Service) clearCaptureError(runID string) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		if len(run.Rooms) > 0 {
			run.Rooms[0].LastError = ""
			if run.Rooms[0].LastSeen.IsZero() {
				run.Rooms[0].Health = SourceWaiting
			} else {
				run.Rooms[0].Health = SourceHealthy
			}
		}
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
}

func (s *Service) RecoverInterrupted(ctx context.Context) error {
	runs, err := s.repository.List(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status != RunRunning {
			continue
		}
		completedAt := time.Now().UTC()
		if _, err := s.repository.Update(ctx, run.ID, func(current *Run) error {
			current.Status = RunStopped
			current.CompletedAt = &completedAt
			for i := range current.Rooms {
				current.Rooms[i].Status = "stopped"
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) replay(ctx context.Context, runID string, events []SignalEvent) {
	defer s.finishRuntime(runID, ctx)
	for _, event := range events {
		if !wait(ctx, s.replayGap) {
			return
		}
		if err := s.Ingest(ctx, runID, event); err != nil {
			if !errors.Is(err, context.Canceled) {
				s.fail(runID)
			}
			return
		}
	}
	// A personal monitor stays active after the sample burst. A real SignalSource
	// will continue feeding events through the same Ingest boundary.
	<-ctx.Done()
}

func (s *Service) fail(runID string) {
	completedAt := time.Now().UTC()
	_, _ = s.repository.Update(context.Background(), runID, func(run *Run) error {
		run.Status = RunFailed
		run.CompletedAt = &completedAt
		return nil
	})
	s.onUpdate()
}

func wait(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func durationSecondsCeil(duration time.Duration) int {
	seconds := int(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	return seconds
}

func (s *Service) Ingest(ctx context.Context, runID string, event SignalEvent) error {
	ctx, release := s.monitorContext(ctx, runID)
	defer release()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.ingest(ctx, runID, event)
}

func (s *Service) monitorContext(ctx context.Context, runID string) (context.Context, func()) {
	s.mu.Lock()
	runCtx := s.contexts[runID]
	s.mu.Unlock()
	if runCtx != nil {
		linkedCtx, cancel := context.WithCancel(ctx)
		stopLink := context.AfterFunc(runCtx, cancel)
		if runCtx.Err() != nil {
			cancel()
		}
		return linkedCtx, func() { stopLink(); cancel() }
	}
	return ctx, func() {}
}

func (s *Service) ingest(ctx context.Context, runID string, event SignalEvent) error {
	snapshot, acceptedEvent, duplicate, err := s.acceptSignal(ctx, runID, event)
	if err != nil || duplicate {
		return err
	}
	event = acceptedEvent
	eventType := classify(event.Text, snapshot.Goal)
	var review *UnexpectedEventDecision
	// A configured model handles arbitrary goal conditions as well as proactive
	// events. Keyword matches cannot bypass semantic checks. During the cooldown
	// signals remain in the evidence window; no rule-based alert is substituted.
	if eventType != "human_verification" && s.reviewer != nil {
		release, allowed, overloaded := s.beginReview(runID, time.Now().UTC(), snapshot.Plan.ReviewIntervalSeconds)
		if overloaded {
			s.recordReviewBackpressure(runID)
		}
		if allowed {
			defer release()
			recent := evidenceWindow(snapshot, event.RoomID, 6)
			if len(recent) > 6 {
				recent = recent[:6]
			}
			input := UnexpectedEventInput{Goal: snapshot.Goal, Plan: snapshot.Plan, RecentEvidence: recent, RecentAlerts: snapshot.Alerts, TriggerFrame: event.FramePath}
			if input.TriggerFrame == "" && snapshot.SourceMode == "browser_ocr" && s.capturer != nil {
				input.CaptureFrame = func(captureCtx context.Context) (FrameAsset, error) {
					captureCtx, cancel := context.WithTimeout(captureCtx, 12*time.Second)
					defer cancel()
					capturedAt := time.Now().UTC()
					file, err := os.CreateTemp("", "liveguard-agent-frame-*.png")
					if err != nil {
						return FrameAsset{}, err
					}
					framePath := file.Name()
					_ = file.Close()
					_ = os.Remove(framePath)
					if err = s.capturer.Capture(captureCtx, runID, snapshot.TargetURL, framePath); err != nil {
						_ = os.Remove(framePath)
						return FrameAsset{}, err
					}
					return FrameAsset{Path: framePath, ObservedAt: capturedAt}, nil
				}
			}
			decision, reviewErr := s.reviewer.Review(ctx, input)
			if reviewErr == nil {
				if reviewDecisionIncomplete(decision) {
					decision.Failure = "本次 AI 调查未形成完整结论，监控会继续运行"
					log.Printf("unexpected livestream review incomplete: model=%s stop_reason=%s turns=%d confidence=%.2f input_tokens=%d output_tokens=%d latency_ms=%d", decision.Model, decision.StopReason, decision.Turns, decision.Confidence, decision.InputTokens, decision.OutputTokens, decision.LatencyMS)
				} else {
					log.Printf("unexpected livestream review completed: model=%s action=%s confidence=%.2f input_tokens=%d output_tokens=%d latency_ms=%d", decision.Model, decision.Action, decision.Confidence, decision.InputTokens, decision.OutputTokens, decision.LatencyMS)
				}
				review = &decision
			} else {
				decision.Failure = "AI 暂时无法完成判断，监控会继续运行"
				if decision.StopReason == "timeout" {
					decision.Failure = "本次 AI 调查超时，监控会继续运行"
				}
				review = &decision
				log.Printf("unexpected livestream review failed: %v", reviewErr)
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err = s.repository.Update(ctx, runID, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		if time.Since(event.ObservedAt) > maxSignalAge {
			return ErrStaleSignal
		}
		for i := range run.Rooms {
			if run.Rooms[i].ID == event.RoomID {
				if event.ObservedAt.After(run.Rooms[i].LastSeen) {
					run.Rooms[i].LastSeen = event.ObservedAt
				}
				run.Rooms[i].Health = SourceHealthy
				run.Rooms[i].LastError = ""
			}
		}
		if eventType == "human_verification" || (s.reviewer == nil && eventType != "") {
			investigate(run, event, eventType)
		} else if review != nil {
			applyUnexpectedReview(run, event, *review)
		}
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
	return err
}

func reviewDecisionIncomplete(decision UnexpectedEventDecision) bool {
	return decision.StopReason != "" && decision.StopReason != "finished" && decision.StopReason != "terminal_tool"
}

func (s *Service) acceptSignal(ctx context.Context, runID string, event SignalEvent) (*Run, SignalEvent, bool, error) {
	if !utf8.ValidString(event.Text) || strings.TrimSpace(event.Text) == "" || utf8.RuneCountInString(event.Text) > maxSignalTextRunes {
		return nil, event, false, errors.New("signal text must contain 1-4000 characters")
	}
	if event.Source != SignalOCR && event.Source != SignalASR {
		return nil, event, false, errors.New("signal source must be ocr or asr")
	}
	if math.IsNaN(event.Confidence) || math.IsInf(event.Confidence, 0) || event.Confidence < 0 || event.Confidence > 1 {
		return nil, event, false, errors.New("signal confidence must be between 0 and 1")
	}
	now := time.Now().UTC()
	if event.ObservedAt.IsZero() {
		event.ObservedAt = now
	}
	event.ObservedAt = event.ObservedAt.UTC()
	if event.ObservedAt.Before(now.Add(-maxSignalAge)) || event.ObservedAt.After(now.Add(maxFutureSignalSkew)) {
		return nil, event, false, ErrStaleSignal
	}
	event.ID = strings.TrimSpace(event.ID)
	if event.ID == "" {
		event.ID = domain.NewID("signal")
	}
	if len(event.ID) > 128 {
		return nil, event, false, errors.New("signal id exceeds 128 characters")
	}
	snapshot, err := s.repository.Get(ctx, runID)
	if err != nil {
		return nil, event, false, err
	}
	if snapshot.Status != RunRunning {
		return nil, event, false, errors.New("monitor is not running")
	}
	if event.RoomID == "" && len(snapshot.Rooms) == 1 {
		event.RoomID = snapshot.Rooms[0].ID
	}
	var room *Room
	for i := range snapshot.Rooms {
		if snapshot.Rooms[i].ID == event.RoomID {
			room = &snapshot.Rooms[i]
			break
		}
	}
	if room == nil {
		return nil, event, false, errors.New("signal room does not belong to this monitor")
	}
	event.RoomName = room.Name
	event.ReceivedAt = now
	fingerprintBytes, _ := json.Marshal(struct {
		RoomID     string
		Source     SignalSource
		Text       string
		Confidence float64
	}{event.RoomID, event.Source, event.Text, event.Confidence})
	fingerprint := sha256.Sum256(fingerprintBytes)
	duplicate := false
	updated, err := s.repository.Update(ctx, runID, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		if run.EventReceipts == nil {
			run.EventReceipts = []EventReceipt{}
		}
		cutoff := now.Add(-eventReceiptRetention)
		kept := run.EventReceipts[:0]
		for _, receipt := range run.EventReceipts {
			if receipt.ReceivedAt.After(cutoff) {
				kept = append(kept, receipt)
			}
		}
		run.EventReceipts = kept
		for _, receipt := range run.EventReceipts {
			if receipt.ID == event.ID {
				if receipt.Fingerprint != hex.EncodeToString(fingerprint[:]) {
					return ErrEventIDConflict
				}
				duplicate = true
				return nil
			}
		}
		run.EventSequence++
		event.Sequence = run.EventSequence
		run.EventReceipts = append(run.EventReceipts, EventReceipt{ID: event.ID, Fingerprint: hex.EncodeToString(fingerprint[:]), ReceivedAt: now})
		if len(run.EventReceipts) > maxEventReceipts {
			run.EventReceipts = append([]EventReceipt(nil), run.EventReceipts[len(run.EventReceipts)-maxEventReceipts:]...)
		}
		run.Events = append(run.Events, event)
		if len(run.Rooms) > 0 {
			for i := range run.Rooms {
				if run.Rooms[i].ID == event.RoomID {
					run.Rooms[i].LastSeen = event.ObservedAt
					run.Rooms[i].Health = SourceHealthy
					run.Rooms[i].LastError = ""
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, event, false, err
	}
	if duplicate {
		return updated, event, true, nil
	}
	for _, stored := range updated.Events {
		if stored.ID == event.ID {
			event = stored
			break
		}
	}
	s.onUpdate()
	return updated, event, false, nil
}

func (s *Service) beginReview(runID string, at time.Time, requestedSeconds int) (func(), bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reviewInFlight[runID] {
		return nil, false, false
	}
	interval := s.reviewGap
	if requestedSeconds > 0 {
		requested := time.Duration(requestedSeconds) * time.Second
		if requested > interval {
			interval = requested
		}
	}
	if previous := s.lastUnexpectedReview[runID]; !previous.IsZero() && at.Sub(previous) < interval {
		return nil, false, false
	}
	select {
	case s.reviewSlots <- struct{}{}:
	default:
		return nil, false, true
	}
	s.lastUnexpectedReview[runID] = at
	s.reviewInFlight[runID] = true
	var once sync.Once
	return func() {
		once.Do(func() { <-s.reviewSlots; s.mu.Lock(); delete(s.reviewInFlight, runID); s.mu.Unlock() })
	}, true, false
}

func (s *Service) recordReviewBackpressure(runID string) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		run.ReviewBackpressureCount++
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
}

// IngestFrame runs OCR and feeds its normalized result into the Agent Runtime.
// The runtime does not know whether the signal came from Tesseract, a cloud
// provider, a browser screenshot, or a deterministic test recognizer.
func (s *Service) IngestFrame(ctx context.Context, runID string, frame VideoFrame) error {
	ctx, release := s.monitorContext(ctx, runID)
	defer release()
	if s.recognizer == nil {
		return errors.New("ocr recognizer is not configured")
	}
	started := time.Now()
	event, err := s.recognizer.Recognize(ctx, frame)
	if err != nil {
		return err
	}
	if event.ID == "" {
		event.ID = frame.EventID
	}
	event.FramePath = frame.Path
	event.PerceptionDurationMS = time.Since(started).Milliseconds()
	return s.Ingest(ctx, runID, event)
}

// IngestAudio transcribes one short, user-authorized audio chunk and sends the
// text through the same Agent decision path as OCR and other signals.
func (s *Service) IngestAudio(ctx context.Context, runID string, chunk AudioChunk) error {
	ctx, release := s.monitorContext(ctx, runID)
	defer release()
	if s.transcriber == nil {
		return errors.New("ASR is not configured")
	}
	started := time.Now()
	event, err := s.transcriber.Transcribe(ctx, chunk)
	if errors.Is(err, ErrNoSpeech) {
		log.Printf("ASR chunk contained no recognized speech: monitor=%s latency_ms=%d", runID, time.Since(started).Milliseconds())
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(event.Text) == "" {
		log.Printf("ASR chunk contained no recognized speech: monitor=%s latency_ms=%d", runID, time.Since(started).Milliseconds())
		return nil
	}
	if event.ID == "" {
		event.ID = chunk.EventID
	}
	event.PerceptionDurationMS = time.Since(started).Milliseconds()
	log.Printf("ASR transcription completed: monitor=%s characters=%d latency_ms=%d", runID, utf8.RuneCountInString(event.Text), event.PerceptionDurationMS)
	return s.Ingest(ctx, runID, event)
}

func investigate(run *Run, event SignalEvent, eventType string) {
	defaultRuntime.Handle(run, event, eventType)
}

type goalProfile struct {
	kind         string
	keywords     []string
	strongTerms  []string
	negative     string
	vague        string
	confirmation string
	duplicate    string
	conclusion   string
	priority     string
}

func profileForGoal(goal string) goalProfile {
	switch {
	case containsAny(goal, []string{"优惠券", "领券", "红包", "降价", "价格", "满减", "折扣"}):
		return goalProfile{kind: "coupon_drop", keywords: []string{"优惠券", "领券", "红包", "福利", "折扣"}, strongTerms: []string{"限量优惠券", "点击领取", "现在上", "马上发"}, negative: "今天没有优惠券，大家不用等", vague: "稍后给大家安排一波福利", confirmation: "限量优惠券 50 元，马上领取", duplicate: "优惠券已经发出来了", conclusion: "你关注的优惠活动已经出现，可以进入直播间查看", priority: "urgent"}
	case containsAny(goal, []string{"抽奖", "福袋", "开奖", "中奖"}):
		return goalProfile{kind: "giveaway", keywords: []string{"抽奖", "福袋", "开奖", "中奖", "互动福利"}, strongTerms: []string{"抽奖倒计时", "福袋开启", "现在开奖", "马上抽奖"}, negative: "本场暂时不安排抽奖", vague: "等会儿有一个互动福利", confirmation: "抽奖倒计时 00:30", duplicate: "抽奖马上开始", conclusion: "你关注的抽奖活动即将开始", priority: "urgent"}
	case containsAny(goal, []string{"官宣", "新品", "发布", "预售", "重大消息", "重要消息", "嘉宾"}):
		return goalProfile{kind: "major_announcement", keywords: []string{"官宣", "新品", "发布", "预售", "重要消息", "嘉宾"}, strongTerms: []string{"正式官宣", "开启预售", "正式发布", "发布时间"}, negative: "今天不会公布新品消息", vague: "稍后会公布一个重要消息", confirmation: "新品发布时间：今晚 20:00", duplicate: "刚刚已经正式官宣", conclusion: "你关注的重要消息已经公布", priority: "high"}
	case containsAny(goal, []string{"团战", "决胜", "比分", "比赛", "击杀", "Boss", "boss", "上分"}):
		return goalProfile{kind: "game_moment", keywords: []string{"团战", "打团", "决胜", "比分", "比赛", "击杀", "Boss", "boss", "关键时刻"}, strongTerms: []string{"决胜团战", "决胜局", "最终比分", "Boss 开始"}, negative: "这局暂时不会打团", vague: "马上进入比赛关键阶段", confirmation: "决胜局 2:2，团战开始", duplicate: "决胜团战已经开始", conclusion: "比赛进入了你关注的关键时刻", priority: "urgent"}
	default:
		keyword := focusKeyword(goal)
		return goalProfile{kind: "custom_goal", keywords: []string{keyword}, strongTerms: []string{"现在开始", "正式公布", "已经出现"}, negative: "暂时不会提到" + keyword, vague: "稍后会聊到" + keyword, confirmation: keyword + "现在开始", duplicate: "已经提到过" + keyword, conclusion: "直播间出现了你关注的内容：" + keyword, priority: "high"}
	}
}

func classify(text, goal string) string {
	if containsAny(text, []string{"验证码", "安全验证", "请登录", "完成验证"}) {
		return "human_verification"
	}
	profile := profileForGoal(goal)
	if containsAny(text, profile.keywords) {
		return profile.kind
	}
	return ""
}

func applyUnexpectedReview(run *Run, event SignalEvent, decision UnexpectedEventDecision) {
	now := time.Now().UTC()
	eventType := "proactive_" + decision.Category
	inv := Investigation{
		ID: domain.NewID("investigation"), RoomID: event.RoomID, RoomName: event.RoomName,
		EventType: eventType, Status: InvestigationSuppressed, CreatedAt: now, UpdatedAt: now,
		Evidence: []Evidence{{EventID: event.ID, Source: string(event.Source), Text: event.Text, Confidence: event.Confidence, ObservedAt: event.ObservedAt}},
	}
	inv.Review = &ReviewMetrics{Model: decision.Model, InputTokens: decision.InputTokens, OutputTokens: decision.OutputTokens, LatencyMS: decision.LatencyMS, Turns: decision.Turns, StopReason: decision.StopReason}
	inv.Review.FormatRepairs = decision.Repairs
	if len(decision.UsedEvidence) > 0 {
		inv.Evidence = append([]Evidence(nil), decision.UsedEvidence...)
	}
	reviewOutcome := decision.Reason
	if decision.Failure != "" {
		reviewOutcome = decision.Failure
	}
	addStep(&inv, "review_unexpected_event", "判断关注条件是否满足，以及是否出现其他重要事件", reviewOutcome, now)
	for _, call := range decision.ToolCalls {
		addStep(&inv, call.Tool, toolPurpose(call.Tool), call.Outcome, now)
		inv.Steps[len(inv.Steps)-1].Succeeded = &call.Succeeded
		inv.Steps[len(inv.Steps)-1].DurationMS = call.DurationMS
	}
	if visualFinding := strings.TrimSpace(decision.VisualFinding); visualFinding != "" {
		visualAt := decision.VisualObservedAt
		if visualAt.IsZero() {
			visualAt = now
		}
		inv.Evidence = append(inv.Evidence, Evidence{EventID: event.ID, Source: "m3_vision", Text: visualFinding, Confidence: decision.Confidence, ObservedAt: visualAt})
	}
	if decision.Failure != "" {
		inv.Status = InvestigationFailed
		inv.Conclusion = decision.Failure
		run.Investigations = append(run.Investigations, inv)
		return
	}
	if decision.Action == "alert" && decision.Category == "chat_match" {
		if !hasOCRSource(evidenceWindow(run, event.RoomID, 6)) || strings.TrimSpace(decision.VisualFinding) == "" {
			inv.Conclusion = "聊天区提醒缺少 OCR 或看图证据，已跳过"
			run.Investigations = append(run.Investigations, inv)
			return
		}
	}
	if decision.Action == "wait" {
		inv.Status = InvestigationWaiting
		inv.Conclusion = "出现可能值得关注的内容，证据还不够明确，继续观察"
		if decision.StopReason != "finished" && decision.StopReason != "terminal_tool" && decision.StopReason != "" {
			inv.Conclusion = "本次调查达到" + reviewStopLabel(decision.StopReason) + "，继续监听新证据"
		}
		run.Investigations = append(run.Investigations, inv)
		return
	}
	if decision.Action != "alert" || decision.Confidence < 0.72 {
		inv.Conclusion = "判断为普通内容，未打扰用户"
		run.Investigations = append(run.Investigations, inv)
		return
	}
	if !hasReviewTool(decision.ToolCalls, "search_recent_alerts") {
		addStep(&inv, "query_recent_alerts", "检查是否已经提醒过同类突发内容", "已检查当前监控窗口的提醒记录", now)
	}
	if hasRecentSemanticAlert(run.Alerts, event, eventType, decision.Summary) {
		inv.Conclusion = "同一类内容已经提醒，本次提醒已去重"
		run.Investigations = append(run.Investigations, inv)
		return
	}
	inv.Status = InvestigationConfirmed
	inv.Conclusion = decision.Summary
	if !hasReviewTool(decision.ToolCalls, "create_alert") {
		addStep(&inv, "create_alert", "将目标之外的重要时刻提醒给用户", "已生成带识别原文的主动提醒", now)
	}
	run.Investigations = append(run.Investigations, inv)
	run.Alerts = append(run.Alerts, Alert{
		ID: domain.NewID("alert"), RoomID: event.RoomID, RoomName: event.RoomName,
		EventType: eventType, Priority: "high", Summary: decision.Summary,
		Confidence: decision.Confidence, DedupeKey: semanticDedupeKey(event.RoomID, eventType, decision.Summary),
		Investigation: inv.ID, Evidence: append([]Evidence(nil), inv.Evidence...), CreatedAt: now,
	})
}

func reviewStopLabel(reason string) string {
	switch reason {
	case "repeated_tool":
		return "重复工具限制"
	case "turn_budget":
		return "判断轮数上限"
	case "call_budget":
		return "工具调用次数上限"
	case "timeout":
		return "执行时限"
	default:
		return "安全执行限制"
	}
}

func hasReviewTool(calls []ReviewToolCall, name string) bool {
	for _, call := range calls {
		if call.Tool == name && call.Succeeded {
			return true
		}
	}
	return false
}

func toolPurpose(name string) string {
	switch name {
	case "read_recent_evidence":
		return "读取最近的直播文字证据"
	case "search_recent_alerts":
		return "检查是否已经提醒过同类事件"
	case "create_alert":
		return "通过策略校验后请求生成提醒"
	case "analyze_signal_window":
		return "在 Go 中统计文字重复、否定和验证提示"
	default:
		return "Agent 请求执行受限工具"
	}
}

func focusKeyword(goal string) string {
	result := goal
	for _, noise := range []string{"请帮我", "帮我", "我想", "监控", "关注", "提醒", "直播间", "主播", "什么时候", "如果", "出现", "关于", "内容", "消息", "的"} {
		result = strings.ReplaceAll(result, noise, "")
	}
	result = strings.TrimSpace(strings.Trim(result, "，,。！？!?：: "))
	if result == "" {
		return "关注目标"
	}
	runes := []rune(result)
	if len(runes) > 16 {
		return string(runes[:16])
	}
	return result
}

func roomNameFromURL(parsed *url.URL) string {
	room := strings.Trim(path.Base(strings.TrimSuffix(parsed.Path, "/")), " ")
	if room == "" || room == "." || room == "/" {
		room = parsed.Hostname()
	}
	if len([]rune(room)) > 18 {
		room = string([]rune(room)[:18])
	}
	return "直播间 " + room
}

func demoSignals(run *Run) []SignalEvent {
	profile := profileForGoal(run.Goal)
	room := run.Rooms[0]
	base := time.Now().UTC()
	items := []SignalEvent{
		{RoomID: room.ID, RoomName: room.Name, Source: "asr", Text: "欢迎来到直播间，我们先聊聊今天的日常内容", Confidence: .96},
		{RoomID: room.ID, RoomName: room.Name, Source: "asr", Text: profile.negative, Confidence: .94},
		{RoomID: room.ID, RoomName: room.Name, Source: "asr", Text: profile.vague, Confidence: .76},
		{RoomID: room.ID, RoomName: room.Name, Source: "ocr", Text: profile.confirmation, Confidence: .84},
		{RoomID: room.ID, RoomName: room.Name, Source: "asr", Text: profile.duplicate, Confidence: .96},
		{RoomID: room.ID, RoomName: room.Name, Source: "ocr", Text: "直播进行中", Confidence: .99},
	}
	for i := range items {
		items[i].ID = domain.NewID("signal")
		items[i].ObservedAt = base.Add(time.Duration(i) * time.Second)
	}
	return items
}

func addStep(inv *Investigation, tool, purpose, outcome string, at time.Time) {
	inv.Steps = append(inv.Steps, AgentStep{ID: domain.NewID("step"), Sequence: len(inv.Steps) + 1, Tool: tool, Purpose: purpose, Outcome: outcome, CreatedAt: at})
}

func waitingInvestigation(run *Run, roomID, eventType string) int {
	for i := len(run.Investigations) - 1; i >= 0; i-- {
		inv := &run.Investigations[i]
		if inv.RoomID == roomID && inv.EventType == eventType && inv.Status == InvestigationWaiting {
			return i
		}
	}
	return -1
}

func evidenceWindow(run *Run, roomID string, limit int) []SignalEvent {
	result := make([]SignalEvent, 0, limit)
	for i := len(run.Events) - 1; i >= 0 && len(result) < limit; i-- {
		if run.Events[i].RoomID == roomID {
			result = append(result, run.Events[i])
		}
	}
	return result
}

func containsAny(text string, values []string) bool {
	for _, value := range values {
		if value != "" && strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func hasCrossSource(evidence []Evidence) bool {
	sources := map[string]bool{}
	for _, item := range evidence {
		sources[item.Source] = true
	}
	return len(sources) >= 2
}

func hasRecentAlert(run *Run, roomID, eventType string) bool {
	for _, alert := range run.Alerts {
		if alert.RoomID == roomID && alert.EventType == eventType {
			return true
		}
	}
	return false
}

func combinedConfidence(evidence []Evidence) float64 {
	product := 1.0
	for _, item := range evidence {
		confidence := item.Confidence
		if confidence <= 0 || confidence > 1 {
			confidence = 0.7
		}
		product *= 1 - confidence
	}
	return 1 - product
}

func setRoomStatus(run *Run, roomID, status string) {
	for i := range run.Rooms {
		if run.Rooms[i].ID == roomID {
			run.Rooms[i].Status = status
			return
		}
	}
}
