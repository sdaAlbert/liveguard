package monitor

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteRepository stores bounded Run snapshots and the complete observation
// ledger in one transactional local database. SQLite remains the source of
// truth; repository operations do not keep a second in-memory copy of the Runs.
type SQLiteRepository struct {
	mu sync.Mutex
	db *sql.DB
}

type jsonExportSnapshot struct {
	Version int    `json:"version"`
	Runs    []*Run `json:"runs"`
}

func OpenSQLiteRepository(path string) (*SQLiteRepository, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("SQLite repository path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create SQLite repository directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite repository: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	repo := &SQLiteRepository{db: db}
	if err := repo.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

func (r *SQLiteRepository) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS runs (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			version INTEGER NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS observations (
			run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			event_id TEXT NOT NULL,
			observed_at_ns INTEGER NOT NULL,
			sequence INTEGER NOT NULL,
			room_id TEXT NOT NULL,
			source TEXT NOT NULL,
			text TEXT NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY (run_id, event_id)
		)`,
		`CREATE INDEX IF NOT EXISTS observations_by_run_time ON observations(run_id, observed_at_ns DESC, sequence DESC, event_id DESC)`,
		`CREATE INDEX IF NOT EXISTS observations_by_run_room_time ON observations(run_id, room_id, observed_at_ns DESC)`,
		`CREATE TABLE IF NOT EXISTS pending_signals (
			run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			event_id TEXT NOT NULL,
			goal_version INTEGER NOT NULL,
			enqueued_at_ns INTEGER NOT NULL,
			expires_at_ns INTEGER NOT NULL,
			priority INTEGER NOT NULL,
			live INTEGER NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('pending','processing')),
			frame BLOB,
			payload BLOB NOT NULL,
			PRIMARY KEY (run_id, event_id)
		)`,
		`CREATE INDEX IF NOT EXISTS pending_signals_by_run ON pending_signals(run_id, status, goal_version, priority DESC, enqueued_at_ns)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS pending_one_processing_per_run ON pending_signals(run_id) WHERE status='processing'`,
	}
	for _, statement := range statements {
		if _, err := r.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize SQLite repository: %w", err)
		}
	}
	return nil
}

func (r *SQLiteRepository) Create(ctx context.Context, run *Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if run == nil || run.ID == "" || run.Status != RunRunning {
		return errors.New("new monitor must have an id and start running")
	}
	candidate := cloneRun(run)
	r.mu.Lock()
	defer r.mu.Unlock()
	payload, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,status,version,payload) VALUES(?,?,?,?)`, candidate.ID, candidate.Status, candidate.Version, payload); err != nil {
		return fmt.Errorf("create SQLite run: %w", err)
	}
	initialObservations := append([]SignalEvent(nil), candidate.Events...)
	for _, investigation := range candidate.Investigations {
		initialObservations = append(initialObservations, visualObservations(investigation)...)
	}
	if err := insertSQLiteObservations(ctx, tx, candidate.ID, initialObservations); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite run: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) Update(ctx context.Context, id string, mutate func(*Run) error) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mutate == nil {
		return nil, errors.New("monitor update callback is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var previousPayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM runs WHERE id=?`, id).Scan(&previousPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var previous Run
	if err := json.Unmarshal(previousPayload, &previous); err != nil {
		return nil, fmt.Errorf("decode current SQLite run: %w", err)
	}
	next := cloneRun(&previous)
	if err := mutate(next); err != nil {
		return nil, err
	}
	boundRunHistory(next)
	desiredStatus := next.Status
	next.Status = previous.Status
	if err := TransitionRun(next, desiredStatus); err != nil {
		return nil, err
	}
	if err := validateInvestigationUpdates(previous.Investigations, next.Investigations); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	next.UpdatedAt = time.Now().UTC()
	next.Version++
	payload, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,version=?,payload=? WHERE id=? AND version=?`, next.Status, next.Version, payload, id, previous.Version)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, errors.New("SQLite run changed concurrently")
	}
	if err := insertSQLiteObservations(ctx, tx, id, observationsAdded(&previous, next)); err != nil {
		return nil, err
	}
	if err := reconcilePendingAfterRunUpdate(ctx, tx, id, previous.GoalVersion, next.GoalVersion, next.Status); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit SQLite run update: %w", err)
	}
	return cloneRun(next), nil
}

func (r *SQLiteRepository) Get(ctx context.Context, id string) (*Run, error) {
	var payload []byte
	if err := r.db.QueryRowContext(ctx, `SELECT payload FROM runs WHERE id=?`, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(payload, &run); err != nil {
		return nil, fmt.Errorf("decode SQLite run: %w", err)
	}
	return cloneRun(&run), nil
}

func (r *SQLiteRepository) List(ctx context.Context) ([]*Run, error) {
	return r.list(ctx, "SELECT payload FROM runs")
}

func (r *SQLiteRepository) ListRunning(ctx context.Context) ([]*Run, error) {
	return r.list(ctx, `SELECT payload FROM runs WHERE status='running'`)
}

func (r *SQLiteRepository) list(ctx context.Context, query string) ([]*Run, error) {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var run Run
		if err := json.Unmarshal(payload, &run); err != nil {
			return nil, fmt.Errorf("decode SQLite run: %w", err)
		}
		runs = append(runs, &run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

func (r *SQLiteRepository) RecordObservation(ctx context.Context, runID string, event SignalEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.ID == "" {
		return errors.New("observation id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertSQLiteObservations(ctx, tx, runID, []SignalEvent{event}); err != nil {
		return err
	}
	return tx.Commit()
}

func insertSQLiteObservations(ctx context.Context, tx *sql.Tx, runID string, events []SignalEvent) error {
	for _, event := range events {
		if event.ID == "" {
			continue
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO observations(run_id,event_id,observed_at_ns,sequence,room_id,source,text,payload) VALUES(?,?,?,?,?,?,?,?)`,
			runID, event.ID, event.ObservedAt.UnixNano(), event.Sequence, event.RoomID, event.Source, event.Text, payload); err != nil {
			return fmt.Errorf("store SQLite observation: %w", err)
		}
	}
	return nil
}

