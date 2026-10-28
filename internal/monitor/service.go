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
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"liveguard/internal/domain"
)

const (
	InvestigationWaiting     = "waiting_more_evidence"
	InvestigationWaitingUser = "waiting_user"
	InvestigationReviewing   = "reviewing_clarification"
	InvestigationConfirmed   = "confirmed"
	InvestigationSuppressed  = "suppressed"
	InvestigationNeedsHuman  = "needs_human"
	InvestigationFailed      = "failed"
	InvestigationIncomplete  = "incomplete"
	maxActiveMonitors        = 8
	maxConcurrentReviews     = 4
	maxEventReceipts         = 2048
	eventReceiptRetention    = 24 * time.Hour
	maxSignalTextRunes       = 4000
	maxFutureSignalSkew      = 2 * time.Minute
)

var (
	ErrEventIDConflict = errors.New("event id was already used with different content")
	ErrFutureSignal    = errors.New("signal observation time is too far in the future")
	ErrGoalChanged     = errors.New("monitor goal changed while planning")
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

// PageTitleReader is an optional capability implemented by browser capturers
// that can read the loaded page's title through their browser session.
type PageTitleReader interface {
	PageTitle(context.Context, string, string) (string, error)
}

type Service struct {
	repository       Repository
	frameArchiveDir  string
	onUpdate         func()
	replayGap        time.Duration
	recognizer       TextRecognizer
	transcriber      Transcriber
	capturer         FrameCapturer
	captureGap       time.Duration
	reviewer         UnexpectedEventReviewer
	reviewGap        time.Duration
	goalPlanner      GoalPlanner
	baselineAnalyzer BaselineAnalyzer
	modelMu          sync.RWMutex
	modelConfig      ModelSettingsStatus
	modelAPIKey      string

	mu                   sync.Mutex
	cancels              map[string]context.CancelFunc
	lastUnexpectedReview map[string]time.Time
	contexts             map[string]context.Context
	reviewInFlight       map[string]bool
	reviewSlots          chan struct{}
	ocrSlots             chan struct{}
	reviewWaiters        []*reviewWaiter
	reservations         int
	pending              map[string]*signalQueue
	retainedFrameBytes   int64
}

func NewService(repository Repository, onUpdate func()) *Service {
	if onUpdate == nil {
		onUpdate = func() {}
	}
	return &Service{repository: repository, frameArchiveDir: filepath.Join("runtime", "frames"), onUpdate: onUpdate, replayGap: 700 * time.Millisecond, reviewGap: 15 * time.Second, goalPlanner: ruleGoalPlanner{}, cancels: map[string]context.CancelFunc{}, contexts: map[string]context.Context{}, lastUnexpectedReview: map[string]time.Time{}, reviewInFlight: map[string]bool{}, reviewSlots: make(chan struct{}, maxConcurrentReviews), ocrSlots: make(chan struct{}, 4), pending: map[string]*signalQueue{}}
}

func (s *Service) SetFrameArchiveDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		s.frameArchiveDir = dir
	}
}

// ArchivedFramePath exposes only images referenced by a persisted alert or
// investigation for this run; callers never provide a filesystem path.
func (s *Service) ArchivedFramePath(ctx context.Context, runID, archiveID string) (string, error) {
	run, err := s.repository.Get(ctx, runID)
	if err != nil {
		return "", err
	}
	referenced := false
	for _, investigation := range run.Investigations {
		for _, evidence := range investigation.Evidence {
			if evidence.FrameArchiveID == archiveID {
				referenced = true
				break
			}
		}
		if referenced {
			break
		}
	}
	if !referenced {
		for _, alert := range run.Alerts {
			for _, evidence := range alert.Evidence {
				if evidence.FrameArchiveID == archiveID {
					referenced = true
					break
				}
			}
			if referenced {
				break
			}
		}
	}
	if !referenced {
		return "", ErrNotFound
	}
	return s.archivedFramePath(runID, archiveID)
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
		s.modelMu.Lock()
		defer s.modelMu.Unlock()
		s.goalPlanner = planner
	}
}

func (s *Service) SetBaselineAnalyzer(analyzer BaselineAnalyzer) {
	s.modelMu.Lock()
	s.baselineAnalyzer = analyzer
	s.modelMu.Unlock()
}

func (s *Service) initialBaselineAnalyzer() BaselineAnalyzer {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	return s.baselineAnalyzer
}

func (s *Service) SetFrameCapturer(capturer FrameCapturer) { s.capturer = capturer }

