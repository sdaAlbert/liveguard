package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ObservationQuery searches the append-only observation history for one run.
// Limits are enforced by repositories so a model cannot load an entire stream.
type ObservationQuery struct {
	EventID        string
	Before         time.Time
	BeforeSequence uint64
	BeforeEventID  string
	Text           string
	TextAny        []string
	Source         SignalSource
	RoomID         string
	From           time.Time
	Until          time.Time
	Limit          int
}

type ObservationPage struct {
	Events             []SignalEvent
	TotalMatches       int64
	HasMore            bool
	NextBefore         time.Time
	NextBeforeSequence uint64
	NextBeforeEventID  string
}

// RunLedger stores the complete history separately from the bounded Run view.
// FileRepository persists it across process restarts; MemoryRepository keeps
// it for the lifetime of the process.
type RunLedger interface {
	RecordObservation(context.Context, string, SignalEvent) error
	SearchObservations(context.Context, string, ObservationQuery) (ObservationPage, error)
}

func normalizeObservationQuery(query ObservationQuery) ObservationQuery {
	query.Text = strings.TrimSpace(query.Text)
	terms := make([]string, 0, min(len(query.TextAny)+1, 9))
	seen := map[string]bool{}
	for _, term := range append([]string{query.Text}, query.TextAny...) {
		term = strings.TrimSpace(term)
		key := strings.ToLower(term)
		if term == "" || seen[key] || len(terms) == 9 {
			continue
		}
		seen[key] = true
		terms = append(terms, term)
	}
	query.TextAny = terms
	query.Text = ""
	if query.Limit <= 0 || query.Limit > 12 {
		query.Limit = 12
	}
	return query
}

func observationMatchesText(event SignalEvent, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	text := strings.ToLower(event.Text)
	for _, term := range terms {
		if strings.Contains(text, strings.ToLower(term)) {
			return true
		}
	}
	return false
}

func visualObservations(investigation Investigation) []SignalEvent {
	var result []SignalEvent
	for i, evidence := range investigation.Evidence {
		if evidence.Source != string(SignalVision) || strings.TrimSpace(evidence.Text) == "" {
			continue
		}
		result = append(result, SignalEvent{
			ID: fmt.Sprintf("vision:%s:%d", investigation.ID, i), ParentEventID: evidence.EventID,
			RoomID: investigation.RoomID, RoomName: investigation.RoomName,
			Source: SignalVision, Text: evidence.Text, Confidence: evidence.Confidence,
			ObservedAt:  evidence.ObservedAt,
			GoalVersion: investigation.GoalVersion, FrameArchiveID: evidence.FrameArchiveID,
		})
	}
	return result
}

func parseTimeRange(from, until string) (time.Time, time.Time, error) {
	var start, end time.Time
	var err error
	if strings.TrimSpace(from) != "" {
		start, err = time.Parse(time.RFC3339, from)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if strings.TrimSpace(until) != "" {
		end, err = time.Parse(time.RFC3339, until)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if !start.IsZero() && !end.IsZero() && end.Before(start) {
		return time.Time{}, time.Time{}, errors.New("until must not be before from")
	}
	return start, end, nil
}

func observationQuery(from, until string, limit int) (ObservationQuery, error) {
	start, end, err := parseTimeRange(from, until)
	return ObservationQuery{From: start, Until: end, Limit: limit}, err
}
