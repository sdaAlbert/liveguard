package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileRepository keeps bounded monitor state in an atomic JSON snapshot and
// appends the complete observation ledger to a JSONL sidecar. Screenshot bytes
// remain in the separate frame archive.
type FileRepository struct {
	mu                 sync.Mutex
	path               string
	observationsPath   string
	loggedObservations map[string]map[string]bool
	memory             *MemoryRepository
}

type fileRepositorySnapshot struct {
	Version      int                      `json:"version"`
	Runs         []*Run                   `json:"runs"`
	Observations map[string][]SignalEvent `json:"observations"`
}

func OpenFileRepository(path string) (*FileRepository, error) {
	if path == "" {
		return nil, errors.New("local repository path is required")
	}
	repo := &FileRepository{
		path:               path,
		observationsPath:   path + ".observations.jsonl",
		loggedObservations: map[string]map[string]bool{},
		memory:             NewMemoryRepository(),
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := repo.loadObservationLog(); err != nil {
			return nil, err
		}
		return repo, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot fileRepositorySnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	for _, run := range snapshot.Runs {
		if run == nil || run.ID == "" {
			continue
		}
		repo.memory.runs[run.ID] = cloneRun(run)
	}
	for runID, events := range snapshot.Observations {
		repo.memory.observations[runID] = make(map[string]SignalEvent, len(events))
		for _, event := range events {
			if event.ID != "" {
				repo.memory.observations[runID][event.ID] = event
			}
		}
	}
	if err := repo.loadObservationLog(); err != nil {
		return nil, err
	}
	// Migrate observations from the original all-in-one snapshot once. New
	// observations are appended to the sidecar and never rewrite this history.
	for runID := range repo.memory.runs {
		if err := repo.appendObservationsLocked(runID, repo.observationsForRun(runID)); err != nil {
			return nil, err
		}
	}
	return repo, nil
}

func (r *FileRepository) Create(ctx context.Context, run *Run) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.memory.Create(ctx, run); err != nil {
		return err
	}
	if err := r.persistLocked(); err != nil {
		return err
	}
	return r.appendObservationsLocked(run.ID, observationsAdded(nil, run))
}

func (r *FileRepository) Update(ctx context.Context, id string, mutate func(*Run) error) (*Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous, err := r.memory.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	run, err := r.memory.Update(ctx, id, mutate)
	if err != nil {
		return nil, err
	}
	if err := r.appendObservationsLocked(id, observationsAdded(previous, run)); err != nil {
		return nil, err
	}
	if err := r.persistLocked(); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *FileRepository) Get(ctx context.Context, id string) (*Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memory.Get(ctx, id)
}

func (r *FileRepository) List(ctx context.Context) ([]*Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memory.List(ctx)
}

func (r *FileRepository) ListRunning(ctx context.Context) ([]*Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memory.ListRunning(ctx)
}

func (r *FileRepository) RecordObservation(ctx context.Context, runID string, event SignalEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.memory.RecordObservation(ctx, runID, event); err != nil {
		return err
	}
	return r.appendObservationsLocked(runID, []SignalEvent{event})
}

func (r *FileRepository) SearchObservations(ctx context.Context, runID string, query ObservationQuery) (ObservationPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memory.SearchObservations(ctx, runID, query)
}

func (r *FileRepository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.persistLocked()
}

func (r *FileRepository) persistLocked() error {
	r.memory.mu.RLock()
	snapshot := fileRepositorySnapshot{Version: 1, Runs: make([]*Run, 0, len(r.memory.runs))}
	for _, run := range r.memory.runs {
		snapshot.Runs = append(snapshot.Runs, cloneRun(run))
	}
	r.memory.mu.RUnlock()
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(r.path), ".liveguard-state-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, r.path)
}

func (r *FileRepository) loadObservationLog() error {
	file, err := os.Open(r.observationsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		var record struct {
			RunID string      `json:"run_id"`
			Event SignalEvent `json:"event"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("read local observation log: %w", err)
		}
		if record.RunID == "" || record.Event.ID == "" {
			continue
		}
		if _, err := r.memory.Get(context.Background(), record.RunID); errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if err := r.memory.RecordObservation(context.Background(), record.RunID, record.Event); err != nil {
			return err
		}
		r.markObservationLogged(record.RunID, record.Event.ID)
	}
	return scanner.Err()
}

func (r *FileRepository) observationsForRun(runID string) []SignalEvent {
	r.memory.mu.RLock()
	defer r.memory.mu.RUnlock()
	events := r.memory.observations[runID]
	result := make([]SignalEvent, 0, len(events))
	for _, event := range events {
		result = append(result, event)
	}
	return result
}

func observationsAdded(previous, next *Run) []SignalEvent {
	if next == nil {
		return nil
	}
	knownEvents := map[string]bool{}
	knownInvestigations := map[string]Investigation{}
	if previous != nil {
		for _, event := range previous.Events {
			knownEvents[event.ID] = true
		}
		for _, investigation := range previous.Investigations {
			knownInvestigations[investigation.ID] = investigation
		}
	}
	var result []SignalEvent
	for _, event := range next.Events {
		if event.ID != "" && !knownEvents[event.ID] {
			result = append(result, event)
		}
	}
	for _, investigation := range next.Investigations {
		previousInvestigation, existed := knownInvestigations[investigation.ID]
		if !existed || previousInvestigation.Status != investigation.Status || previousInvestigation.UpdatedAt != investigation.UpdatedAt {
			result = append(result, visualObservations(investigation)...)
		}
	}
	return result
}

func (r *FileRepository) appendObservationsLocked(runID string, events []SignalEvent) error {
	if len(events) == 0 {
		return nil
	}
	var output []byte
	var appended []string
	for _, event := range events {
		if event.ID == "" || r.isObservationLogged(runID, event.ID) {
			continue
		}
		payload, err := json.Marshal(struct {
			RunID string      `json:"run_id"`
			Event SignalEvent `json:"event"`
		}{RunID: runID, Event: event})
		if err != nil {
			return err
		}
		output = append(output, payload...)
		output = append(output, '\n')
		appended = append(appended, event.ID)
	}
	if len(output) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.observationsPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(r.observationsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(output)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, eventID := range appended {
		r.markObservationLogged(runID, eventID)
	}
	return nil
}

func (r *FileRepository) isObservationLogged(runID, eventID string) bool {
	return r.loggedObservations[runID] != nil && r.loggedObservations[runID][eventID]
}

func (r *FileRepository) markObservationLogged(runID, eventID string) {
	if r.loggedObservations[runID] == nil {
		r.loggedObservations[runID] = map[string]bool{}
	}
	r.loggedObservations[runID][eventID] = true
}
