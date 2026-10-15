package monitor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReviewContextBudgetCompactsAndKeepsServerEvidenceOriginal(t *testing.T) {
	observedAt := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	event := SignalEvent{ID: "evidence-1", Source: SignalOCR, Text: "主播正在展示商品", Confidence: .91, ObservedAt: observedAt,
		TextRegions: []TextRegion{{Text: "主播正在展示商品", Area: "main_screen", X: 10, Y: 20, Width: 30, Height: 10}}}
	b := newReviewContextBudget()
	modelEvents, accepted, truncated := b.compactObservationPage([]SignalEvent{event})
	if truncated || len(modelEvents) != 1 || len(accepted) != 1 {
		t.Fatalf("event should fit in fresh budget: model=%+v accepted=%d truncated=%v", modelEvents, len(accepted), truncated)
	}
	if len(accepted[0].TextRegions) != 1 || accepted[0].Text != event.Text {
		t.Fatalf("server evidence was altered by provider-input compaction: %+v", accepted[0])
	}
	encoded, err := json.Marshal(modelEvents[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "text_regions") || strings.Contains(string(encoded), "main_screen") {
		t.Fatalf("duplicate region geometry leaked into compact model context: %s", encoded)
	}
}

func TestReviewContextBudgetStopsAtWholeRecordBoundary(t *testing.T) {
	events := []SignalEvent{
		{ID: "evidence-1", Source: SignalOCR, Text: "first", Confidence: .8},
		{ID: "evidence-2", Source: SignalOCR, Text: "second", Confidence: .8},
	}
	first := compactReviewObservation{ID: events[0].ID, Source: events[0].Source, Text: events[0].Text, Confidence: events[0].Confidence, ObservedAt: events[0].ObservedAt}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b := &reviewContextBudget{remaining: runeCount(string(encoded))}
	modelEvents, accepted, truncated := b.compactObservationPage(events)
	if !truncated || !b.exhausted || len(modelEvents) != 1 || len(accepted) != 1 || accepted[0].ID != events[0].ID {
		t.Fatalf("budget must stop cleanly before a partial record: model=%+v accepted=%+v truncated=%v budget=%+v", modelEvents, accepted, truncated, b)
	}
	if _, more, againTruncated := b.compactObservationPage([]SignalEvent{{ID: "evidence-3", Text: "later"}}); len(more) != 0 || !againTruncated {
		t.Fatalf("an exhausted investigation budget must reject later pages: accepted=%+v truncated=%v", more, againTruncated)
	}
}