// SetUnexpectedEventReviewer lets the model spot high-salience moments outside
// the user's explicit goal. Calls are throttled per monitor to control cost.
func (s *Service) SetUnexpectedEventReviewer(reviewer UnexpectedEventReviewer, minInterval time.Duration) {
	traceStore, _ := s.repository.(InvestigationTraceRepository)
	switch configured := reviewer.(type) {
	case *AgentReviewer:
		if configured.TraceStore == nil {
			configured.TraceStore = traceStore
		}
	case *MiniMaxUnexpectedEventReviewer:
		if configured.TraceStore == nil {
			configured.TraceStore = traceStore
		}
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	s.reviewer = reviewer
	if minInterval > 0 {
		s.reviewGap = minInterval
	}
}

func (s *Service) SetUnexpectedEventReviewerInterval(minInterval time.Duration) {
	if minInterval <= 0 {
		return
	}
	s.modelMu.Lock()
	s.reviewGap = minInterval
	s.modelMu.Unlock()
}

func (s *Service) agentConfig() (GoalPlanner, UnexpectedEventReviewer, time.Duration) {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	return s.goalPlanner, s.reviewer, s.reviewGap
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
	planner, reviewer, reviewGap := s.agentConfig()
	if state, ok := reviewer.(interface{ QuotaExhausted() bool }); ok && state.QuotaExhausted() {
		return nil, ErrModelQuotaExhausted
	}
	plan, planErr := planner.Plan(ctx, input.Goal)
	if errors.Is(planErr, ErrModelQuotaExhausted) {
		s.endQuotaExhaustedRuns("", reviewer)
		return nil, planErr
	}
	if planErr != nil {
		log.Printf("monitor goal planning failed; using bounded local plan: %v", planErr)
		plan = defaultGoalPlan(input.Goal, "本地规则兜底")
	}
	plan = normalizeMonitoringPlan(plan, input.Goal)
	if catalog, ok := reviewer.(interface{ AdditionalToolNames() []string }); ok {
		plan.Tools = append(plan.Tools, catalog.AdditionalToolNames()...)
	}
	if s.captureGap >= time.Second {
		plan.CaptureIntervalSeconds = durationSecondsCeil(s.captureGap)
	}
	if reviewGap > time.Duration(plan.ReviewIntervalSeconds)*time.Second {
		plan.ReviewIntervalSeconds = durationSecondsCeil(reviewGap)
	}
	plan = planForReviewer(plan, input.Goal, reviewer)

	now := time.Now().UTC()
	roomID := domain.NewID("room")
	roomName := roomNameFromURL(parsed)
	platform := platformFromURL(parsed)
	run := &Run{
		ID: domain.NewID("monitor"), Name: roomName, TargetURL: input.TargetURL, Goal: input.Goal,
		Plan: plan, SourceMode: input.SourceMode, Status: RunRunning, StartedAt: now, UpdatedAt: now, Version: 1,
		GoalVersion: 1, GoalUpdatedAt: now, Sampling: initialSampling(plan, now),
		Rooms: []Room{{ID: roomID, Name: roomName, Platform: platform, Status: "listening", Health: SourceWaiting}},
	}
	if input.SourceMode == "browser_ocr" {
		run.Baseline = &LiveBaseline{Status: BaselinePending}
		if s.initialBaselineAnalyzer() == nil {
			run.Baseline = &LiveBaseline{Status: BaselineUnavailable, Error: "当前未配置支持看图的模型，继续使用 OCR 监控"}
		}
	}
	if err := s.repository.Create(ctx, run); err != nil {
		return nil, err
	}
	// A monitor has no wall-clock deadline. It runs until explicitly stopped,
	// the service exits, or its model reports exhausted API quota.
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.reservations--
	reserved = false
	s.cancels[run.ID] = cancel
	s.contexts[run.ID] = runCtx
	s.pending[run.ID] = s.newPendingQueue(runCtx, run.ID, run.GoalVersion)
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
	_, reviewer, _ := s.agentConfig()
	if run.Status == RunRunning {
		run.Plan = s.planForRuntime(run, reviewer)
	}
	s.mu.Lock()
	q := s.pending[id]
	s.mu.Unlock()
	if q != nil {
		status := q.stats()
		run.Pending = &status
	}
	if run.Status == RunRunning {
		run.Sampling = s.effectiveSampling(run, time.Now().UTC())
		if run.SourceMode == "browser_ocr" {
			run.Plan.CaptureIntervalSeconds = run.Sampling.CaptureIntervalSeconds
			run.Plan.Summary = summarizeMonitoringPlan(run.Plan)
		}
	}
	if run.Status == RunRunning && run.SourceMode == "browser_ocr" && len(run.Rooms) > 0 {
		interval := time.Duration(run.Sampling.CaptureIntervalSeconds) * time.Second
		if interval <= 0 {
			interval = time.Second
		}
		lastSignal := run.Rooms[0].LastCaptureAt
		if lastSignal.IsZero() {
			lastSignal = run.StartedAt
		}
		if time.Since(lastSignal) > 2*interval && (run.Rooms[0].Health == SourceWaiting || run.Rooms[0].Health == SourceHealthy) {
			run.Rooms[0].Health = SourceDelayed
		}
	}
	return run, nil
}

func (s *Service) planForRuntime(run *Run, reviewer UnexpectedEventReviewer) MonitoringPlan {
	plan := planForReviewer(run.Plan, run.Goal, reviewer)
	if run.SourceMode == "browser_ocr" {
		plan.CaptureIntervalSeconds = s.effectiveSampling(run, time.Now().UTC()).CaptureIntervalSeconds
		plan.Summary = summarizeMonitoringPlan(plan)
	}
	return plan
}

// UpdateGoal applies a selected monitoring goal and replans its bounded sensing
// policy. User collaboration messages or the dedicated goal API can select it.
func (s *Service) UpdateGoal(ctx context.Context, id, goal string) (*Run, error) {
	goal = strings.TrimSpace(goal)
	if utf8.RuneCountInString(goal) < 2 || utf8.RuneCountInString(goal) > 240 {
		return nil, errors.New("monitoring goal must contain 2-240 characters")
	}
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Status != RunRunning && current.Status != RunPaused {
		return nil, ErrInvalidTransition
	}
	planner, reviewer, reviewGap := s.agentConfig()
	if state, ok := reviewer.(interface{ QuotaExhausted() bool }); ok && state.QuotaExhausted() {
		return nil, ErrModelQuotaExhausted
	}
	plan, planErr := planner.Plan(ctx, goal)
	if errors.Is(planErr, ErrModelQuotaExhausted) {
		s.endQuotaExhaustedRuns(id, reviewer)
		return nil, planErr
	}
	if planErr != nil {
		log.Printf("monitor goal update planning failed; using bounded local plan: %v", planErr)
		plan = defaultGoalPlan(goal, "本地规则兜底")
	}
	plan = normalizeMonitoringPlan(plan, goal)
	if catalog, ok := reviewer.(interface{ AdditionalToolNames() []string }); ok {
		plan.Tools = append(plan.Tools, catalog.AdditionalToolNames()...)
	}
	if s.captureGap >= time.Second {
		plan.CaptureIntervalSeconds = durationSecondsCeil(s.captureGap)
	}
	if reviewGap > time.Duration(plan.ReviewIntervalSeconds)*time.Second {
		plan.ReviewIntervalSeconds = durationSecondsCeil(reviewGap)
	}
	plan = planForReviewer(plan, goal, reviewer)
	baselineAvailable := s.initialBaselineAnalyzer() != nil
	updated, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status != RunRunning && run.Status != RunPaused {
			return ErrInvalidTransition
		}
		if run.GoalVersion != current.GoalVersion {
			return ErrGoalChanged
		}
		for i := range run.Investigations {
			inv := &run.Investigations[i]
			if inv.GoalVersion != run.GoalVersion || (inv.Status != InvestigationWaitingUser && inv.Status != InvestigationReviewing) {
				continue
			}
			if err := TransitionInvestigation(inv, InvestigationSuppressed); err != nil {
				return err
			}
			inv.Conclusion = "监控目标已更新，这次求助已结束；后续信号将按新目标判断"
			inv.UpdatedAt = time.Now().UTC()
		}
		run.Goal = goal
		run.UserInstructions = nil
		run.Plan = plan
		run.GoalVersion++
		run.InvestigationMemory = nil
		run.GoalUpdatedAt = time.Now().UTC()
		run.Sampling = initialSampling(plan, run.GoalUpdatedAt)
		if run.SourceMode == "browser_ocr" {
			run.Baseline = &LiveBaseline{Status: BaselinePending}
			if !baselineAvailable {
				run.Baseline = &LiveBaseline{Status: BaselineUnavailable, Error: "当前未配置支持看图的模型，继续使用 OCR 监控"}
			}
		}
		return nil
	})
	if err == nil {
		s.mu.Lock()
		delete(s.lastUnexpectedReview, id)
		q := s.pending[id]
		s.mu.Unlock()
		if q != nil {
			q.invalidate(updated.GoalVersion, false)
		}
		s.onUpdate()
	}
	return updated, err
}

// SubmitInstruction records a user message for the agent's next review. A
// paused monitor is resumed because submitting a message is an explicit
// request to continue after the user has handled the page.
func (s *Service) SubmitInstruction(ctx context.Context, id, instruction string) (*Run, error) {
	instruction = strings.TrimSpace(instruction)
	if n := utf8.RuneCountInString(instruction); n < 1 || n > 1000 {
		return nil, errors.New("instruction must contain 1-1000 characters")
	}
	var investigationID string
	updated, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status != RunRunning && run.Status != RunPaused {
			return ErrInvalidTransition
		}
		run.UserInstructions = append(run.UserInstructions, instruction)
		if len(run.UserInstructions) > 10 {
			run.UserInstructions = append([]string(nil), run.UserInstructions[len(run.UserInstructions)-10:]...)
		}
		_, reviewer, _ := s.agentConfig()
		if reviewer != nil && len(run.Rooms) > 0 {
			for i := len(run.Events) - 1; i >= 0; i-- {
				event := run.Events[i]
				if (event.Source != SignalOCR && event.Source != SignalASR) || !currentEvidence(event.ObservedAt, event.ValidUntil, event.GoalVersion, run.GoalVersion, run.GoalUpdatedAt, time.Now().UTC()) {
					continue
				}
				investigationID = domain.NewID("investigation")
				run.Investigations = append(run.Investigations, Investigation{
					ID: investigationID, GoalVersion: run.GoalVersion, RoomID: event.RoomID, RoomName: event.RoomName,
					EventType: "user_instruction", Status: InvestigationReviewing, UserAnswer: instruction,
					Conclusion: "收到新指令，Agent 正在结合近期信号处理",
					Evidence:   []Evidence{{EventID: event.ID, RoomID: event.RoomID, GoalVersion: event.GoalVersion, Source: string(event.Source), Text: event.Text, Confidence: event.Confidence, ObservedAt: event.ObservedAt, ValidUntil: event.ValidUntil}},
					CreatedAt:  time.Now().UTC(), UpdatedAt: time.Now().UTC(),
				})
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if updated.Status == RunPaused {
		s.onUpdate()
		return s.Resume(ctx, id)
	}
	s.reopenPendingQueue(id, updated.GoalVersion)
	s.onUpdate()
	if investigationID != "" {
		go s.resumeClarifiedInvestigation(id, investigationID)
	}
	return updated, nil
}

// reopenPendingQueue restores admission after a user has handled a human
// verification challenge. The monitor may still be running while that queue
// is closed, so an explicit user message is the signal to resume review.
func (s *Service) reopenPendingQueue(runID string, version uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runCtx := s.contexts[runID]
	if runCtx == nil || runCtx.Err() != nil {
		return
	}
	if q := s.pending[runID]; q != nil && q.accepting() {
		return
	}
	s.pending[runID] = s.newPendingQueue(runCtx, runID, version)
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
	q := s.pending[id]
	s.mu.Unlock()
	completedAt := time.Now().UTC()
	var pendingStatus *PendingStatus
	if q != nil {
		status := q.stats()
		status.Waiting, status.Reviewing = 0, false
		pendingStatus = &status
	}
	run, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status != RunRunning && run.Status != RunPaused {
			return nil
		}
		run.Status = RunStopped
		if pendingStatus != nil {
			run.Pending = pendingStatus
		}
		for i := range run.Investigations {
			inv := &run.Investigations[i]
			if inv.Status == InvestigationWaitingUser || inv.Status == InvestigationReviewing {
				if err := TransitionInvestigation(inv, InvestigationSuppressed); err != nil {
					return err
				}
				inv.Conclusion = "监控已结束，这次求助已关闭"
				inv.UpdatedAt = completedAt
			}
		}
		run.CompletedAt = &completedAt
		for i := range run.Rooms {
			run.Rooms[i].Status = "stopped"
		}
		return nil
	})
	if err == nil {
		if q != nil {
			q.invalidate(0, true)
		}
		if cancel != nil {
			cancel()
		}
		if s.capturer != nil {
			s.capturer.Close(id)
		}
		s.mu.Lock()
		if s.contexts[id] == runCtx {
			delete(s.cancels, id)
			delete(s.contexts, id)
			delete(s.lastUnexpectedReview, id)
			delete(s.pending, id)
		}
		s.mu.Unlock()
	}
	if err == nil {
		s.onUpdate()
	}
	return run, err
}

