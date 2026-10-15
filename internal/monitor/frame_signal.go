package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"liveguard/internal/domain"
)

// Preserve OCR novelty filtering. An unchanged frame is archived as metadata
// and never becomes an Agent trigger or invented recognized text.
func (s *Service) ingestCapturedFrame(ctx context.Context, runID string, event SignalEvent, deduper *OCRLineDeduper) error {
	return s.handleCapturedFrame(ctx, runID, event, deduper, s.Ingest)
}

func (s *Service) submitCapturedFrame(ctx context.Context, runID string, event SignalEvent, deduper *OCRLineDeduper) error {
	return s.handleCapturedFrame(ctx, runID, event, deduper, s.SubmitSignal)
}

func (s *Service) handleCapturedFrame(ctx context.Context, runID string, event SignalEvent, deduper *OCRLineDeduper, submit func(context.Context, string, SignalEvent) error) error {
	run, err := s.repository.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != RunRunning {
		return ErrInvalidTransition
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
	recordFrame := func(reason string) error {
		if event.FrameArchiveID == "" {
			return nil
		}
		frame := event
		frame.Source, frame.Text, frame.Confidence = SignalFrame, reason, 0
		frame.TextRegions = nil
		ledger, ok := s.repository.(RunLedger)
		if !ok {
			return nil
		}
		return ledger.RecordObservation(ctx, runID, frame)
	}
	if run.GoalVersion > 1 && event.ObservedAt.Before(run.GoalUpdatedAt) {
		return recordFrame("归档截图：目标更新前采样")
	}
	text, commitDedupe := deduper.Preview(event.Text, event.ObservedAt)
	if text != "" {
		event.Text = text
		newLines := map[string]bool{}
		for _, line := range strings.Split(text, "\n") {
			newLines[normalizeOCRLine(line)] = true
		}
		var regions []TextRegion
		for _, region := range event.TextRegions {
			if newLines[normalizeOCRLine(region.Text)] {
				regions = append(regions, region)
			}
		}
		event.TextRegions = regions
		for _, part := range splitCapturedOCR(event) {
			if err := submit(ctx, runID, part); err != nil {
				return err
			}
		}
		commitDedupe()
		return nil
	}
	return recordFrame("归档截图：OCR 与近期文字重复")
}

// OCR is an internal observation, not a user request. Preserve long frames as
// independently addressable ledger entries instead of rejecting the source.
func splitCapturedOCR(event SignalEvent) []SignalEvent {
	remaining := []rune(event.Text)
	if len(remaining) <= maxSignalTextRunes {
		return []SignalEvent{event}
	}
	var parts []SignalEvent
	for len(remaining) > 0 {
		n := min(len(remaining), maxSignalTextRunes)
		if n < len(remaining) {
			for i := n - 1; i > n/2; i-- {
				if remaining[i] == '\n' {
					n = i + 1
					break
				}
			}
		}
		part := event
		part.ID = fmt.Sprintf("%s:part:%d", event.ID, len(parts)+1)
		if part.ParentEventID == "" {
			part.ParentEventID = event.ID
		}
		part.Text = strings.TrimSpace(string(remaining[:n]))
		part.TextRegions = nil
		for _, region := range event.TextRegions {
			for _, line := range strings.Split(part.Text, "\n") {
				if line != "" && (strings.Contains(region.Text, line) || strings.Contains(line, region.Text)) {
					copy := region
					copy.Text = line
					part.TextRegions = append(part.TextRegions, copy)
					break
				}
			}
		}
		if part.Text != "" {
			parts = append(parts, part)
		}
		remaining = remaining[n:]
	}
	return parts
}
