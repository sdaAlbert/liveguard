package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"liveguard/internal/domain"
)

type PostgresStore struct {
	db *sql.DB
}

func OpenPostgres(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	store := &PostgresStore{db: db}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS agent_tasks (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			campaign_id TEXT NOT NULL DEFAULT '',
			payload JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS agent_tasks_created_idx ON agent_tasks (created_at DESC);
		CREATE INDEX IF NOT EXISTS agent_tasks_campaign_idx ON agent_tasks (campaign_id, created_at);
		CREATE INDEX IF NOT EXISTS agent_tasks_dispatch_idx ON agent_tasks (status, updated_at);
	`)
	return err
}

func (s *PostgresStore) Create(task *domain.Task) error {
	return s.CreateMany([]*domain.Task{task})
}

func (s *PostgresStore) CreateMany(tasks []*domain.Task) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, task := range tasks {
		payload, err := json.Marshal(task)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_tasks (id,status,campaign_id,payload,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,$6)`, task.ID, task.Status, task.CampaignID, payload, task.CreatedAt, task.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) Update(id string, mutate func(*domain.Task) error) (*domain.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM agent_tasks WHERE id=$1 FOR UPDATE`, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var task domain.Task
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, err
	}
	if err := mutate(&task); err != nil {
		return nil, err
	}
	task.UpdatedAt = time.Now().UTC()
	task.Version++
	payload, err = json.Marshal(&task)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status=$2,campaign_id=$3,payload=$4,updated_at=$5 WHERE id=$1`, id, task.Status, task.CampaignID, payload, task.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return clone(&task), nil
}

func (s *PostgresStore) Get(id string) (*domain.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var payload []byte
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_tasks WHERE id=$1`, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var task domain.Task
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (s *PostgresStore) List() []*domain.Task {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM agent_tasks ORDER BY created_at DESC LIMIT 2000`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var tasks []*domain.Task
	for rows.Next() {
		var payload []byte
		var task domain.Task
		if rows.Scan(&payload) == nil && json.Unmarshal(payload, &task) == nil {
			tasks = append(tasks, &task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	return tasks
}

func (s *PostgresStore) RecoverInterrupted() error {
	for _, task := range s.List() {
		if task.Status != domain.StatusPlanning && task.Status != domain.StatusRunning {
			continue
		}
		if _, err := s.Update(task.ID, func(current *domain.Task) error {
			current.Status = domain.StatusQueued
			current.Error = ""
			current.AddEvent("recovery", "检测到未完成任务，已重新进入队列")
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) Close() error { return s.db.Close() }
