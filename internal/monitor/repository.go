package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("monitor run not found")
var ErrCapacity = errors.New("monitor capacity reached")
var ErrInvalidTransition = errors.New("invalid monitor lifecycle transition")

type Repository interface {
	Create(context.Context, *Run) error
	// Update atomically loads, mutates and commits one monitor. The callback is
	// restricted to bounded in-memory state changes; it must not call providers,
	// browsers, or perform other network I/O.
	Update(context.Context, string, func(*Run) error) (*Run, error)
	Get(context.Context, string) (*Run, error)
	List(context.Context) ([]*Run, error)
	ListRunning(context.Context) ([]*Run, error)
}

type MemoryRepository struct {
	mu           sync.RWMutex
	runs         map[string]*Run
	observations map[string]map[string]SignalEvent
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{runs: map[string]*Run{}, observations: map[string]map[string]SignalEvent{}}
}

func (r *MemoryRepository) Create(ctx context.Context, run *Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if run == nil || run.ID == "" || run.Status != RunRunning {
		return errors.New("new monitor must have an id and start running")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.runs[run.ID]; exists {
		return errors.New("monitor run already exists")
	}
	r.syncLedger(run.ID, nil, run)
	r.runs[run.ID] = cloneRun(run)
	return nil
}

func (r *MemoryRepository) Update(ctx context.Context, id string, mutate func(*Run) error) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mutate == nil {
		return nil, errors.New("monitor update callback is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.runs[id]
	if current == nil {
		return nil, ErrNotFound
	}
	next := cloneRun(current)
	if err := mutate(next); err != nil {
		return nil, err
	}
	boundRunHistory(next)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	desiredStatus := next.Status
	next.Status = current.Status
	if err := TransitionRun(next, desiredStatus); err != nil {
		return nil, err
	}
	if err := validateInvestigationUpdates(current.Investigations, next.Investigations); err != nil {
		return nil, err
	}
	r.syncLedger(id, current, next)
	next.UpdatedAt = time.Now().UTC()
	next.Version++
	r.runs[id] = next
	return cloneRun(next), nil
}

func (r *MemoryRepository) syncLedger(runID string, previous, next *Run) {
	if r.observations[runID] == nil {
		r.observations[runID] = map[string]SignalEvent{}
	}
	oldEvents := map[string]bool{}
	oldInvestigations := map[string]bool{}
	if previous != nil {
		for _, event := range previous.Events {
			oldEvents[event.ID] = true
		}
		for _, investigation := range previous.Investigations {
			oldInvestigations[investigation.ID] = true
		}
	}
	for _, event := range next.Events {
		if event.ID != "" && !oldEvents[event.ID] {
			if _, exists := r.observations[runID][event.ID]; exists {
				continue
			}
			r.observations[runID][event.ID] = event
		}
	}
	for _, investigation := range next.Investigations {
		if investigation.ID != "" && (!oldInvestigations[investigation.ID] || previousInvestigationChanged(previous, investigation)) {
			for _, observation := range visualObservations(investigation) {
				if _, exists := r.observations[runID][observation.ID]; !exists {
					r.observations[runID][observation.ID] = observation
				}
			}
		}
	}
}

func previousInvestigationChanged(previous *Run, next Investigation) bool {
	if previous == nil {
		return true
	}
	for _, before := range previous.Investigations {
		if before.ID == next.ID {
			return before.Status != next.Status || before.UpdatedAt != next.UpdatedAt
		}
	}
	return true
}

func (r *MemoryRepository) RecordObservation(ctx context.Context, runID string, event SignalEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.ID == "" {
		return errors.New("observation id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs[runID] == nil {
		return ErrNotFound
	}
	if r.observations[runID] == nil {
		r.observations[runID] = map[string]SignalEvent{}
	}
	if _, exists := r.observations[runID][event.ID]; !exists {
		r.observations[runID][event.ID] = event
	}
	return nil
}

func (r *MemoryRepository) SearchObservations(ctx context.Context, runID string, query ObservationQuery) (ObservationPage, error) {
	if err := ctx.Err(); err != nil {
		return ObservationPage{}, err
	}
	query = normalizeObservationQuery(query)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.runs[runID] == nil {
		return ObservationPage{}, ErrNotFound
	}
	results := make([]SignalEvent, 0, query.Limit)
	var totalMatches int64
	for _, event := range r.observations[runID] {
		if query.EventID != "" && event.ID != query.EventID {
			continue
		}
		if query.RoomID != "" && event.RoomID != query.RoomID || query.Source != "" && event.Source != query.Source {
			continue
		}
		if !query.From.IsZero() && event.ObservedAt.Before(query.From) || !query.Until.IsZero() && event.ObservedAt.After(query.Until) {
			continue
		}
		if !observationMatchesText(event, query.TextAny) {
			continue
		}
		totalMatches++
		if !observationBeforeCursor(event, query) {
			continue
		}
		results = append(results, event)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].ObservedAt.Equal(results[j].ObservedAt) {
			if results[i].Sequence == results[j].Sequence {
				return results[i].ID > results[j].ID
			}
			return results[i].Sequence > results[j].Sequence
		}
		return results[i].ObservedAt.After(results[j].ObservedAt)
	})
	page := ObservationPage{Events: results, TotalMatches: totalMatches}
	if len(results) > query.Limit {
		page.HasMore = true
		results = results[:query.Limit]
	}
	page.Events = results
	if page.HasMore && len(page.Events) > 0 {
		last := page.Events[len(page.Events)-1]
		page.NextBefore = last.ObservedAt
		page.NextBeforeSequence = last.Sequence
		page.NextBeforeEventID = last.ID
	}
	return page, nil
}

func observationBeforeCursor(event SignalEvent, query ObservationQuery) bool {
	if query.Before.IsZero() {
		return true
	}
	if event.ObservedAt.Before(query.Before) {
		return true
	}
	if !event.ObservedAt.Equal(query.Before) {
		return false
	}
	if event.Sequence < query.BeforeSequence {
		return true
	}
	return event.Sequence == query.BeforeSequence && event.ID < query.BeforeEventID
}

func (r *MemoryRepository) Get(ctx context.Context, id string) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.runs[id] == nil {
		return nil, ErrNotFound
	}
	return cloneRun(r.runs[id]), nil
}

