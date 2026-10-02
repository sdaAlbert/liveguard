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
}

type MemoryRepository struct {
	mu   sync.RWMutex
	runs map[string]*Run
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{runs: map[string]*Run{}}
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
	next.UpdatedAt = time.Now().UTC()
	next.Version++
	r.runs[id] = next
	return cloneRun(next), nil
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

func validateRunTransition(from, to RunStatus) error {
	if from == to {
		return nil
	}
	if from == RunRunning && (to == RunStopped || to == RunFailed || to == RunCompleted) {
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
	case InvestigationWaiting, InvestigationConfirmed, InvestigationSuppressed, InvestigationNeedsHuman, InvestigationFailed:
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
