package monitor

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"liveguard/internal/domain"
)

func TestLifecycleTransitionTable(t *testing.T) {
	valid := [][2]RunStatus{{RunRunning, RunPaused}, {RunRunning, RunStopped}, {RunRunning, RunFailed}, {RunRunning, RunCompleted}, {RunPaused, RunRunning}, {RunPaused, RunStopped}, {RunStopped, RunStopped}}
	for _, pair := range valid {
		if err := validateRunTransition(pair[0], pair[1]); err != nil {
			t.Errorf("%s -> %s: %v", pair[0], pair[1], err)
		}
	}
	invalid := [][2]RunStatus{{RunStopped, RunRunning}, {RunFailed, RunRunning}, {RunCompleted, RunStopped}}
	for _, pair := range invalid {
		if err := validateRunTransition(pair[0], pair[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s should be rejected, got %v", pair[0], pair[1], err)
		}
	}
	waiting := Investigation{Status: InvestigationWaiting}
	if err := TransitionInvestigation(&waiting, InvestigationConfirmed); err != nil {
		t.Fatal(err)
	}
	if err := TransitionInvestigation(&waiting, InvestigationFailed); !errors.Is(err, ErrInvalidInvestigationTransition) {
		t.Fatalf("terminal investigation changed: %v", err)
	}
}

func TestSignalIdempotencyValidationAndArrivalSequence(t *testing.T) {
	repo := NewMemoryRepository()
	s := NewService(repo, nil)
	calls := &testUnexpectedReviewer{decision: UnexpectedEventDecision{Action: "ignore", Category: "other", Confidence: .9}}
	s.SetUnexpectedEventReviewer(calls, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.example.com/test", Goal: "重要消息提醒", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	event := SignalEvent{ID: "evt-1", RoomID: run.Rooms[0].ID, Source: SignalOCR, Text: "直播间正式公告", Confidence: .8, ObservedAt: time.Now().UTC()}
	if err = s.Ingest(context.Background(), run.ID, event); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(context.Background(), run.ID, event); err != nil {
		t.Fatalf("same event retry should be idempotent: %v", err)
	}
	conflict := event
	conflict.Text = "other content"
	if err = s.Ingest(context.Background(), run.ID, conflict); !errors.Is(err, ErrEventIDConflict) {
		t.Fatalf("same id/different data should conflict: %v", err)
	}
	current, _ := repo.Get(context.Background(), run.ID)
	if len(current.Events) != 1 || len(current.Alerts) != 0 || calls.calls.Load() != 1 || current.Events[0].Sequence != 1 || current.Events[0].ReceivedAt.IsZero() {
		t.Fatalf("duplicate changed business state: %#v calls=%d", current, calls.calls.Load())
	}
	if current.Rooms[0].Health != SourceHealthy {
		t.Fatalf("valid signal not reflected in health: %#v", current.Rooms[0])
	}
	invalid := []SignalEvent{{ID: "x", RoomID: run.Rooms[0].ID, Source: "camera", Text: "hi"}, {ID: "y", RoomID: run.Rooms[0].ID, Source: SignalOCR, Text: "hi", Confidence: 1.1}, {ID: "nan", RoomID: run.Rooms[0].ID, Source: SignalOCR, Text: "hi", Confidence: math.NaN()}, {ID: "z", RoomID: "elsewhere", Source: SignalASR, Text: "hi"}, {ID: "t", RoomID: run.Rooms[0].ID, Source: SignalOCR, Text: "future", ObservedAt: time.Now().Add(maxFutureSignalSkew + time.Minute)}}
	for _, item := range invalid {
		if err := s.Ingest(context.Background(), run.ID, item); err == nil {
			t.Errorf("accepted invalid signal %#v", item)
		}
	}
}

func TestMemoryRepositoryContractAndConcurrentUpdates(t *testing.T) {
	runRepositoryContract(t, NewMemoryRepository())
}

func TestFileRepositoryPersistsRunInvestigationAndObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveguard.json")
	repo, err := OpenFileRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: domain.NewID("persisted"), Status: RunRunning, Version: 1, StartedAt: time.Now().UTC(), Rooms: []Room{}, Events: []SignalEvent{}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Update(context.Background(), run.ID, func(current *Run) error {
		current.Investigations = append(current.Investigations, Investigation{ID: "investigation-1", Status: InvestigationConfirmed, Conclusion: "识别到主播宣布活动开始", Steps: []AgentStep{{Tool: "search_observations", Outcome: "找到一条匹配字幕"}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordObservation(context.Background(), run.ID, SignalEvent{ID: "observation-1", Source: SignalOCR, Text: "活动现在开始", ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(context.Background(), run.ID)
	if err != nil || len(got.Investigations) != 1 || got.Investigations[0].Conclusion != "识别到主播宣布活动开始" || len(got.Investigations[0].Steps) != 1 {
		t.Fatalf("investigation not restored: %#v %v", got, err)
	}
	page, err := reopened.SearchObservations(context.Background(), run.ID, ObservationQuery{Limit: 12})
	if err != nil || len(page.Events) != 1 || page.Events[0].Text != "活动现在开始" {
		t.Fatalf("observation not restored: %#v %v", page, err)
	}
}

func TestRecoverInterruptedStopsPersistedMonitors(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	run := &Run{ID: "persisted", Status: RunRunning, StartedAt: now, Rooms: []Room{{ID: "room", Status: "listening", Health: SourceHealthy}}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := NewService(repo, nil).RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := repo.Get(context.Background(), run.ID)
	if err != nil || recovered.Status != RunPaused || recovered.CompletedAt != nil || recovered.Rooms[0].Status != "paused" || recovered.StopReason != "process_interrupted" {
		t.Fatalf("persisted active monitor was not paused for manual continuation: %#v %v", recovered, err)
	}
}

func runRepositoryContract(t *testing.T, repo Repository) {
	t.Helper()
	id := domain.NewID("contract")
	run := &Run{ID: id, Status: RunRunning, Version: 1, StartedAt: time.Now().UTC(), Rooms: []Room{}, Events: []SignalEvent{}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	// Returned snapshots and the caller's original value never alias stored state.
	run.Status = RunStopped
	got, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunRunning {
		t.Fatal("Create retained caller-owned pointer")
	}
	got.Events = append(got.Events, SignalEvent{Text: "outside mutation"})
	got, err = repo.Get(context.Background(), id)
	if err != nil || len(got.Events) != 0 {
		t.Fatalf("Get returned mutable alias: %#v %v", got, err)
	}
	_, err = repo.Update(context.Background(), id, func(r *Run) error { r.Goal = "partial"; return errors.New("abort") })
	if err == nil {
		t.Fatal("callback error committed")
	}
	got, _ = repo.Get(context.Background(), id)
	if got.Goal != "" {
		t.Fatal("failed update partially committed")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = repo.Update(canceled, id, func(r *Run) error { r.Goal = "cancelled"; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Update ignored cancellation: %v", err)
	}
	const workers = 24
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := repo.Update(context.Background(), id, func(r *Run) error { r.EventSequence++; return nil }); e != nil {
				t.Errorf("concurrent Update: %v", e)
			}
		}()
	}
	wg.Wait()
	got, err = repo.Get(context.Background(), id)
	if err != nil || got.EventSequence != workers || got.Version != 1+workers {
		t.Fatalf("lost concurrent update: version=%d sequence=%d err=%v", got.Version, got.EventSequence, err)
	}
	if _, err = repo.Update(context.Background(), id, func(r *Run) error { r.Status = RunStopped; return nil }); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Update(context.Background(), id, func(r *Run) error { r.Status = RunRunning; return nil })
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("invalid status transition accepted: %v", err)
	}
}
