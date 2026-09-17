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

type Repository interface {
	Create(context.Context, *Run) error
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

func (r *MemoryRepository) Create(_ context.Context, run *Run) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.runs[run.ID]; exists {
		return errors.New("monitor run already exists")
	}
	r.runs[run.ID] = cloneRun(run)
	return nil
}

func (r *MemoryRepository) Update(_ context.Context, id string, mutate func(*Run) error) (*Run, error) {
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
	next.UpdatedAt = time.Now().UTC()
	next.Version++
	r.runs[id] = next
	return cloneRun(next), nil
}

func (r *MemoryRepository) Get(_ context.Context, id string) (*Run, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.runs[id] == nil {
		return nil, ErrNotFound
	}
	return cloneRun(r.runs[id]), nil
}

func (r *MemoryRepository) List(_ context.Context) ([]*Run, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	runs := make([]*Run, 0, len(r.runs))
	for _, run := range r.runs {
		runs = append(runs, cloneRun(run))
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

func cloneRun(run *Run) *Run {
	data, _ := json.Marshal(run)
	var copy Run
	_ = json.Unmarshal(data, &copy)
	return &copy
}
