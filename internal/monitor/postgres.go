package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type PostgresRepository struct {
	db *sql.DB
}

func OpenPostgres(ctx context.Context, databaseURL string) (*PostgresRepository, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	repository := &PostgresRepository{db: db}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS live_monitor_runs (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			payload JSONB NOT NULL,
			started_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS live_monitor_runs_started_idx ON live_monitor_runs (started_at DESC);
		CREATE INDEX IF NOT EXISTS live_monitor_runs_status_idx ON live_monitor_runs (status);
		CREATE TABLE IF NOT EXISTS live_monitor_observations (
			run_id TEXT NOT NULL REFERENCES live_monitor_runs(id) ON DELETE CASCADE,
			event_id TEXT NOT NULL,
			sequence BIGINT NOT NULL,
			room_id TEXT NOT NULL,
			source TEXT NOT NULL,
			text TEXT NOT NULL,
			confidence DOUBLE PRECISION NOT NULL,
			observed_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (run_id, event_id)
		);
		CREATE INDEX IF NOT EXISTS live_monitor_observations_time_idx ON live_monitor_observations (run_id, observed_at DESC);
	`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO live_monitor_observations (run_id,event_id,sequence,room_id,source,text,confidence,observed_at,payload)
		SELECT r.id, e->>'id', COALESCE(NULLIF(e->>'sequence','')::BIGINT,0), COALESCE(e->>'room_id',''), COALESCE(e->>'source',''), COALESCE(e->>'text',''), COALESCE(NULLIF(e->>'confidence','')::DOUBLE PRECISION,0), (e->>'observed_at')::TIMESTAMPTZ, e
		FROM live_monitor_runs r CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(r.payload->'events')='array' THEN r.payload->'events' ELSE '[]'::JSONB END) e
		WHERE COALESCE(e->>'id','') <> '' AND COALESCE(e->>'observed_at','') <> ''
		ON CONFLICT (run_id,event_id) DO NOTHING;
		INSERT INTO live_monitor_observations (run_id,event_id,sequence,room_id,source,text,confidence,observed_at,payload)
		SELECT r.id, 'vision:' || (i->>'id') || ':' || (e.ordinality-1)::TEXT, 0, COALESCE(i->>'room_id',''), 'vision', COALESCE(e.item->>'text',''), COALESCE(NULLIF(e.item->>'confidence','')::DOUBLE PRECISION,0), (e.item->>'observed_at')::TIMESTAMPTZ,
			jsonb_build_object('id','vision:' || (i->>'id') || ':' || (e.ordinality-1)::TEXT,'parent_event_id',COALESCE(e.item->>'event_id',''),'room_id',COALESCE(i->>'room_id',''),'room_name',COALESCE(i->>'room_name',''),'source','vision','text',COALESCE(e.item->>'text',''),'confidence',COALESCE(NULLIF(e.item->>'confidence','')::DOUBLE PRECISION,0),'observed_at',e.item->'observed_at')
		FROM live_monitor_runs r CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(r.payload->'investigations')='array' THEN r.payload->'investigations' ELSE '[]'::JSONB END) i
		CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(i->'evidence')='array' THEN i->'evidence' ELSE '[]'::JSONB END) WITH ORDINALITY AS e(item, ordinality)
		WHERE e.item->>'source'='vision' AND COALESCE(e.item->>'text','') <> '' AND COALESCE(e.item->>'observed_at','') <> ''
		ON CONFLICT (run_id,event_id) DO NOTHING;
	`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return repository, nil
}

func (r *PostgresRepository) Create(ctx context.Context, run *Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if run == nil || run.ID == "" || run.Status != RunRunning {
		return errors.New("new monitor must have an id and start running")
	}
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO live_monitor_runs (id,status,payload,started_at,updated_at) VALUES ($1,$2,$3,$4,$5)`, run.ID, run.Status, payload, run.StartedAt, run.UpdatedAt); err != nil {
		return err
	}
	if err = syncPostgresLedger(ctx, tx, run.ID, nil, run); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *PostgresRepository) Update(ctx context.Context, id string, mutate func(*Run) error) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mutate == nil {
		return nil, errors.New("monitor update callback is required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM live_monitor_runs WHERE id=$1 FOR UPDATE`, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(payload, &run); err != nil {
		return nil, err
	}
	previousStatus := run.Status
	previousEvents := append([]SignalEvent(nil), run.Events...)
	previousInvestigations := append([]Investigation(nil), run.Investigations...)
	if err := mutate(&run); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	desiredStatus := run.Status
	run.Status = previousStatus
	if err := TransitionRun(&run, desiredStatus); err != nil {
		return nil, err
	}
	if err := validateInvestigationUpdates(previousInvestigations, run.Investigations); err != nil {
		return nil, err
	}
	boundRunHistory(&run)
	if err := syncPostgresLedger(ctx, tx, id, &Run{Events: previousEvents, Investigations: previousInvestigations}, &run); err != nil {
		return nil, err
	}
	run.UpdatedAt = time.Now().UTC()
	run.Version++
	payload, err = json.Marshal(&run)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE live_monitor_runs SET status=$2,payload=$3,updated_at=$4 WHERE id=$1`, id, run.Status, payload, run.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cloneRun(&run), nil
}

func syncPostgresLedger(ctx context.Context, tx *sql.Tx, runID string, previous, next *Run) error {
	oldEvents := map[string]bool{}
	oldInvestigations := map[string]Investigation{}
	if previous != nil {
		for _, event := range previous.Events {
			oldEvents[event.ID] = true
		}
		for _, item := range previous.Investigations {
			oldInvestigations[item.ID] = item
		}
	}
	for _, event := range next.Events {
		if event.ID == "" || oldEvents[event.ID] {
			continue
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO live_monitor_observations (run_id,event_id,sequence,room_id,source,text,confidence,observed_at,payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (run_id,event_id) DO NOTHING`, runID, event.ID, event.Sequence, event.RoomID, event.Source, event.Text, event.Confidence, event.ObservedAt, payload); err != nil {
			return err
		}
	}
	for _, item := range next.Investigations {
		before, exists := oldInvestigations[item.ID]
		if item.ID == "" || exists && before.Status == item.Status && before.UpdatedAt.Equal(item.UpdatedAt) {
			continue
		}
		for _, observation := range visualObservations(item) {
			observationPayload, err := json.Marshal(observation)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO live_monitor_observations (run_id,event_id,sequence,room_id,source,text,confidence,observed_at,payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (run_id,event_id) DO NOTHING`, runID, observation.ID, observation.Sequence, observation.RoomID, observation.Source, observation.Text, observation.Confidence, observation.ObservedAt, observationPayload); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *PostgresRepository) SearchObservations(ctx context.Context, runID string, query ObservationQuery) (ObservationPage, error) {
	query = normalizeObservationQuery(query)
	var textPatterns []string
	for _, term := range query.TextAny {
		textPatterns = append(textPatterns, likePattern(escapeLike(term)))
	}
	filters := `run_id=$1 AND ($2='' OR event_id=$2) AND ($3='' OR room_id=$3) AND ($4='' OR source=$4) AND (COALESCE(cardinality($5::text[]),0)=0 OR text ILIKE ANY($5::text[])) AND ($6::timestamptz IS NULL OR observed_at >= $6) AND ($7::timestamptz IS NULL OR observed_at <= $7)`
	var totalMatches int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_monitor_observations WHERE `+filters, runID, query.EventID, query.RoomID, query.Source, textPatterns, nullableTime(query.From), nullableTime(query.Until)).Scan(&totalMatches); err != nil {
		return ObservationPage{}, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT observed_at,payload FROM live_monitor_observations WHERE `+filters+` AND ($8::timestamptz IS NULL OR observed_at < $8 OR (observed_at=$8 AND (sequence < $9 OR (sequence=$9 AND event_id < $10)))) ORDER BY observed_at DESC, sequence DESC, event_id DESC LIMIT $11`, runID, query.EventID, query.RoomID, query.Source, textPatterns, nullableTime(query.From), nullableTime(query.Until), nullableTime(query.Before), query.BeforeSequence, query.BeforeEventID, query.Limit+1)
	if err != nil {
		return ObservationPage{}, err
	}
	defer rows.Close()
	result := make([]SignalEvent, 0, query.Limit+1)
	for rows.Next() {
		var payload []byte
		var observedAt time.Time
		var event SignalEvent
		if err := rows.Scan(&observedAt, &payload); err != nil {
			return ObservationPage{}, err
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return ObservationPage{}, err
		}
		event.ObservedAt = observedAt
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return ObservationPage{}, err
	}
	page := ObservationPage{HasMore: len(result) > query.Limit, TotalMatches: totalMatches}
	if page.HasMore {
		result = result[:query.Limit]
	}
	page.Events = result
	if page.HasMore && len(page.Events) > 0 {
		last := page.Events[len(page.Events)-1]
		page.NextBefore = last.ObservedAt
		page.NextBeforeSequence = last.Sequence
		page.NextBeforeEventID = last.ID
	}
	return page, nil
}

func (r *PostgresRepository) RecordObservation(ctx context.Context, runID string, event SignalEvent) error {
	if event.ID == "" {
		return errors.New("observation id is required")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO live_monitor_observations (run_id,event_id,sequence,room_id,source,text,confidence,observed_at,payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (run_id,event_id) DO NOTHING`, runID, event.ID, event.Sequence, event.RoomID, event.Source, event.Text, event.Confidence, event.ObservedAt, payload)
	return err
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func likePattern(value string) string {
	if value == "" {
		return ""
	}
	return "%" + value + "%"
}

func (r *PostgresRepository) Get(ctx context.Context, id string) (*Run, error) {
	var payload []byte
	if err := r.db.QueryRowContext(ctx, `SELECT payload FROM live_monitor_runs WHERE id=$1`, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(payload, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *PostgresRepository) List(ctx context.Context) ([]*Run, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT payload FROM live_monitor_runs WHERE status IN ('running','paused') OR id IN (SELECT id FROM live_monitor_runs WHERE status NOT IN ('running','paused') ORDER BY started_at DESC LIMIT 50) ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		var payload []byte
		var run Run
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &run); err != nil {
			return nil, err
		}
		runs = append(runs, &run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return runs, nil
}

func (r *PostgresRepository) ListRunning(ctx context.Context) ([]*Run, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT payload FROM live_monitor_runs WHERE status=$1 ORDER BY started_at ASC`, RunRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		var payload []byte
		var run Run
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &run); err != nil {
			return nil, err
		}
		runs = append(runs, &run)
	}
	return runs, rows.Err()
}

func (r *PostgresRepository) Close() error { return r.db.Close() }