// Pause suspends collection and investigation while preserving the monitor's
// identity, goal and history so Resume can continue the same run.
func (s *Service) Pause(ctx context.Context, id string) (*Run, error) {
	s.mu.Lock()
	cancel, runCtx, q := s.cancels[id], s.contexts[id], s.pending[id]
	s.mu.Unlock()
	var pendingStatus *PendingStatus
	if q != nil {
		status := q.stats()
		status.Waiting, status.Reviewing = 0, false
		pendingStatus = &status
	}
	updated, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status == RunPaused {
			return nil
		}
		if run.Status != RunRunning {
			return ErrInvalidTransition
		}
		run.Status = RunPaused
		if pendingStatus != nil {
			run.Pending = pendingStatus
		}
		for i := range run.Rooms {
			run.Rooms[i].Status = "paused"
		}
		for i := range run.Investigations {
			inv := &run.Investigations[i]
			if inv.Status == InvestigationReviewing && inv.UserAnswer != "" {
				if err := TransitionInvestigation(inv, InvestigationWaitingUser); err != nil {
					return err
				}
				inv.Conclusion = "监控已暂停；继续后会恢复结合补充的判断"
				inv.UpdatedAt = time.Now().UTC()
			}
		}
		return nil
	})
	if err == nil {
		if q != nil {
			q.invalidate(0, true)
		}
		if cancel != nil {
			cancel()
		}
		if s.capturer != nil {
			s.capturer.Close(id)
		}
		s.mu.Lock()
		if s.contexts[id] == runCtx {
			delete(s.cancels, id)
			delete(s.contexts, id)
			delete(s.pending, id)
			delete(s.lastUnexpectedReview, id)
		}
		s.mu.Unlock()
		s.onUpdate()
	}
	return updated, err
}

// Resume restarts the source loop against the existing run and ledger.
func (s *Service) Resume(ctx context.Context, id string) (*Run, error) {
	s.mu.Lock()
	if len(s.cancels)+s.reservations >= maxActiveMonitors {
		s.mu.Unlock()
		return nil, ErrCapacity
	}
	s.reservations++
	s.mu.Unlock()
	if store, ok := s.repository.(DurablePendingQueue); ok {
		current, err := s.repository.Get(ctx, id)
		if err == nil && current.Status == RunPaused {
			err = store.RecoverPending(ctx, id, current.GoalVersion, time.Now().UTC())
		}
		if err != nil {
			s.mu.Lock()
			s.reservations--
			s.mu.Unlock()
			return nil, err
		}
	}
	updated, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status != RunPaused {
			return ErrInvalidTransition
		}
		run.Status = RunRunning
		run.LastError = ""
		run.StopReason = ""
		run.CompletedAt = nil
		for i := range run.Investigations {
			inv := &run.Investigations[i]
			if inv.Status == InvestigationWaitingUser && inv.UserAnswer != "" {
				if err := TransitionInvestigation(inv, InvestigationReviewing); err != nil {
					return err
				}
				inv.Conclusion = "继续监控后，Agent 正在结合补充重新判断"
				inv.UpdatedAt = time.Now().UTC()
			}
		}
		for i := range run.Rooms {
			run.Rooms[i].Status = "listening"
			if run.Rooms[i].Health == SourceNeedsLogin || run.Rooms[i].Health == SourceFailed {
				run.Rooms[i].Health = SourceWaiting
				run.Rooms[i].LastError = ""
			}
		}
		if run.Baseline != nil && run.Baseline.Status == BaselineNeedsLogin {
			run.Baseline = &LiveBaseline{Status: BaselinePending}
		}
		return nil
	})
	if err != nil {
		s.mu.Lock()
		s.reservations--
		s.mu.Unlock()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.reservations--
	s.cancels[id], s.contexts[id] = cancel, runCtx
	s.pending[id] = s.newPendingQueue(runCtx, id, updated.GoalVersion)
	s.mu.Unlock()
	for _, inv := range updated.Investigations {
		if inv.Status == InvestigationReviewing && inv.UserAnswer != "" {
			go s.resumeClarifiedInvestigation(id, inv.ID)
		}
	}
	if updated.SourceMode == "demo_replay" {
		seen := make(map[string]bool, len(updated.Events))
		for _, event := range updated.Events {
			seen[event.ID] = true
		}
		remaining := make([]SignalEvent, 0)
		for _, event := range demoSignals(updated) {
			if !seen[event.ID] {
				remaining = append(remaining, event)
			}
		}
		go s.replay(runCtx, id, remaining)
	} else if updated.SourceMode == "browser_ocr" && len(updated.Rooms) > 0 {
		go s.captureLoop(runCtx, id, updated.TargetURL, updated.Rooms[0].ID)
	}
	s.onUpdate()
	return updated, nil
}

