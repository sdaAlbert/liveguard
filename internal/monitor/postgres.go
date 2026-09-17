package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
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
	`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return repository, nil
}

func (r *PostgresRepository) Create(ctx context.Context, run *Run) error {
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO live_monitor_runs (id,status,payload,started_at,updated_at) VALUES ($1,$2,$3,$4,$5)`, run.ID, run.Status, payload, run.StartedAt, run.UpdatedAt)
	return err
}

func (r *PostgresRepository) Update(ctx context.Context, id string, mutate func(*Run) error) (*Run, error) {
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
	if err := mutate(&run); err != nil {
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
	rows, err := r.db.QueryContext(ctx, `SELECT payload FROM live_monitor_runs ORDER BY started_at DESC LIMIT 50`)
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
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

func (r *PostgresRepository) Close() error { return r.db.Close() }