func (r *SQLiteRepository) UpdateAndEnqueuePending(ctx context.Context, id string, apply func(*Run) (pendingSignal, bool, error)) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if apply == nil {
		return nil, errors.New("pending signal admission callback is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var previousPayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM runs WHERE id=?`, id).Scan(&previousPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var previous Run
	if err := json.Unmarshal(previousPayload, &previous); err != nil {
		return nil, fmt.Errorf("decode current SQLite run: %w", err)
	}
	next := cloneRun(&previous)
	item, enqueue, err := apply(next)
	if err != nil {
		return nil, err
	}
	boundRunHistory(next)
	desiredStatus := next.Status
	next.Status = previous.Status
	if err := TransitionRun(next, desiredStatus); err != nil {
		return nil, err
	}
	if err := validateInvestigationUpdates(previous.Investigations, next.Investigations); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	next.UpdatedAt = time.Now().UTC()
	next.Version++
	if enqueue {
		if item.version != next.GoalVersion || item.event.GoalVersion != next.GoalVersion {
			return nil, ErrGoalChanged
		}
		if err := admitSQLitePending(ctx, tx, id, item, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,version=?,payload=? WHERE id=? AND version=?`, next.Status, next.Version, payload, id, previous.Version)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, errors.New("SQLite run changed concurrently")
	}
	if err := insertSQLiteObservations(ctx, tx, id, observationsAdded(&previous, next)); err != nil {
		return nil, err
	}
	if err := reconcilePendingAfterRunUpdate(ctx, tx, id, previous.GoalVersion, next.GoalVersion, next.Status); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit SQLite signal and pending admission: %w", err)
	}
	return cloneRun(next), nil
}

func admitSQLitePending(ctx context.Context, tx *sql.Tx, runID string, item pendingSignal, now time.Time) error {
	item.priority = pendingPriority(item.event)
	if !now.Before(item.expires) {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_id,goal_version,enqueued_at_ns,expires_at_ns,priority,live,payload,frame FROM pending_signals WHERE run_id=? AND status='pending' ORDER BY enqueued_at_ns,event_id`, runID)
	if err != nil {
		return err
	}
	var queued []pendingSignal
	for rows.Next() {
		var eventID string
		var version uint64
		var enqueuedNS, expiresNS int64
		var priority int
		var live int
		var payload, frame []byte
		if err := rows.Scan(&eventID, &version, &enqueuedNS, &expiresNS, &priority, &live, &payload, &frame); err != nil {
			_ = rows.Close()
			return err
		}
		var event SignalEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode queued SQLite signal: %w", err)
		}
		queued = append(queued, pendingSignal{event: event, version: version, enqueuedAt: time.Unix(0, enqueuedNS).UTC(), expires: time.Unix(0, expiresNS).UTC(), priority: priority, live: live != 0, frameBytes: frame})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	kept := queued[:0]
	for _, old := range queued {
		if !now.Before(old.expires) {
			if err := deleteSQLitePending(ctx, tx, runID, old.event.ID); err != nil {
				return err
			}
			continue
		}
		coalesce := old.event.Source == SignalFrame || samePendingObservation(old.event, item.event) || numericRevision(old.event, item.event) || (item.live && old.live && old.priority == 0 && item.priority == 0 && old.event.RoomID == item.event.RoomID && old.event.Source == SignalOCR && item.event.Source == SignalOCR)
		if coalesce {
			if !item.live && numericRevision(old.event, item.event) {
				item.priority = 2
			}
			if err := deleteSQLitePending(ctx, tx, runID, old.event.ID); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, old)
	}
	if len(kept) >= maxPendingSignals && item.priority > 0 {
		worst := -1
		for i, old := range kept {
			score := old.priority + int(max(0, now.Sub(old.enqueuedAt))/(30*time.Second))
			if score < item.priority && (worst < 0 || old.priority < kept[worst].priority) {
				worst = i
			}
		}
		if worst >= 0 {
			if err := deleteSQLitePending(ctx, tx, runID, kept[worst].event.ID); err != nil {
				return err
			}
			kept = append(kept[:worst], kept[worst+1:]...)
		}
	}
	if len(kept) >= maxPendingSignals {
		return ErrPendingFull
	}
	var retainedBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(frame)),0) FROM pending_signals`).Scan(&retainedBytes); err != nil {
		return err
	}
	if retainedBytes+int64(len(item.frameBytes)) > maxRetainedFrameBytes {
		return ErrFrameBudget
	}
	if item.enqueuedAt.IsZero() {
		item.enqueuedAt = now
	}
	item.event.FramePath = ""
	payload, err := json.Marshal(item.event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pending_signals(run_id,event_id,goal_version,enqueued_at_ns,expires_at_ns,priority,live,status,frame,payload) VALUES(?,?,?,?,?,?,?,'pending',?,?)`,
		runID, item.event.ID, item.version, item.enqueuedAt.UnixNano(), item.expires.UnixNano(), item.priority, boolInt(item.live), nullableBytes(item.frameBytes), payload)
	if err != nil {
		return fmt.Errorf("persist pending signal: %w", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func deleteSQLitePending(ctx context.Context, tx *sql.Tx, runID, eventID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM pending_signals WHERE run_id=? AND event_id=?`, runID, eventID)
	return err
}

func reconcilePendingAfterRunUpdate(ctx context.Context, tx *sql.Tx, runID string, previousVersion, nextVersion uint64, status RunStatus) error {
	if previousVersion != nextVersion {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_signals WHERE run_id=? AND goal_version<>?`, runID, nextVersion); err != nil {
			return err
		}
	}
	if status == RunStopped || status == RunFailed || status == RunCompleted {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_signals WHERE run_id=?`, runID); err != nil {
			return err
		}
	}
	return nil
}

func (r *SQLiteRepository) ClaimPending(ctx context.Context, runID string, version uint64, now time.Time) (pendingSignal, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return pendingSignal{}, false, err
	}
	defer tx.Rollback()
	var runStatus RunStatus
	var runPayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT status,payload FROM runs WHERE id=?`, runID).Scan(&runStatus, &runPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pendingSignal{}, false, ErrNotFound
		}
		return pendingSignal{}, false, err
	}
	var current Run
	if err := json.Unmarshal(runPayload, &current); err != nil {
		return pendingSignal{}, false, fmt.Errorf("decode Run before pending claim: %w", err)
	}
	if runStatus != RunRunning || current.GoalVersion != version {
		if current.GoalVersion != version {
			if _, err := tx.ExecContext(ctx, `DELETE FROM pending_signals WHERE run_id=? AND goal_version<>?`, runID, current.GoalVersion); err != nil {
				return pendingSignal{}, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return pendingSignal{}, false, err
		}
		return pendingSignal{}, false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_id,goal_version,enqueued_at_ns,expires_at_ns,priority,live,payload,frame FROM pending_signals WHERE run_id=? AND status='pending'`, runID)
	if err != nil {
		return pendingSignal{}, false, err
	}
	var items []pendingSignal
	for rows.Next() {
		var eventID string
		var itemVersion uint64
		var enqueuedNS, expiresNS int64
		var priority, live int
		var payload, frame []byte
		if err := rows.Scan(&eventID, &itemVersion, &enqueuedNS, &expiresNS, &priority, &live, &payload, &frame); err != nil {
			_ = rows.Close()
			return pendingSignal{}, false, err
		}
		var event SignalEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			_ = rows.Close()
			return pendingSignal{}, false, fmt.Errorf("decode pending SQLite signal: %w", err)
		}
		items = append(items, pendingSignal{event: event, version: itemVersion, enqueuedAt: time.Unix(0, enqueuedNS).UTC(), expires: time.Unix(0, expiresNS).UTC(), priority: priority, live: live != 0, frameBytes: frame})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return pendingSignal{}, false, err
	}
	if err := rows.Close(); err != nil {
		return pendingSignal{}, false, err
	}
	kept := items[:0]
	for _, item := range items {
		if item.version != version || !now.Before(item.expires) {
			if err := deleteSQLitePending(ctx, tx, runID, item.event.ID); err != nil {
				return pendingSignal{}, false, err
			}
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == 0 {
		if err := tx.Commit(); err != nil {
			return pendingSignal{}, false, err
		}
		return pendingSignal{}, false, nil
	}
	selector := signalQueue{items: kept, now: func() time.Time { return now }}
	item := kept[selector.nextIndex(now)]
	result, err := tx.ExecContext(ctx, `UPDATE pending_signals SET status='processing' WHERE run_id=? AND event_id=? AND status='pending'`, runID, item.event.ID)
	if err != nil {
		return pendingSignal{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return pendingSignal{}, false, err
	}
	if changed != 1 {
		return pendingSignal{}, false, errors.New("pending signal claim lost")
	}
	if err := tx.Commit(); err != nil {
		return pendingSignal{}, false, err
	}
	return item, true, nil
}

func (r *SQLiteRepository) FinishPending(ctx context.Context, runID, eventID string, requeue bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if requeue {
		_, err := r.db.ExecContext(ctx, `UPDATE pending_signals SET status='pending' WHERE run_id=? AND event_id=? AND status='processing'`, runID, eventID)
		return err
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM pending_signals WHERE run_id=? AND event_id=?`, runID, eventID)
	return err
}