func (s *Service) StopAll() {
	s.mu.Lock()
	var ids []string
	var cancels []context.CancelFunc
	var queues []*signalQueue
	for id, cancel := range s.cancels {
		ids = append(ids, id)
		cancels = append(cancels, cancel)
		if q := s.pending[id]; q != nil {
			queues = append(queues, q)
			delete(s.pending, id)
		}
		delete(s.cancels, id)
		delete(s.contexts, id)
		delete(s.lastUnexpectedReview, id)
	}
	s.mu.Unlock()
	for _, q := range queues {
		q.invalidate(0, true)
	}
	for _, cancel := range cancels {
		cancel()
	}
	for _, id := range ids {
		_, err := s.repository.Update(context.Background(), id, func(run *Run) error {
			if run.Status != RunRunning {
				return nil
			}
			run.Status, run.StopReason = RunPaused, "service_shutdown"
			run.LastError = "本地服务已关闭；启动服务后可点击继续，原观测保留"
			for i := range run.Rooms {
				run.Rooms[i].Status = "paused"
			}
			return nil
		})
		if err != nil {
			log.Printf("save shutdown state failed: monitor=%q error=%q", id, err)
		}
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
	if s.contexts[runID] != runCtx {
		s.mu.Unlock()
		return
	}
	q := s.pending[runID]
	delete(s.pending, runID)
	delete(s.cancels, runID)
	delete(s.contexts, runID)
	delete(s.lastUnexpectedReview, runID)
	s.mu.Unlock()
	if q != nil {
		q.invalidate(0, true)
	}
}

func (s *Service) recordCaptureMetrics(runID string, capturedAt time.Time, intervalMS, durationMS int64) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		run.Sampling.CaptureCount++
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

// Temporary acquisition failures are retryable. Only explicit verification or
// the consecutive-failure threshold puts the source into an admission-blocking state.
func (s *Service) setCaptureError(ctx context.Context, runID string, goalVersion uint64, captureErr error, pause bool) (bool, error) {
	updated, err := s.repository.Update(ctx, runID, func(run *Run) error {
		if run.Status != RunRunning {
			return ErrInvalidTransition
		}
		if run.GoalVersion != goalVersion {
			return ErrGoalChanged
		}
		if len(run.Rooms) == 0 {
			return nil
		}
		room := &run.Rooms[0]
		if room.Health == SourceNeedsLogin || room.Health == SourceFailed {
			return nil
		}
		message := captureErr.Error()
		if containsAny(strings.ToLower(message), []string{"login", "captcha", "验证", "登录"}) {
			room.Health = SourceNeedsLogin
			room.LastError = "页面需要登录或验证，已暂停采集；处理后请暂停再点击继续"
		} else if pause {
			room.Health = SourceFailed
			room.LastError = "采集连续失败，已暂停；处理后请暂停再点击继续：" + message
		} else {
			room.Health = SourceDelayed
			room.LastError = "采集暂时失败，正在退避重试：" + message
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	s.onUpdate()
	paused := len(updated.Rooms) > 0 && (updated.Rooms[0].Health == SourceNeedsLogin || updated.Rooms[0].Health == SourceFailed)
	return paused, nil
}

// A stale capture cannot clear login/pause state or revive a stopped/new-goal run.
func (s *Service) clearCaptureError(ctx context.Context, runID string, goalVersion uint64, duration ...time.Duration) error {
	_, err := s.repository.Update(ctx, runID, func(run *Run) error {
		if run.Status != RunRunning {
			return ErrInvalidTransition
		}
		if run.GoalVersion != goalVersion {
			return ErrGoalChanged
		}
		if len(run.Rooms) == 0 {
			return nil
		}
		room := &run.Rooms[0]
		if room.Health == SourceNeedsLogin || room.Health == SourceFailed {
			return ErrInputPaused
		}
		room.LastError = ""
		room.Health = SourceHealthy
		if len(duration) > 0 {
			room.PerceptionDurationMS = duration[0].Milliseconds()
		}
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
	return err
}
func (s *Service) RecoverInterrupted(ctx context.Context) error {
	runs, err := s.repository.List(ctx)
	if err != nil {
		return err
	}
	durable, hasDurableQueue := s.repository.(DurablePendingQueue)
	for _, run := range runs {
		if run.Status == RunRunning {
			if _, err := s.repository.Update(ctx, run.ID, func(current *Run) error {
				if current.Status != RunRunning {
					return nil
				}
				current.Status = RunPaused
				current.CompletedAt = nil
				current.StopReason = "process_interrupted"
				current.Pending = &PendingStatus{LastIssue: "未完成工作在进程重启时中断；有效待处理信号可在继续后恢复"}
				current.LastError = "服务重启中断了上次运行，已暂停；点击继续会重新采集，并恢复仍有效的待处理信号"
				for i := range current.Rooms {
					current.Rooms[i].Status = "paused"
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if hasDurableQueue && (run.Status == RunRunning || run.Status == RunPaused) {
			if err := durable.RecoverPending(ctx, run.ID, run.GoalVersion, time.Now().UTC()); err != nil {
				return err
			}
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
		if err := s.SubmitSignal(ctx, runID, event); err != nil {
			if errors.Is(err, ErrPendingFull) || errors.Is(err, ErrFrameBudget) || errors.Is(err, ErrGoalChanged) {
				continue
			}
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
	return s.processAccepted(ctx, runID, snapshot, acceptedEvent, false, 0)
}

// Both synchronous compatibility callers and the background queue use this
// same decision/commit implementation; queued work waits instead of dropping.
func (s *Service) processAccepted(ctx context.Context, runID string, snapshot *Run, event SignalEvent, queued bool, priority int) error {
	if snapshot.GoalVersion > 1 && event.ObservedAt.Before(snapshot.GoalUpdatedAt) {
		return nil
	}
	eventType := classify(event.Text, snapshot.Goal)
	var review *UnexpectedEventDecision
	var quotaFailure bool
	var err error
	// A configured model handles arbitrary goal conditions as well as proactive
	// events. Keyword matches cannot bypass semantic checks. During the cooldown
	// signals remain in the evidence window; no rule-based alert is substituted.
	_, reviewer, _ := s.agentConfig()
	snapshot.Plan = s.planForRuntime(snapshot, reviewer)
	if eventType != "human_verification" && reviewer != nil {
		attemptID := domain.NewID("review")
		sampling := s.effectiveSampling(snapshot, time.Now().UTC())
		var release func()
		var allowed, overloaded bool
		if queued {
			if time.Until(signalDeadline(event)) < 15*time.Second {
				return context.DeadlineExceeded
			}
			requested := samplingAt(snapshot, time.Now().UTC()).ReviewIntervalSeconds
			if priority > 0 || pendingPriority(event) > 0 || (snapshot.SourceMode != "browser_ocr" && time.Until(signalDeadline(event)) < time.Minute) {
				requested = -1
			}
			slotWaitStarted := time.Now()
			log.Printf("agent review waiting for slot: monitor=%q review_id=%q attempt_id=%q event_id=%q minimum_interval_seconds=%d signal_remaining_ms=%d", runID, event.ID, attemptID, event.ID, requested, time.Until(signalDeadline(event)).Milliseconds())
			waitCtx, waitCancel := context.WithDeadline(ctx, signalDeadline(event))
			release, err = s.awaitReview(waitCtx, runID, requested)
			waitCancel()
			if err != nil {
				log.Printf("agent review slot wait ended: monitor=%q review_id=%q attempt_id=%q event_id=%q wait_ms=%d error=%q", runID, event.ID, attemptID, event.ID, time.Since(slotWaitStarted).Milliseconds(), err)
				return err
			}
			log.Printf("agent review slot acquired: monitor=%q review_id=%q attempt_id=%q event_id=%q wait_ms=%d", runID, event.ID, attemptID, event.ID, time.Since(slotWaitStarted).Milliseconds())
			allowed = true
			// Reload after waiting so previous alerts are visible. The
			// triggering signal itself remains in the bounded evidence input.
			current, getErr := s.repository.Get(ctx, runID)
			if getErr != nil {
				release()
				return getErr
			}
			if current.Status != RunRunning || current.GoalVersion != snapshot.GoalVersion {
				release()
				return ErrGoalChanged
			}
			snapshot = current
			snapshot.Plan = s.planForRuntime(snapshot, reviewer)
			sampling = s.effectiveSampling(snapshot, time.Now().UTC())
		} else {
			release, allowed, overloaded = s.beginReview(runID, time.Now().UTC(), sampling.ReviewIntervalSeconds)
		}
		if overloaded {
			s.recordReviewBackpressure(runID)
		}
		if allowed {
			defer release()
			if queued && snapshot.SourceMode == "browser_ocr" && event.Source == SignalOCR && pendingPriority(event) == 0 {
				s.mu.Lock()
				q := s.pending[runID]
				s.mu.Unlock()
				if q != nil {
					var cleanup func()
					event, cleanup = q.latestLiveTrigger(event, snapshot.GoalVersion)
					if cleanup != nil {
						defer cleanup()
					}
				}
				eventType = classify(event.Text, snapshot.Goal)
			}
			// Check after waiting for both cadence and a global slot. A request
			// with only seconds left cannot complete a useful investigation.
			if queued && time.Until(signalDeadline(event)) < 15*time.Second {
				return context.DeadlineExceeded
			}
			if queued {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, signalDeadline(event))
				defer cancel()
			}
			recent := []SignalEvent{event}
			for _, e := range evidenceWindow(snapshot, event.RoomID, 6) {
				if e.ID != event.ID && len(recent) < 6 {
					recent = append(recent, e)
				}
			}
			frameCheck := event.Source == SignalFrame
			input := UnexpectedEventInput{
				RunID:       runID,
				AttemptID:   attemptID,
				EvidenceAt:  time.Now().UTC(),
				GoalVersion: snapshot.GoalVersion, GoalUpdatedAt: snapshot.GoalUpdatedAt,
				Goal: snapshot.Goal, UserInstructions: append([]string(nil), snapshot.UserInstructions...),
				Plan: snapshot.Plan, Baseline: snapshot.Baseline, Sampling: sampling, FrameCheck: frameCheck, RecentEvidence: recent,
				RecentAlerts:           snapshot.Alerts,
				RecentMemory:           selectInvestigationMemory(snapshot.InvestigationMemory, snapshot.GoalVersion, event.RoomID, time.Now().UTC()),
				TriggerFrameObservedAt: event.ObservedAt,
				UpdateGoal: func(updateCtx context.Context, goal string) (*Run, error) {
					current, getErr := s.repository.Get(updateCtx, runID)
					if getErr != nil {
						return nil, getErr
					}
					if strings.TrimSpace(current.Goal) == strings.TrimSpace(goal) {
						return current, nil
					}
					return s.UpdateGoal(updateCtx, runID, goal)
				},
				SearchObservations: func(queryCtx context.Context, query ObservationQuery) (ObservationPage, error) {
					ledger, ok := s.repository.(RunLedger)
					if !ok {
						return ObservationPage{}, errors.New("repository does not support observation search")
					}
					return ledger.SearchObservations(queryCtx, runID, query)
				},
				ArchivedFrame: func(queryCtx context.Context, observationID string) (FrameAsset, error) {
					return s.archivedObservationFrame(queryCtx, runID, observationID)
				},
			}
			_, err := s.repository.Update(ctx, runID, func(run *Run) error {
				if run.Status != RunRunning || run.GoalVersion != snapshot.GoalVersion {
					return ErrGoalChanged
				}
				run.Sampling.ReviewCount++
				if frameCheck || (event.FramePath != "" && run.Plan.VisionPolicy == "goal_required") {
					run.Sampling.LastVisualAttemptAt = time.Now().UTC()
				}
				return nil
			})
			if err != nil {
				return err
			}
			if snapshot.SourceMode == "browser_ocr" && s.capturer != nil {
				input.CaptureFrame = func(captureCtx context.Context) (FrameAsset, error) {
					captureCtx, cancel := context.WithTimeout(captureCtx, 12*time.Second)
					defer cancel()
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
					capturedAt := time.Now().UTC()
					observation := SignalEvent{ID: domain.NewID("frame"), RoomID: event.RoomID, GoalVersion: snapshot.GoalVersion, Source: SignalFrame, Text: "Agent 请求查看此画面", ObservedAt: capturedAt}
					var ocrEvidence *SignalEvent
					if s.recognizer != nil {
						ocrCtx, cancelOCR := context.WithTimeout(captureCtx, 8*time.Second)
						recognized, ocrErr := s.recognizer.Recognize(ocrCtx, VideoFrame{RoomID: event.RoomID, Path: framePath})
						cancelOCR()
						if ocrErr == nil && strings.TrimSpace(recognized.Text) != "" {
							recognized.ID = observation.ID + ":ocr"
							recognized.ParentEventID = observation.ID
							recognized.RoomID, recognized.GoalVersion = event.RoomID, snapshot.GoalVersion
							recognized.Source = SignalOCR
							recognized.ObservedAt = capturedAt
							recognized.ValidUntil = signalDeadline(recognized)
							ocrEvidence = &recognized
						}
					}
					return FrameAsset{Path: framePath, ObservedAt: capturedAt, ObservationID: observation.ID, OCR: ocrEvidence}, nil
				}
				input.RecordFrameEvidence = func(saveCtx context.Context, asset FrameAsset) error {
					current, getErr := s.repository.Get(saveCtx, runID)
					if getErr != nil {
						return getErr
					}
					if current.Status != RunRunning || current.GoalVersion != snapshot.GoalVersion {
						return ErrGoalChanged
					}
					if ledger, ok := s.repository.(RunLedger); ok && asset.OCR != nil {
						if err := ledger.RecordObservation(saveCtx, runID, *asset.OCR); err != nil {
							return err
						}
					}
					return nil
				}
			}
			decision, reviewErr := reviewer.Review(ctx, input)
			if reviewErr == nil {
				if reviewDecisionIncomplete(decision) {
					decision.Failure = reviewIncompleteMessage(decision.StopReason)
					log.Printf("unexpected livestream review incomplete: model=%s review_id=%q attempt_id=%q event_id=%q stop_reason=%s turns=%d confidence=%.2f input_tokens=%d output_tokens=%d latency_ms=%d", decision.Model, event.ID, decision.AttemptID, event.ID, decision.StopReason, decision.Turns, decision.Confidence, decision.InputTokens, decision.OutputTokens, decision.LatencyMS)
				} else {
					log.Printf("unexpected livestream review completed: model=%s review_id=%q attempt_id=%q event_id=%q action=%s confidence=%.2f input_tokens=%d output_tokens=%d latency_ms=%d", decision.Model, event.ID, decision.AttemptID, event.ID, decision.Action, decision.Confidence, decision.InputTokens, decision.OutputTokens, decision.LatencyMS)
				}
				review = &decision
			} else {
				decision.Failure = "AI 暂时无法完成判断，监控会继续运行"
				if errors.Is(reviewErr, ErrModelQuotaExhausted) {
					quotaFailure = true
					decision.Failure = quotaStopMessage
				}
				switch {
				case decision.StopReason == "cancelled" && errors.Is(reviewErr, context.DeadlineExceeded):
					decision.Failure = "信号在排队或调查期间过期，本次未完成判断；监控会继续运行"
				case decision.StopReason == "timeout":
					decision.Failure = "Agent 调查超过总时限，本次未完成判断；监控会继续运行"
				case decision.StopReason == "model_error" && errors.Is(reviewErr, context.DeadlineExceeded):
					decision.Failure = "模型请求超时，本次未完成判断；监控会继续运行"
				}
				review = &decision
				if decision.StopReason != "circuit_open" && !errors.Is(reviewErr, context.Canceled) {
					log.Printf("unexpected livestream review failed: model=%s review_id=%q attempt_id=%q event_id=%q stop_reason=%s signal_age_ms=%d signal_remaining_ms=%d turns=%d input_tokens=%d output_tokens=%d latency_ms=%d error=%q", decision.Model, event.ID, decision.AttemptID, event.ID, decision.StopReason, time.Since(event.ObservedAt).Milliseconds(), time.Until(signalDeadline(event)).Milliseconds(), decision.Turns, decision.InputTokens, decision.OutputTokens, decision.LatencyMS, reviewErr)
				}
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err = s.repository.Update(ctx, runID, func(run *Run) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		// Human input wins: an older investigation cannot alert or alter the
		// sampling strategy for a goal that the user has replaced.
		if run.GoalVersion != snapshot.GoalVersion {
			return nil
		}
		for i := range run.Rooms {
			if run.Rooms[i].ID == event.RoomID {
				if event.ObservedAt.After(run.Rooms[i].LastSeen) {
					run.Rooms[i].LastSeen = event.ObservedAt
				}
			}
		}
		if eventType == "human_verification" || (reviewer == nil && eventType != "") {
			investigate(run, event, eventType)
			if eventType == "human_verification" {
				for i := range run.Rooms {
					if run.Rooms[i].ID == event.RoomID {
						run.Rooms[i].Health = SourceNeedsLogin
						run.Rooms[i].LastError = "页面要求完成登录或安全验证"
					}
				}
			}
		} else if review != nil {
			if review.Action == "alert" && review.EvidenceBasis == "live" {
				for _, newer := range run.Events {
					if newer.Sequence > event.Sequence && !newer.ObservedAt.Before(event.ObservedAt) && newer.RoomID == event.RoomID && (pendingPriority(newer) > 0 || numericRevision(event, newer)) {
						review.Failure = "调查期间出现较新的取消、更正或数值变化，旧结论未提交，等待新观测复核"
						break
					}
				}
			}
			if event.Source == SignalFrame && review.Action == "alert" && (!hasReviewTool(review.ToolCalls, "inspect_live_frame") || strings.TrimSpace(review.VisualFinding) == "") {
				review.Failure = "画面检查缺少有效看图证据，未生成提醒"
			}
			applyUnexpectedReview(run, event, *review)
			applySamplingDecision(run, *review, time.Now().UTC())
			run.Sampling = s.effectiveSampling(run, time.Now().UTC())
			if hasReviewTool(review.ToolCalls, "inspect_live_frame") {
				run.Sampling.VisualCount++
				run.Sampling.LastVisualAttemptAt = time.Now().UTC()
			}
			if len(run.Investigations) > 0 {
				metrics := run.Investigations[len(run.Investigations)-1].Review
				if metrics != nil {
					metrics.SamplingProposal = review.Sampling
					metrics.SamplingPolicy = run.Sampling
				}
			}
		}
		return nil
	})
	if err == nil {
		if eventType == "human_verification" {
			_, err = s.pauseForHuman(context.Background(), runID)
			if err != nil {
				return err
			}
		}
		if quotaFailure {
			s.endQuotaExhaustedRuns(runID, reviewer)
		}
		s.onUpdate()
	}
	return err
}

// pauseForHuman waits for the user to handle a verification challenge while
// keeping the dedicated browser tab open. Resume can then restart collection
// in that same authenticated tab after the user confirms they are done.
func (s *Service) pauseForHuman(ctx context.Context, id string) (*Run, error) {
	s.mu.Lock()
	cancel, runCtx, q := s.cancels[id], s.contexts[id], s.pending[id]
	s.mu.Unlock()
	var pendingStatus *PendingStatus
	if q != nil {
		status := q.stats()
		status.Waiting, status.Reviewing = 0, false
		pendingStatus = &status
	}
	updated, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status == RunPaused {
			return nil
		}
		if run.Status != RunRunning {
			return ErrInvalidTransition
		}
		run.Status, run.StopReason = RunPaused, "human_verification"
		if pendingStatus != nil {
			run.Pending = pendingStatus
		}
		for i := range run.Rooms {
			run.Rooms[i].Status = "paused"
		}
		for i := range run.Investigations {
			inv := &run.Investigations[i]
			if inv.Status == InvestigationReviewing && inv.UserAnswer != "" {
				if err := TransitionInvestigation(inv, InvestigationWaitingUser); err != nil {
					return err
				}
				inv.Conclusion = "监控等待你完成登录或安全验证；确认处理后发送消息即可继续"
				inv.UpdatedAt = time.Now().UTC()
			}
		}
		return nil
	})
	if err != nil {
		return updated, err
	}
	if q != nil {
		q.invalidate(0, true)
		if q.durable != nil {
			if err := q.durable.RecoverPending(context.Background(), id, updated.GoalVersion, q.now()); err != nil {
				log.Printf("recover pending signals while waiting for verification failed: monitor=%s error=%q", id, err)
			}
		}
	}
	if cancel != nil {
		cancel()
	}
	s.mu.Lock()
	if s.contexts[id] == runCtx {
		delete(s.cancels, id)
		delete(s.contexts, id)
		delete(s.pending, id)
		delete(s.lastUnexpectedReview, id)
	}
	s.mu.Unlock()
	s.onUpdate()
	return updated, nil
}

func reviewDecisionIncomplete(decision UnexpectedEventDecision) bool {
	return decision.StopReason != "" && decision.StopReason != "finished" && decision.StopReason != "terminal_tool"
}

func (s *Service) acceptSignal(ctx context.Context, runID string, event SignalEvent) (*Run, SignalEvent, bool, error) {
	return s.acceptSignalAndEnqueue(ctx, runID, event, nil)
}

func (s *Service) acceptSignalAndEnqueue(ctx context.Context, runID string, event SignalEvent, pending *pendingSignal) (*Run, SignalEvent, bool, error) {
	if !utf8.ValidString(event.Text) || strings.TrimSpace(event.Text) == "" || utf8.RuneCountInString(event.Text) > maxSignalTextRunes {
		return nil, event, false, errors.New("signal text must contain 1-4000 characters")
	}
	if event.Source != SignalOCR && event.Source != SignalASR && !(event.Source == SignalFrame && event.FramePath != "") {
		return nil, event, false, errors.New("signal source must be ocr or asr; frame checks require an internal captured image")
	}
	if math.IsNaN(event.Confidence) || math.IsInf(event.Confidence, 0) || event.Confidence < 0 || event.Confidence > 1 {
		return nil, event, false, errors.New("signal confidence must be between 0 and 1")
	}
	now := time.Now().UTC()
	if event.ObservedAt.IsZero() {
		event.ObservedAt = now
	}
	event.ObservedAt = event.ObservedAt.UTC()
	event.ValidUntil = signalDeadline(event)
	if event.ObservedAt.After(now.Add(maxFutureSignalSkew)) {
		return nil, event, false, ErrFutureSignal
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
	apply := func(run *Run) (pendingSignal, bool, error) {
		if run.Status != RunRunning {
			return pendingSignal{}, false, errors.New("monitor is not running")
		}
		if pending != nil && run.GoalVersion > 1 && event.ObservedAt.Before(run.GoalUpdatedAt) {
			return pendingSignal{}, false, ErrGoalChanged
		}
		if event.GoalVersion != 0 && event.GoalVersion != run.GoalVersion {
			return pendingSignal{}, false, ErrGoalChanged
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
					return pendingSignal{}, false, ErrEventIDConflict
				}
				duplicate = true
				return pendingSignal{}, false, nil
			}
		}
		run.EventSequence++
		event.GoalVersion = run.GoalVersion
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
					if run.SourceMode != "browser_ocr" {
						run.Rooms[i].Health = SourceHealthy
						run.Rooms[i].LastError = ""
					}
				}
			}
		}
		if pending == nil {
			return pendingSignal{}, false, nil
		}
		item := *pending
		item.event = event
		item.version = run.GoalVersion
		item.priority = pendingPriority(event)
		return item, true, nil
	}
	var updated *Run
	var updateErr error
	if pending != nil {
		if store, ok := s.repository.(DurablePendingQueue); ok {
			updated, updateErr = store.UpdateAndEnqueuePending(ctx, runID, apply)
		} else {
			updated, updateErr = s.repository.Update(ctx, runID, func(run *Run) error {
				_, _, applyErr := apply(run)
				return applyErr
			})
		}
	} else {
		updated, updateErr = s.repository.Update(ctx, runID, func(run *Run) error {
			_, _, applyErr := apply(run)
			return applyErr
		})
	}
	if updateErr != nil {
		return nil, event, false, updateErr
	}
	if duplicate {
		return updated, event, true, nil
	}
	framePath := event.FramePath
	for _, stored := range updated.Events {
		if stored.ID == event.ID {
			event = stored
			break
		}
	}
	// Repository snapshots deliberately omit temporary file paths. Preserve
	// this request metadata for the pending review lifetime.
	event.FramePath = framePath
	event.GoalVersion = updated.GoalVersion
	return updated, event, false, nil
}

func (s *Service) beginReview(runID string, at time.Time, requestedSeconds int) (func(), bool, bool) {
	_, _, configuredGap := s.agentConfig()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reviewWaiters) > 0 {
		return nil, false, true
	}
	if s.reviewInFlight[runID] {
		return nil, false, false
	}
	interval := configuredGap
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
	return s.reviewReleaseLocked(runID), true, false
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
	return s.ingestFrame(ctx, runID, frame, s.Ingest)
}

func (s *Service) SubmitFrame(ctx context.Context, runID string, frame VideoFrame) error {
	return s.ingestFrame(ctx, runID, frame, s.SubmitSignal)
}

func (s *Service) ingestFrame(ctx context.Context, runID string, frame VideoFrame, submit func(context.Context, string, SignalEvent) error) error {
	ctx, release := s.monitorContext(ctx, runID)
	defer release()
	if s.recognizer == nil {
		return errors.New("ocr recognizer is not configured")
	}
	if err := s.requireLiveInput(ctx, runID); err != nil {
		return err
	}
	started := time.Now()
	event, err := s.recognizer.Recognize(ctx, frame)
	if err != nil && !errors.Is(err, ErrNoText) {
		return err
	}
	if event.ID == "" {
		event.ID = frame.EventID
	}
	if event.ID == "" {
		event.ID = domain.NewID("observation")
	}
	if event.Source == "" {
		event.Source = SignalOCR
	}
	if event.ObservedAt.IsZero() {
		event.ObservedAt = time.Now().UTC()
	}
	event.FramePath = frame.Path
	event.PerceptionDurationMS = time.Since(started).Milliseconds()
	if errors.Is(err, ErrNoText) {
		event.Source, event.Text = SignalFrame, "本次画面没有识别到文字"
		if ledger, ok := s.repository.(RunLedger); ok {
			if recordErr := ledger.RecordObservation(ctx, runID, event); recordErr != nil {
				return recordErr
			}
		}
		return err
	}
	return submit(ctx, runID, event)
}

// IngestAudio transcribes one short, user-authorized audio chunk and sends the
// text through the same Agent decision path as OCR and other signals.
func (s *Service) IngestAudio(ctx context.Context, runID string, chunk AudioChunk) error {
	return s.ingestAudio(ctx, runID, chunk, s.Ingest)
}

func (s *Service) SubmitAudio(ctx context.Context, runID string, chunk AudioChunk) error {
	return s.ingestAudio(ctx, runID, chunk, s.SubmitSignal)
}

func (s *Service) ingestAudio(ctx context.Context, runID string, chunk AudioChunk, submit func(context.Context, string, SignalEvent) error) error {
	ctx, release := s.monitorContext(ctx, runID)
	defer release()
	if s.transcriber == nil {
		return errors.New("ASR is not configured")
	}
	if err := s.requireLiveInput(ctx, runID); err != nil {
		return err
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
	return submit(ctx, runID, event)
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
	applyUnexpectedReviewTo(run, event, decision, "")
}

func applyUnexpectedReviewTo(run *Run, event SignalEvent, decision UnexpectedEventDecision, investigationID string) {
	if decision.Action == "ask_user" && !hasReviewTool(decision.ToolCalls, "request_user_input") {
		decision.Failure = "本次求助没有通过服务端工具授权，未向用户发送问题"
	}
	if decision.Action == "alert" && !validCommittedCitations(run, event, decision, time.Now().UTC()) {
		decision.Failure = "引用证据已过期、来自旧目标或缺少明确引用，本次未提醒"
	}
	if decision.Action == "alert" && reviewRequiresVision(UnexpectedEventInput{Goal: run.Goal, Plan: run.Plan, FrameCheck: event.Source == SignalFrame}) &&
		(!hasReviewTool(decision.ToolCalls, "inspect_live_frame") || strings.TrimSpace(decision.VisualFinding) == "") {
		decision.Failure = "该目标需要实际看图证据，文字判断不能生成画面或聊天区提醒"
	}
	now := time.Now().UTC()
	eventType := "proactive_" + decision.Category
	inv := Investigation{
		GoalVersion: run.GoalVersion,
		ID:          domain.NewID("investigation"), RoomID: event.RoomID, RoomName: event.RoomName,
		EventType: eventType, Status: InvestigationSuppressed, Reason: strings.TrimSpace(decision.Reason), CreatedAt: now, UpdatedAt: now,
		Evidence: []Evidence{{EventID: event.ID, Source: string(event.Source), Text: event.Text, Confidence: event.Confidence, ObservedAt: event.ObservedAt}},
	}
	existingIndex := -1
	if investigationID != "" {
		for i := range run.Investigations {
			if run.Investigations[i].ID == investigationID {
				existingIndex = i
				inv = run.Investigations[i]
				break
			}
		}
		if existingIndex < 0 {
			return
		}
		eventType = inv.EventType
		if decision.Category != "" {
			eventType = "proactive_" + decision.Category
			inv.EventType = eventType
		}
	}
	commit := func() {
		inv.UpdatedAt = now
		if existingIndex >= 0 {
			run.Investigations[existingIndex] = inv
		} else {
			run.Investigations = append(run.Investigations, inv)
		}
		updateInvestigationMemory(run, inv, decision, now)
	}
	inv.Review = &ReviewMetrics{Model: decision.Model, InputTokens: decision.InputTokens, OutputTokens: decision.OutputTokens, LatencyMS: decision.LatencyMS, Turns: decision.Turns, StopReason: decision.StopReason, HistoryRecordsReused: decision.HistoryRecordsReused}
	inv.Review.TraceID = decision.TraceID
	inv.Review.FormatRepairs = decision.Repairs
	inv.Review.TransportRetries = decision.TransportRetries
	if len(decision.UsedEvidence) > 0 {
		seen := map[string]bool{}
		for _, item := range inv.Evidence {
			seen[item.EventID] = true
		}
		for _, item := range decision.UsedEvidence {
			if !seen[item.EventID] {
				inv.Evidence = append(inv.Evidence, item)
				seen[item.EventID] = true
			}
		}
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
	visualFinding := strings.TrimSpace(decision.VisualFinding)
	if visualFinding == "" && decision.Action != "alert" && decision.Action != "ask_user" && decision.VisualArchiveID != "" {
		visualFinding = strings.TrimSpace(decision.Reason)
	}
	if visualFinding != "" && hasReviewTool(decision.ToolCalls, "inspect_live_frame") && hasReviewTool(decision.ToolCalls, "save_frame_evidence") {
		visualAt := decision.VisualObservedAt
		if visualAt.IsZero() {
			visualAt = now
		}
		inv.Evidence = append(inv.Evidence, Evidence{EventID: decision.VisualEvidenceID, RoomID: event.RoomID, GoalVersion: run.GoalVersion, Source: "vision", Text: visualFinding, Confidence: decision.Confidence, ObservedAt: visualAt})
	}
	if decision.Failure != "" {
		inv.Status = InvestigationFailed
		if reviewExecutionIncomplete(decision.StopReason) {
			inv.Status = InvestigationIncomplete
		}
		inv.Conclusion = decision.Failure
		commit()
		return
	}
	if decision.Action == "ask_user" {
		// Keep clarification scoped to one outstanding question per monitor.
		// New signals still get reviewed, but their evidence is folded into the
		// pending investigation instead of creating another answer form.
		for i := range run.Investigations {
			pending := &run.Investigations[i]
			if i == existingIndex || pending.Status != InvestigationWaitingUser || pending.GoalVersion != run.GoalVersion {
				continue
			}
			mergeInvestigationEvidence(pending, inv.Evidence)
			pending.UpdatedAt = now
			run.Investigations[i] = *pending
			inv.Status = InvestigationSuppressed
			inv.UserQuestion = ""
			inv.UserOptions = nil
			inv.Conclusion = "已并入待补充的问题；回复后会一并复核这些信号"
			commit()
			return
		}
		inv.Status = InvestigationWaitingUser
		inv.UserQuestion = strings.TrimSpace(decision.UserQuestion)
		inv.UserOptions = append([]string(nil), decision.UserOptions...)
		inv.Conclusion = inv.UserQuestion
		inv.Reason = strings.TrimSpace(decision.Reason)
		commit()
		return
	}
	if decision.Action == "alert" && (decision.Category == "chat_match" || run.Plan.AlertScope == "chat" || goalNeedsChatRegion(run.Goal)) {
		ocrCited := false
		for _, cited := range decision.UsedEvidence {
			if cited.Source == string(SignalOCR) && (cited.EventID == decision.VisualOCREvidenceID || (cited.FrameArchiveID != "" && cited.FrameArchiveID == decision.VisualArchiveID)) {
				ocrCited = true
			}
		}
		if !ocrCited || strings.TrimSpace(decision.VisualFinding) == "" || !hasReviewTool(decision.ToolCalls, "inspect_live_frame") || !hasReviewTool(decision.ToolCalls, "save_frame_evidence") {
			inv.Conclusion = "聊天区提醒缺少 OCR 或看图证据，已跳过"
			commit()
			return
		}
	}
	if decision.Action == "wait" {
		inv.Status = InvestigationWaiting
		inv.Conclusion = "出现可能值得关注的内容，证据还不够明确，继续观察"
		if reason := strings.TrimSpace(decision.Reason); reason != "" {
			inv.Conclusion = reason
		}
		if decision.StopReason != "finished" && decision.StopReason != "terminal_tool" && decision.StopReason != "" {
			inv.Conclusion = "本次调查达到" + reviewStopLabel(decision.StopReason) + "，继续监听新证据"
		}
		commit()
		return
	}
	if decision.Action != "alert" || decision.Confidence < 0.72 {
		inv.Conclusion = strings.TrimSpace(decision.Reason)
		if inv.Conclusion == "" {
			inv.Conclusion = "没有发现需要提醒的明确证据"
		}
		commit()
		return
	}
	if !hasReviewTool(decision.ToolCalls, "search_recent_alerts") {
		addStep(&inv, "query_recent_alerts", "检查是否已经提醒过同类突发内容", "已检查当前监控窗口的提醒记录", now)
	}
	if hasRecentSemanticAlert(run.Alerts, event, eventType, decision.Summary) {
		inv.Conclusion = "同一类内容已经提醒，本次提醒已去重"
		commit()
		return
	}
	inv.Status = InvestigationConfirmed
	inv.Conclusion = decision.Summary
	if !hasReviewTool(decision.ToolCalls, "create_alert") {
		addStep(&inv, "create_alert", "将目标之外的重要时刻提醒给用户", "已生成带识别原文的主动提醒", now)
	}
	commit()
	receivedAt := event.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = event.ObservedAt
	}
	run.Alerts = append(run.Alerts, Alert{
		EvidenceBasis:     decision.EvidenceBasis,
		SignalToAlertMS:   max(0, now.Sub(event.ObservedAt).Milliseconds()),
		ReceivedToAlertMS: max(0, now.Sub(receivedAt).Milliseconds()), ReceivedAt: receivedAt,
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

func reviewExecutionIncomplete(reason string) bool {
	switch reason {
	case "turn_budget", "call_budget", "repeated_tool", "timeout", "cancelled":
		return true
	default:
		return false
	}
}

func reviewIncompleteMessage(reason string) string {
	switch reason {
	case "turn_budget":
		return "本次调查达到轮数上限，未判断目标是否出现或规则是否需要澄清；监控会继续监听新信号。"
	case "call_budget", "repeated_tool":
		return "本次调查达到工具执行限制，未判断目标是否出现；监控会继续监听新信号。"
	case "timeout":
		return "本次调查超过执行时限，未判断目标是否出现；监控会继续监听新信号。"
	case "cancelled":
		return "本次调查在信号有效期内未完成，未判断目标是否出现；监控会继续监听新信号。"
	default:
		return "本次调查未完成，未判断目标是否出现；监控会继续监听新信号。"
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
	case "search_observations":
		return "检索本场近期或整场观测记录"
	case "search_recent_alerts":
		return "检查是否已经提醒过同类事件"
	case "inspect_live_frame":
		return "查看 Agent 认为需要确认的画面"
	case "request_user_input":
		return "向用户询问只有用户才能定义的判断规则"
	case "save_frame_evidence":
		return "保存 Agent 已检查并选中的画面"
	case "create_alert":
		return "通过策略校验后请求生成提醒"
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
	return room
}

func platformFromURL(parsed *url.URL) string {
	host := strings.ToLower(parsed.Hostname())
	switch {
	case strings.Contains(host, "douyin"):
		return "抖音"
	case strings.Contains(host, "bilibili") || strings.Contains(host, "b23.tv"):
		return "哔哩哔哩"
	case strings.Contains(host, "kuaishou") || strings.Contains(host, "gifshow"):
		return "快手"
	case strings.Contains(host, "taobao") || strings.Contains(host, "tmall"):
		return "淘宝"
	case strings.Contains(host, "channels") || strings.Contains(host, "weixin.qq.com"):
		return "视频号"
	case host != "":
		return host
	default:
		return ""
	}
}

func (s *Service) updatePageTitle(ctx context.Context, runID, roomID, targetURL string) {
	reader, ok := s.capturer.(PageTitleReader)
	if !ok {
		return
	}
	parsed, err := url.Parse(targetURL)
	if err != nil || parsed.Hostname() == "" {
		return
	}
	fallbackName := roomNameFromURL(parsed)
	current, err := s.repository.Get(ctx, runID)
	if err != nil {
		return
	}
	found := false
	for _, room := range current.Rooms {
		if room.ID == roomID {
			found = true
			if room.Name != fallbackName {
				return
			}
			break
		}
	}
	if !found {
		return
	}
	title, err := reader.PageTitle(ctx, runID, targetURL)
	if err != nil {
		return
	}
	title = cleanPageTitle(title)
	if title == "" || title == fallbackName {
		return
	}
	updated, err := s.repository.Update(ctx, runID, func(run *Run) error {
		for i := range run.Rooms {
			if run.Rooms[i].ID == roomID && run.Rooms[i].Name == fallbackName {
				run.Rooms[i].Name = title
				run.Name = title
				break
			}
		}
		return nil
	})
	if err == nil && updated != nil {
		s.onUpdate()
	}
}

func cleanPageTitle(title string) string {
	title = strings.Join(strings.Fields(strings.TrimSpace(title)), " ")
	for _, suffix := range []string{" - 抖音", " - 哔哩哔哩", " - bilibili", " | 抖音", " | 哔哩哔哩", " | bilibili"} {
		title = strings.TrimSpace(strings.TrimSuffix(title, suffix))
	}
	switch strings.ToLower(title) {
	case "抖音", "哔哩哔哩", "bilibili", "快手", "淘宝", "视频号", "直播", "登录", "用户登录", "账号登录":
		return ""
	}
	if strings.Contains(title, "记录美好生活") || strings.Contains(title, "干杯~-bilibili") {
		return ""
	}
	if len([]rune(title)) > 64 {
		title = string([]rune(title)[:64])
	}
	return title
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
		if inv.RoomID == roomID && inv.EventType == eventType && inv.Status == InvestigationWaiting && (run.GoalVersion <= 1 || inv.GoalVersion == run.GoalVersion) {
			return i
		}
	}
	return -1
}

func evidenceWindow(run *Run, roomID string, limit int) []SignalEvent {
	result := make([]SignalEvent, 0, limit)
	for i := len(run.Events) - 1; i >= 0 && len(result) < limit; i-- {
		if run.Events[i].RoomID == roomID && currentEvidence(run.Events[i].ObservedAt, run.Events[i].ValidUntil, run.Events[i].GoalVersion, run.GoalVersion, run.GoalUpdatedAt, time.Now().UTC()) {
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

// Check lifecycle before costly OCR/ASR work. A late upload to an ended run must
// not start a new provider request after its runtime context has been removed.
func (s *Service) requireLiveInput(ctx context.Context, runID string) error {
	run, err := s.repository.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != RunRunning {
		return ErrInvalidTransition
	}
	if len(run.Rooms) > 0 && (run.Rooms[0].Health == SourceNeedsLogin || run.Rooms[0].Health == SourceFailed) {
		return ErrInputPaused
	}
	return nil
}
