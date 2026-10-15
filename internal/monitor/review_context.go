package monitor

import (
	"encoding/json"
	"time"
)

const maxReviewSearchContextRunes = 12000

// reviewContextBudget bounds historical evidence added to one model session.
// It compacts provider input only; the repository and server-side evidence
// validation continue to use the original observations.
type reviewContextBudget struct {
	remaining int
	exhausted bool
}

type compactReviewObservation struct {
	ID         string       `json:"id"`
	Source     SignalSource `json:"source"`
	Text       string       `json:"text"`
	Confidence float64      `json:"confidence"`
	ObservedAt time.Time    `json:"observed_at"`
}

func newReviewContextBudget() *reviewContextBudget {
	return &reviewContextBudget{remaining: maxReviewSearchContextRunes}
}

// compactObservationPage drops duplicate geometry and admits only complete
// records that fit the remaining per-investigation search context budget.
func (b *reviewContextBudget) compactObservationPage(events []SignalEvent) (modelEvents []compactReviewObservation, accepted []SignalEvent, truncated bool) {
	if b == nil || b.exhausted || b.remaining <= 0 {
		if b != nil {
			b.exhausted = true
		}
		return nil, nil, len(events) > 0
	}
	for _, event := range events {
		text := boundedReviewText(event.Text, 400)
		compact := compactReviewObservation{
			ID: event.ID, Source: event.Source, Text: text,
			Confidence: event.Confidence, ObservedAt: event.ObservedAt,
		}
		encoded, _ := json.Marshal(compact)
		cost := runeCount(string(encoded))
		if cost > b.remaining {
			b.exhausted = true
			return modelEvents, accepted, true
		}
		b.remaining -= cost
		modelEvents = append(modelEvents, compact)
		accepted = append(accepted, event)
	}
	return modelEvents, accepted, false
}