func (r *MemoryRepository) List(ctx context.Context) ([]*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	runs := make([]*Run, 0, len(r.runs))
	for _, run := range r.runs {
		runs = append(runs, cloneRun(run))
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

func (r *MemoryRepository) ListRunning(ctx context.Context) ([]*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var runs []*Run
	for _, run := range r.runs {
		if run.Status == RunRunning {
			runs = append(runs, cloneRun(run))
		}
	}
	return runs, nil
}

func validateRunTransition(from, to RunStatus) error {
	if from == to {
		return nil
	}
	if from == RunRunning && (to == RunPaused || to == RunStopped || to == RunFailed || to == RunCompleted) {
		return nil
	}
	if from == RunPaused && (to == RunRunning || to == RunStopped || to == RunFailed) {
		return nil
	}
	return ErrInvalidTransition
}

func validateInvestigationUpdates(previous, next []Investigation) error {
	old := make(map[string]Investigation, len(previous))
	for _, item := range previous {
		old[item.ID] = item
	}
	for _, item := range next {
		if !validInvestigationStatus(item.Status) {
			return ErrInvalidInvestigationTransition
		}
		if before, exists := old[item.ID]; exists && before.Status != item.Status {
			probe := before
			if err := TransitionInvestigation(&probe, item.Status); err != nil {
				return err
			}
		}
	}
	return nil
}

func validInvestigationStatus(status string) bool {
	switch status {
	case InvestigationWaiting, InvestigationWaitingUser, InvestigationReviewing, InvestigationConfirmed, InvestigationSuppressed, InvestigationNeedsHuman, InvestigationFailed:
		return true
	default:
		return false
	}
}

func cloneRun(run *Run) *Run {
	data, _ := json.Marshal(run)
	var copy Run
	_ = json.Unmarshal(data, &copy)
	return &copy
}
