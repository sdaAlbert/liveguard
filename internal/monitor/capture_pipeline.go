package monitor

import (
	"context"
	"errors"
	"os"
	"time"

	"liveguard/internal/domain"
)

type capturedSample struct {
	event SignalEvent
	path  string
}
type perceptionResult struct {
	version uint64
	err     error
	ack     chan error
}

// Capture has its own cadence. Frames are temporary OCR input; persistent image
// archives are created only from the Agent's inspect_live_frame tool.
func (s *Service) captureLoop(ctx context.Context, runID, targetURL, roomID string) {
	ctx, cancel := context.WithCancel(ctx)
	jobs := make(chan capturedSample, 2)
	results := make(chan perceptionResult, 1)
	done := make(chan struct{})
	go s.perceptionLoop(ctx, runID, jobs, results, done)
	defer func() { cancel(); close(jobs); <-done }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	var lastCapture time.Time
	var version uint64
	captureFailures, ocrFailures := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-results:
			if result.version != version {
				result.ack <- ErrGoalChanged
				continue
			}
			if result.err == nil {
				ocrFailures = 0
				stateErr := s.clearCaptureError(ctx, runID, version)
				result.ack <- stateErr
				if stateErr != nil && !errors.Is(stateErr, ErrGoalChanged) {
					return
				}
				continue
			}
			ocrFailures++
			paused, err := s.setCaptureError(ctx, runID, version, result.err, ocrFailures >= 3)
			if paused && err == nil {
				err = ErrInputPaused
			}
			result.ack <- err
			if errors.Is(err, ErrGoalChanged) {
				continue
			}
			if err != nil || paused {
				s.closePending(runID)
				return
			}
		case <-timer.C:
			run, err := s.repository.Get(ctx, runID)
			if err != nil || run.Status != RunRunning {
				return
			}
			if len(run.Rooms) > 0 && (run.Rooms[0].Health == SourceFailed || run.Rooms[0].Health == SourceNeedsLogin) {
				s.closePending(runID)
				return
			}
			if version != run.GoalVersion {
				version = run.GoalVersion
				captureFailures, ocrFailures = 0, 0
			}
			started := time.Now().UTC()
			intervalMS := int64(0)
			if !lastCapture.IsZero() {
				intervalMS = started.Sub(lastCapture).Milliseconds()
			}
			lastCapture = started
			sample, duration, captureErr := s.captureSample(ctx, runID, targetURL, roomID, version)
			s.recordCaptureMetrics(runID, started, intervalMS, duration)
			if captureErr != nil {
				if ctx.Err() != nil {
					return
				}
				captureFailures++
				paused, err := s.setCaptureError(ctx, runID, version, captureErr, captureFailures >= 3)
				if errors.Is(err, ErrGoalChanged) {
					timer.Reset(0)
					continue
				}
				if err != nil || paused {
					s.closePending(runID)
					return
				}
			} else {
				captureFailures = 0
				select {
				case jobs <- sample:
				default:
					_ = os.Remove(sample.path)
					s.recordPerceptionDrop(runID)
				}
			}
			gap := time.Duration(s.effectiveSampling(run, time.Now().UTC()).CaptureIntervalSeconds) * time.Second
			if gap <= 0 {
				gap = 6 * time.Second
			}
			if goalNeedsChatRegion(run.Goal) || run.Plan.AlertScope == "chat" {
				gap = min(gap, time.Second)
			}
			if s.captureGap > 0 && s.captureGap < time.Second {
				gap = s.captureGap
			}
			if captureFailures > 0 {
				gap *= time.Duration(1 << captureFailures)
			}
			timer.Reset(max(0, gap-time.Since(started)))
		}
	}
}