func (r *SQLiteRepository) RecoverPending(ctx context.Context, runID string, version uint64, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_signals WHERE run_id=? AND (goal_version<>? OR expires_at_ns<=?)`, runID, version, now.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pending_signals SET status='pending' WHERE run_id=? AND status='processing'`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) PendingCounts(ctx context.Context, runID string) (int, bool, error) {
	var waiting int
	var reviewing int
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='processing' THEN 1 ELSE 0 END),0) FROM pending_signals WHERE run_id=?`, runID).Scan(&waiting, &reviewing)
	return waiting, reviewing > 0, err
}

func (r *SQLiteRepository) SearchObservations(ctx context.Context, runID string, query ObservationQuery) (ObservationPage, error) {
	if err := ctx.Err(); err != nil {
		return ObservationPage{}, err
	}
	var exists int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id=?`, runID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ObservationPage{}, ErrNotFound
		}
		return ObservationPage{}, err
	}
	query = normalizeObservationQuery(query)
	base := []string{"run_id = ?"}
	args := []any{runID}
	add := func(condition string, values ...any) {
		base = append(base, condition)
		args = append(args, values...)
	}
	if query.EventID != "" {
		add("event_id = ?", query.EventID)
	}
	if query.RoomID != "" {
		add("room_id = ?", query.RoomID)
	}
	if query.Source != "" {
		add("source = ?", query.Source)
	}
	if !query.From.IsZero() {
		add("observed_at_ns >= ?", query.From.UnixNano())
	}
	if !query.Until.IsZero() {
		add("observed_at_ns <= ?", query.Until.UnixNano())
	}
	if len(query.TextAny) > 0 {
		conditions := make([]string, 0, len(query.TextAny))
		for _, term := range query.TextAny {
			conditions = append(conditions, "instr(lower(text), lower(?)) > 0")
			args = append(args, term)
		}
		base = append(base, "("+strings.Join(conditions, " OR ")+")")
	}
	where := strings.Join(base, " AND ")
	var page ObservationPage
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM observations WHERE `+where, args...).Scan(&page.TotalMatches); err != nil {
		return ObservationPage{}, err
	}
	pageArgs := append([]any(nil), args...)
	pageWhere := where
	if !query.Before.IsZero() {
		pageWhere += ` AND (observed_at_ns < ? OR (observed_at_ns = ? AND (sequence < ? OR (sequence = ? AND event_id < ?))))`
		cursorNS := query.Before.UnixNano()
		pageArgs = append(pageArgs, cursorNS, cursorNS, query.BeforeSequence, query.BeforeSequence, query.BeforeEventID)
	}
	pageArgs = append(pageArgs, query.Limit+1)
	rows, err := r.db.QueryContext(ctx, `SELECT payload FROM observations WHERE `+pageWhere+` ORDER BY observed_at_ns DESC, sequence DESC, event_id DESC LIMIT ?`, pageArgs...)
	if err != nil {
		return ObservationPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return ObservationPage{}, err
		}
		var event SignalEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return ObservationPage{}, fmt.Errorf("decode SQLite observation: %w", err)
		}
		page.Events = append(page.Events, event)
	}
	if err := rows.Err(); err != nil {
		return ObservationPage{}, err
	}
	if len(page.Events) > query.Limit {
		page.HasMore = true
		page.Events = page.Events[:query.Limit]
		last := page.Events[len(page.Events)-1]
		page.NextBefore, page.NextBeforeSequence, page.NextBeforeEventID = last.ObservedAt, last.Sequence, last.ID
	}
	return page, nil
}

func (r *SQLiteRepository) Close() error { return r.db.Close() }

// ExportJSON writes a consistent, human-readable snapshot and observation
// ledger into a new directory. Existing destinations are never overwritten.
func (r *SQLiteRepository) ExportJSON(ctx context.Context, targetDir string) error {
	if strings.TrimSpace(targetDir) == "" {
		return errors.New("JSON export directory is required")
	}
	parent := filepath.Dir(targetDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create JSON export parent: %w", err)
	}
	if _, err := os.Stat(targetDir); err == nil {
		return fmt.Errorf("JSON export destination already exists: %s", targetDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".liveguard-json-export-*")
	if err != nil {
		return fmt.Errorf("create JSON export staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	snapshot := jsonExportSnapshot{Version: 1, Runs: []*Run{}}
	runs, err := tx.QueryContext(ctx, `SELECT payload FROM runs ORDER BY id`)
	if err != nil {
		return err
	}
	for runs.Next() {
		var payload []byte
		var run Run
		if err := runs.Scan(&payload); err != nil {
			_ = runs.Close()
			return err
		}
		if err := json.Unmarshal(payload, &run); err != nil {
			_ = runs.Close()
			return fmt.Errorf("decode Run for JSON export: %w", err)
		}
		snapshot.Runs = append(snapshot.Runs, cloneRun(&run))
	}
	if err := runs.Err(); err != nil {
		_ = runs.Close()
		return err
	}
	if err := runs.Close(); err != nil {
		return err
	}
	snapshotData, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "liveguard.json"), append(snapshotData, '\n'), 0o600); err != nil {
		return fmt.Errorf("write JSON Run snapshot: %w", err)
	}
	ledger, err := os.OpenFile(filepath.Join(stage, "liveguard.json.observations.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(ledger)
	observations, err := tx.QueryContext(ctx, `SELECT run_id,payload FROM observations ORDER BY run_id,observed_at_ns,sequence,event_id`)
	if err != nil {
		_ = ledger.Close()
		return err
	}
	for observations.Next() {
		var runID string
		var payload []byte
		if err := observations.Scan(&runID, &payload); err != nil {
			_ = observations.Close()
			_ = ledger.Close()
			return err
		}
		var event SignalEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			_ = observations.Close()
			_ = ledger.Close()
			return fmt.Errorf("decode observation for JSON export: %w", err)
		}
		line, err := json.Marshal(struct {
			RunID string      `json:"run_id"`
			Event SignalEvent `json:"event"`
		}{RunID: runID, Event: event})
		if err != nil {
			_ = observations.Close()
			_ = ledger.Close()
			return err
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			_ = observations.Close()
			_ = ledger.Close()
			return err
		}
	}
	if err := observations.Err(); err != nil {
		_ = observations.Close()
		_ = ledger.Close()
		return err
	}
	if err := observations.Close(); err != nil {
		_ = ledger.Close()
		return err
	}
	if err := writer.Flush(); err != nil {
		_ = ledger.Close()
		return err
	}
	if err := ledger.Sync(); err != nil {
		_ = ledger.Close()
		return err
	}
	if err := ledger.Close(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := os.Rename(stage, targetDir); err != nil {
		return fmt.Errorf("publish JSON export directory: %w", err)
	}
	return nil
}
