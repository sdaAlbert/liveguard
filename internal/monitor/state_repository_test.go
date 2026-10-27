package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
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

func TestSQLiteRepositoryContractAndConcurrentUpdates(t *testing.T) {
	repo, err := OpenSQLiteRepository(filepath.Join(t.TempDir(), "liveguard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	runRepositoryContract(t, repo)
}

func TestSQLiteRepositoryPersistsRunAndObservationAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveguard.sqlite")
	repo, err := OpenSQLiteRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: domain.NewID("persisted"), Status: RunRunning, Version: 1, StartedAt: time.Now().UTC(), Rooms: []Room{}, Events: []SignalEvent{}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	event := SignalEvent{ID: "observation-1", Source: SignalOCR, Text: "活动现在开始", ObservedAt: time.Now().UTC()}
	_, err = repo.Update(context.Background(), run.ID, func(current *Run) error {
		current.Investigations = append(current.Investigations, Investigation{ID: "investigation-1", Status: InvestigationConfirmed, Conclusion: "识别到主播宣布活动开始", Steps: []AgentStep{{Tool: "search_observations", Outcome: "找到一条匹配字幕"}}})
		current.Events = append(current.Events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Re-recording the same event through the ledger interface is idempotent.
	if err := repo.RecordObservation(context.Background(), run.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.Get(context.Background(), run.ID)
	if err != nil || len(got.Investigations) != 1 || got.Investigations[0].Conclusion != "识别到主播宣布活动开始" || len(got.Investigations[0].Steps) != 1 {
		t.Fatalf("investigation not restored: %#v %v", got, err)
	}
	page, err := reopened.SearchObservations(context.Background(), run.ID, ObservationQuery{Limit: 12})
	if err != nil || len(page.Events) != 1 || page.Events[0].Text != event.Text {
		t.Fatalf("observation not restored: %#v %v", page, err)
	}
}

func TestSQLiteObservationSearchPaginatesInStableNewestFirstOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveguard.sqlite")
	repo, err := OpenSQLiteRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "search-run", Status: RunRunning, Version: 1, StartedAt: time.Now().UTC(), Rooms: []Room{}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, event := range []SignalEvent{
		{ID: "a", Sequence: 1, RoomID: "room-a", Source: SignalOCR, Text: "活动预告", ObservedAt: observedAt},
		{ID: "b", Sequence: 2, RoomID: "room-a", Source: SignalOCR, Text: "活动开始", ObservedAt: observedAt},
		{ID: "c", Sequence: 3, RoomID: "room-b", Source: SignalASR, Text: "新品公告", ObservedAt: observedAt.Add(time.Second)},
	} {
		if err := repo.RecordObservation(context.Background(), run.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	query := ObservationQuery{TextAny: []string{"活动", "公告"}, Limit: 1}
	first, err := repo.SearchObservations(context.Background(), run.ID, query)
	if err != nil || first.TotalMatches != 3 || !first.HasMore || len(first.Events) != 1 || first.Events[0].ID != "c" {
		t.Fatalf("unexpected first page: %#v %v", first, err)
	}
	query.Before, query.BeforeSequence, query.BeforeEventID = first.NextBefore, first.NextBeforeSequence, first.NextBeforeEventID
	second, err := repo.SearchObservations(context.Background(), run.ID, query)
	if err != nil || second.TotalMatches != 3 || !second.HasMore || len(second.Events) != 1 || second.Events[0].ID != "b" {
		t.Fatalf("unexpected second page: %#v %v", second, err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	query.Before, query.BeforeSequence, query.BeforeEventID = time.Time{}, 0, ""
	page, err := reopened.SearchObservations(context.Background(), run.ID, ObservationQuery{RoomID: "room-a", Source: SignalOCR, Limit: 12})
	if err != nil || page.TotalMatches != 2 || len(page.Events) != 2 || page.Events[0].ID != "b" || page.Events[1].ID != "a" {
		t.Fatalf("room/source filter did not survive reopen: %#v %v", page, err)
	}
}

func TestSQLiteRepositoryCanRecoverInterruptedRunWithoutLosingObservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveguard.sqlite")
	repo, err := OpenSQLiteRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "interrupted-run", Status: RunRunning, Version: 1, StartedAt: time.Now().UTC(), Rooms: []Room{{ID: "room-1", Status: "listening", Health: SourceHealthy}}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordObservation(context.Background(), run.ID, SignalEvent{ID: "before-restart", RoomID: "room-1", Source: SignalOCR, Text: "重启前的证据", ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	service := NewService(reopened, nil)
	if err := service.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.Get(context.Background(), run.ID)
	if err != nil || recovered.Status != RunPaused || recovered.StopReason != "process_interrupted" {
		t.Fatalf("run was not restored as paused: %#v %v", recovered, err)
	}
	page, err := reopened.SearchObservations(context.Background(), run.ID, ObservationQuery{Limit: 12})
	if err != nil || len(page.Events) != 1 || page.Events[0].Text != "重启前的证据" {
		t.Fatalf("durable observation missing after recovery: %#v %v", page, err)
	}
}

func TestSQLitePendingQueueReclaimsInFlightSignalAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveguard.sqlite")
	repo, err := OpenSQLiteRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "pending-run", Status: RunRunning, Version: 1, GoalVersion: 1, StartedAt: time.Now().UTC(), Rooms: []Room{{ID: "room-1", Name: "直播间"}}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	item := pendingSignal{
		version: 1, enqueuedAt: time.Now().UTC(), expires: time.Now().Add(time.Minute).UTC(),
		event:      SignalEvent{ID: "pending-1", RoomID: "room-1", Source: SignalOCR, Text: "活动开始", GoalVersion: 1, ObservedAt: time.Now().UTC()},
		frameBytes: []byte{0x89, 0x50, 0x4e, 0x47},
	}
	if _, err := repo.UpdateAndEnqueuePending(context.Background(), run.ID, func(current *Run) (pendingSignal, bool, error) {
		current.EventSequence++
		item.event.Sequence = current.EventSequence
		current.Events = append(current.Events, item.event)
		return item, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := repo.ClaimPending(context.Background(), run.ID, 1, time.Now().UTC())
	if err != nil || !found || claimed.event.ID != item.event.ID || string(claimed.frameBytes) != string(item.frameBytes) {
		t.Fatalf("signal was not claimed with its screenshot: %#v found=%v err=%v", claimed, found, err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.RecoverPending(context.Background(), run.ID, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waiting, reviewing, err := reopened.PendingCounts(context.Background(), run.ID)
	if err != nil || waiting != 1 || reviewing {
		t.Fatalf("in-flight signal was not returned to the queue: waiting=%d reviewing=%v err=%v", waiting, reviewing, err)
	}
	retried, found, err := reopened.ClaimPending(context.Background(), run.ID, 1, time.Now().UTC())
	if err != nil || !found || retried.event.ID != item.event.ID || string(retried.frameBytes) != string(item.frameBytes) {
		t.Fatalf("recovered signal cannot be retried: %#v found=%v err=%v", retried, found, err)
	}
	if err := reopened.FinishPending(context.Background(), run.ID, item.event.ID, false); err != nil {
		t.Fatal(err)
	}
	waiting, reviewing, err = reopened.PendingCounts(context.Background(), run.ID)
	if err != nil || waiting != 0 || reviewing {
		t.Fatalf("finished signal remained pending: waiting=%d reviewing=%v err=%v", waiting, reviewing, err)
	}
}

func TestSQLiteQueueFullAdmissionRollsBackRunAndObservation(t *testing.T) {
	repo, err := OpenSQLiteRepository(filepath.Join(t.TempDir(), "liveguard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	run := &Run{ID: "bounded-queue", Status: RunRunning, Version: 1, GoalVersion: 1, StartedAt: time.Now().UTC(), Rooms: []Room{{ID: "room-1"}}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPendingSignals; i++ {
		event := SignalEvent{ID: fmt.Sprintf("event-%d", i), RoomID: "room-1", Source: SignalOCR, Text: fmt.Sprintf("消息 %c", '甲'+rune(i)), GoalVersion: 1, ObservedAt: time.Now().UTC()}
		item := pendingSignal{event: event, version: 1, enqueuedAt: time.Now().UTC(), expires: time.Now().Add(time.Minute)}
		if _, err := repo.UpdateAndEnqueuePending(context.Background(), run.ID, func(current *Run) (pendingSignal, bool, error) {
			current.EventSequence++
			event.Sequence = current.EventSequence
			current.Events = append(current.Events, event)
			item.event = event
			return item, true, nil
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	tooMany := SignalEvent{ID: "event-overflow", RoomID: "room-1", Source: SignalOCR, Text: "队列之外壹", GoalVersion: 1, ObservedAt: time.Now().UTC()}
	item := pendingSignal{event: tooMany, version: 1, enqueuedAt: time.Now().UTC(), expires: time.Now().Add(time.Minute)}
	if _, err := repo.UpdateAndEnqueuePending(context.Background(), run.ID, func(current *Run) (pendingSignal, bool, error) {
		current.EventSequence++
		tooMany.Sequence = current.EventSequence
		current.Events = append(current.Events, tooMany)
		item.event = tooMany
		return item, true, nil
	}); !errors.Is(err, ErrPendingFull) {
		t.Fatalf("expected queue rejection, got %v", err)
	}
	got, err := repo.Get(context.Background(), run.ID)
	if err != nil || got.EventSequence != maxPendingSignals || len(got.Events) != maxPendingSignals {
		t.Fatalf("rejected admission partially updated Run: seq=%d events=%d err=%v", got.EventSequence, len(got.Events), err)
	}
	page, err := repo.SearchObservations(context.Background(), run.ID, ObservationQuery{Limit: 12})
	if err != nil || page.TotalMatches != maxPendingSignals {
		t.Fatalf("rejected admission partially updated observations: matches=%d err=%v", page.TotalMatches, err)
	}
}

func TestSQLitePendingSignalsExpireAndAreRemovedWhenGoalChanges(t *testing.T) {
	repo, err := OpenSQLiteRepository(filepath.Join(t.TempDir(), "liveguard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	run := &Run{ID: "pending-version", Status: RunRunning, Version: 1, GoalVersion: 1, StartedAt: time.Now().UTC(), Rooms: []Room{{ID: "room-1"}}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	enqueue := func(id, text string, version uint64, expires time.Time) {
		t.Helper()
		event := SignalEvent{ID: id, RoomID: "room-1", Source: SignalOCR, Text: text, GoalVersion: version, ObservedAt: time.Now().UTC()}
		item := pendingSignal{event: event, version: version, enqueuedAt: time.Now().UTC(), expires: expires}
		if _, err := repo.UpdateAndEnqueuePending(context.Background(), run.ID, func(current *Run) (pendingSignal, bool, error) {
			current.EventSequence++
			event.Sequence = current.EventSequence
			current.Events = append(current.Events, event)
			item.event = event
			return item, true, nil
		}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	enqueue("old-goal", "旧目标信号", 1, time.Now().Add(time.Minute))
	if _, err := repo.Update(context.Background(), run.ID, func(current *Run) error {
		current.Goal = "新的关注目标"
		current.GoalVersion++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if waiting, reviewing, err := repo.PendingCounts(context.Background(), run.ID); err != nil || waiting != 0 || reviewing {
		t.Fatalf("goal change retained an obsolete signal: waiting=%d reviewing=%v err=%v", waiting, reviewing, err)
	}
	enqueue("expired", "已过期信号", 2, time.Now().Add(time.Minute))
	if err := repo.RecoverPending(context.Background(), run.ID, 2, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if waiting, reviewing, err := repo.PendingCounts(context.Background(), run.ID); err != nil || waiting != 0 || reviewing {
		t.Fatalf("recovery retained an expired signal: waiting=%d reviewing=%v err=%v", waiting, reviewing, err)
	}
}

func TestSQLiteExportJSONContainsRunAndObservationLedger(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLiteRepository(filepath.Join(dir, "liveguard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "exported-run", Status: RunRunning, Version: 1, GoalVersion: 1, StartedAt: time.Now().UTC(), Rooms: []Room{{ID: "room-1"}}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	event := SignalEvent{ID: "exported-event", RoomID: "room-1", Source: SignalOCR, Text: "直播商品讲解", ObservedAt: time.Now().UTC()}
	if err := repo.RecordObservation(context.Background(), run.ID, event); err != nil {
		t.Fatal(err)
	}
	exportDir := filepath.Join(dir, "json-backup")
	if err := repo.ExportJSON(context.Background(), exportDir); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	snapshotData, err := os.ReadFile(filepath.Join(exportDir, "liveguard.json"))
	if err != nil {
		t.Fatalf("read exported snapshot: %v", err)
	}
	var snapshot struct {
		Version int    `json:"version"`
		Runs    []*Run `json:"runs"`
	}
	if err := json.Unmarshal(snapshotData, &snapshot); err != nil {
		t.Fatalf("decode exported snapshot: %v", err)
	}
	if snapshot.Version != 1 || len(snapshot.Runs) != 1 || snapshot.Runs[0].ID != run.ID {
		t.Fatalf("exported Run snapshot mismatch: %+v", snapshot)
	}
	ledger, err := os.Open(filepath.Join(exportDir, "liveguard.json.observations.jsonl"))
	if err != nil {
		t.Fatalf("open exported observation ledger: %v", err)
	}
	defer ledger.Close()
	scanner := bufio.NewScanner(ledger)
	var record struct {
		RunID string      `json:"run_id"`
		Event SignalEvent `json:"event"`
	}
	if !scanner.Scan() {
		t.Fatalf("expected an exported observation, scanner error: %v", scanner.Err())
	}
	if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
		t.Fatalf("decode exported observation: %v", err)
	}
	if record.RunID != run.ID || record.Event.ID != event.ID || scanner.Scan() {
		t.Fatalf("exported observation mismatch: %+v", record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read exported observation ledger: %v", err)
	}
	if err := repo.ExportJSON(context.Background(), exportDir); err == nil {
		t.Fatal("expected existing export destination to be preserved")
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