func (s *Service) captureSample(ctx context.Context, runID, targetURL, roomID string, version uint64) (capturedSample, int64, error) {
	frame, err := os.CreateTemp("", "liveguard-capture-*.png")
	if err != nil {
		return capturedSample{}, 0, err
	}
	path := frame.Name()
	if err = frame.Close(); err != nil {
		_ = os.Remove(path)
		return capturedSample{}, 0, err
	}
	captureCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	started := time.Now()
	err = s.capturer.Capture(captureCtx, runID, targetURL, path)
	duration := time.Since(started).Milliseconds()
	if err != nil {
		_ = os.Remove(path)
		return capturedSample{}, duration, err
	}
	event := SignalEvent{ID: domain.NewID("frame"), RoomID: roomID, GoalVersion: version, Source: SignalFrame, Text: "临时画面采样，用于 OCR；原图识别后删除", ObservedAt: time.Now().UTC(), CaptureDurationMS: duration}
	event.ValidUntil = signalDeadline(event)
	return capturedSample{event: event, path: path}, duration, nil
}

func (s *Service) perceptionLoop(ctx context.Context, runID string, jobs <-chan capturedSample, results chan<- perceptionResult, done chan<- struct{}) {
	defer close(done)
	defer func() {
		for sample := range jobs {
			_ = os.Remove(sample.path)
		}
	}()
	var deduper OCRLineDeduper
	var version uint64
	for {
		select {
		case <-ctx.Done():
			return
		case sample, ok := <-jobs:
			if !ok {
				return
			}
			err := func() error {
				defer os.Remove(sample.path)
				run, err := s.repository.Get(ctx, runID)
				if err != nil {
					return err
				}
				if run.Status != RunRunning || run.GoalVersion != sample.event.GoalVersion {
					return ErrGoalChanged
				}
				if version != run.GoalVersion {
					version = run.GoalVersion
					deduper = OCRLineDeduper{}
				}
				started := time.Now()
				ocrCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
				focusChat := run.Plan.AlertScope == "chat" || goalNeedsChatRegion(run.Goal)
				event, err := s.recognizer.Recognize(ocrCtx, VideoFrame{RoomID: sample.event.RoomID, Path: sample.path, FocusChat: focusChat})
				cancel()
				if err != nil && !errors.Is(err, ErrNoText) {
					return err
				}
				event.ID = sample.event.ID
				if event.Text != "" {
					event.ID += ":ocr"
					event.ParentEventID = sample.event.ID
				}
				event.RoomID, event.GoalVersion, event.ObservedAt = sample.event.RoomID, sample.event.GoalVersion, sample.event.ObservedAt
				// The OCR input is deleted after recognition. If the Agent needs a
				// picture, its tool captures and archives a fresh frame on demand.
				event.FramePath = ""
				event.CaptureDurationMS, event.PerceptionDurationMS = sample.event.CaptureDurationMS, time.Since(started).Milliseconds()
				if err = coordinatePerception(ctx, results, version, nil); err != nil {
					return err
				}
				err = s.submitCapturedFrame(ctx, runID, event, &deduper)
				if errors.Is(err, ErrPendingFull) || errors.Is(err, ErrFrameBudget) {
					return nil
				}
				return err
			}()
			if errors.Is(err, ErrGoalChanged) || errors.Is(err, ErrInvalidTransition) || errors.Is(err, context.Canceled) || errors.Is(err, ErrInputPaused) {
				continue
			}
			if err != nil {
				if coordinatePerception(ctx, results, sample.event.GoalVersion, err) != nil {
					continue
				}
			}
		}
	}
}

// Source health is changed by the capture coordinator in result order. The
// OCR worker waits for that decision before admitting a recovered observation.
func coordinatePerception(ctx context.Context, results chan<- perceptionResult, version uint64, err error) error {
	ack := make(chan error, 1)
	select {
	case results <- perceptionResult{version: version, err: err, ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case stateErr := <-ack:
		return stateErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) recordPerceptionDrop(runID string) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		if run.Status != RunRunning {
			return ErrInvalidTransition
		}
		run.Sampling.PerceptionDropped++
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
}
