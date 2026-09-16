package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"liveguard/internal/domain"
)

const sandboxStream = "liveguard:sandbox:runs"
const sandboxGroup = "sandbox-workers"
const sandboxDeadLetterStream = "liveguard:sandbox:dlq"

const runLease = 45 * time.Second

type PostgresRunStore struct{ db *sql.DB }

func OpenPostgresRunStore(ctx context.Context, databaseURL string) (*PostgresRunStore, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	store := &PostgresRunStore{db: db}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresRunStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS sandbox_runs (
			id TEXT PRIMARY KEY,
			idempotency_key TEXT UNIQUE,
			suite_id TEXT NOT NULL DEFAULT '',
			scenario TEXT NOT NULL,
			status TEXT NOT NULL,
			payload JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS sandbox_runs_created_at_idx ON sandbox_runs (created_at DESC);
		CREATE INDEX IF NOT EXISTS sandbox_runs_status_idx ON sandbox_runs (status);
		ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS lease_owner TEXT;
		ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
		ALTER TABLE sandbox_runs ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0;
		CREATE TABLE IF NOT EXISTS sandbox_outbox (
			run_id TEXT PRIMARY KEY REFERENCES sandbox_runs(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			published_at TIMESTAMPTZ
		);
	`)
	if err != nil {
		return fmt.Errorf("postgres migrate: %w", err)
	}
	return nil
}

func (s *PostgresRunStore) InsertOrGet(ctx context.Context, run *Run) (*Run, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	payload, _ := json.Marshal(run)
	var stored []byte
	err = tx.QueryRowContext(ctx, `
		INSERT INTO sandbox_runs (id, idempotency_key, suite_id, scenario, status, payload, created_at)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING payload`, run.ID, run.IdempotencyKey, run.SuiteID, run.Scenario, run.Status, payload, run.CreatedAt).Scan(&stored)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_outbox (run_id) VALUES ($1) ON CONFLICT DO NOTHING`, run.ID); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return cloneRun(run), true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) || run.IdempotencyKey == "" {
		return nil, false, err
	}
	err = tx.QueryRowContext(ctx, `SELECT payload FROM sandbox_runs WHERE idempotency_key=$1`, run.IdempotencyKey).Scan(&stored)
	if err != nil {
		return nil, false, err
	}
	var existing Run
	if err := json.Unmarshal(stored, &existing); err != nil {
		return nil, false, err
	}
	if existing.RequestHash != "" && run.RequestHash != "" && existing.RequestHash != run.RequestHash {
		return nil, false, fmt.Errorf("idempotency key already exists with different input")
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &existing, false, nil
}

func (s *PostgresRunStore) Save(ctx context.Context, run *Run) error {
	payload, _ := json.Marshal(run)
	result, err := s.db.ExecContext(ctx, `UPDATE sandbox_runs SET status=$2, payload=$3, lease_owner=NULL, lease_until=NULL, updated_at=NOW() WHERE id=$1`, run.ID, run.Status, payload)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return fmt.Errorf("sandbox run %s not found", run.ID)
	}
	return nil
}

func (s *PostgresRunStore) SaveClaimed(ctx context.Context, run *Run, owner string) (bool, error) {
	payload, _ := json.Marshal(run)
	result, err := s.db.ExecContext(ctx, `
		UPDATE sandbox_runs
		SET status=$2, payload=$3, lease_owner=NULL, lease_until=NULL, updated_at=NOW()
		WHERE id=$1 AND attempt=$4 AND lease_owner=$5`, run.ID, run.Status, payload, run.Attempt, owner)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows == 1, nil
}

func (s *PostgresRunStore) Claim(ctx context.Context, id, owner string) (*Run, bool, error) {
	var payload []byte
	var attempt int
	err := s.db.QueryRowContext(ctx, `
		UPDATE sandbox_runs
		SET status='running', lease_owner=$2, lease_until=NOW()+$3::interval,
		    attempt=attempt+1,
		    payload=jsonb_set(jsonb_set(payload, '{status}', '"running"'), '{attempt}', to_jsonb(attempt+1)),
		    updated_at=NOW()
		WHERE id=$1 AND status NOT IN ('passed', 'failed')
		  AND (lease_until IS NULL OR lease_until < NOW() OR lease_owner=$2)
		RETURNING payload, attempt`, id, owner, fmt.Sprintf("%d seconds", int(runLease.Seconds()))).Scan(&payload, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var run Run
	if err := json.Unmarshal(payload, &run); err != nil {
		return nil, false, err
	}
	run.Attempt = attempt
	return &run, true, nil
}

func (s *PostgresRunStore) Unpublished(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id FROM sandbox_outbox WHERE published_at IS NULL ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *PostgresRunStore) MarkPublished(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sandbox_outbox SET published_at=NOW() WHERE run_id=$1`, runID)
	return err
}

func (s *PostgresRunStore) Get(ctx context.Context, id string) (*Run, error) {
	var payload []byte
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM sandbox_runs WHERE id=$1`, id).Scan(&payload); err != nil {
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(payload, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *PostgresRunStore) List(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM sandbox_runs ORDER BY created_at DESC LIMIT 100`)
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
		if json.Unmarshal(payload, &run) == nil {
			runs = append(runs, &run)
		}
	}
	return runs, rows.Err()
}

func (s *PostgresRunStore) Close() error { return s.db.Close() }

func (s *PostgresRunStore) Count(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_runs`).Scan(&count)
	return count, err
}

func (s *PostgresRunStore) OutboxBacklog(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_outbox WHERE published_at IS NULL`).Scan(&count)
	return count, err
}

type RedisStreamQueue struct {
	client   *redis.Client
	consumer string
}

func OpenRedisStreamQueue(ctx context.Context, address, consumer string) (*RedisStreamQueue, error) {
	client := redis.NewClient(&redis.Options{Addr: address})
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	queue := &RedisStreamQueue{client: client, consumer: consumer}
	err := client.XGroupCreateMkStream(ctx, sandboxStream, sandboxGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		client.Close()
		return nil, fmt.Errorf("redis create group: %w", err)
	}
	return queue, nil
}

func (q *RedisStreamQueue) Publish(ctx context.Context, run *Run) error {
	return q.client.XAdd(ctx, &redis.XAddArgs{Stream: sandboxStream, Values: map[string]any{"run_id": run.ID, "traceparent": run.TraceParent, "tracestate": run.TraceState}}).Err()
}

func (q *RedisStreamQueue) readNew(ctx context.Context) ([]redis.XMessage, error) {
	streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: sandboxGroup, Consumer: q.consumer, Streams: []string{sandboxStream, ">"}, Count: 1, Block: 2 * time.Second}).Result()
	if err != nil {
		return nil, err
	}
	if len(streams) == 0 {
		return nil, nil
	}
	return streams[0].Messages, nil
}

func (q *RedisStreamQueue) claimStale(ctx context.Context) ([]redis.XMessage, error) {
	messages, _, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: sandboxStream, Group: sandboxGroup, Consumer: q.consumer, MinIdle: 50 * time.Second, Start: "0-0", Count: 10}).Result()
	return messages, err
}

func (q *RedisStreamQueue) Ack(ctx context.Context, messageID string) error {
	return q.client.XAck(ctx, sandboxStream, sandboxGroup, messageID).Err()
}

func (q *RedisStreamQueue) DeadLetter(ctx context.Context, message redis.XMessage, reason string) error {
	values := map[string]any{
		"source_stream": sandboxStream,
		"source_id":     message.ID,
		"reason":        reason,
		"failed_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	for key, value := range message.Values {
		values[key] = value
	}
	pipe := q.client.TxPipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{Stream: sandboxDeadLetterStream, Values: values})
	pipe.XAck(ctx, sandboxStream, sandboxGroup, message.ID)
	_, err := pipe.Exec(ctx)
	return err
}

func (q *RedisStreamQueue) Close() error { return q.client.Close() }

func (q *RedisStreamQueue) Pending(ctx context.Context) (int64, error) {
	pending, err := q.client.XPending(ctx, sandboxStream, sandboxGroup).Result()
	if err != nil {
		return 0, err
	}
	return pending.Count, nil
}

func (q *RedisStreamQueue) DeadLetterCount(ctx context.Context) (int64, error) {
	return q.client.XLen(ctx, sandboxDeadLetterStream).Result()
}

type DurableLab struct {
	executor ExecutionEngine
	store    *PostgresRunStore
	queue    *RedisStreamQueue
}

func NewDurableLab(executor ExecutionEngine, store *PostgresRunStore, queue *RedisStreamQueue) *DurableLab {
	return &DurableLab{executor: executor, store: store, queue: queue}
}

func (l *DurableLab) Submit(scenarioName, suiteID string) (*Run, error) {
	return l.SubmitWithKey(scenarioName, suiteID, "")
}

func (l *DurableLab) SubmitWithKey(scenarioName, suiteID, idempotencyKey string) (*Run, error) {
	return l.submitWithInput(context.Background(), scenarioName, suiteID, idempotencyKey, nil)
}

func (l *DurableLab) submitWithInput(parent context.Context, scenarioName, suiteID, idempotencyKey string, input *ToolInput) (*Run, error) {
	spec, ok := scenarios[scenarioName]
	if !ok {
		return nil, fmt.Errorf("unsupported scenario %q", scenarioName)
	}
	run := &Run{ID: domain.NewID("sbx"), IdempotencyKey: strings.TrimSpace(idempotencyKey), RequestHash: hashRequest(scenarioName, input), SuiteID: suiteID, Scenario: scenarioName, ScenarioLabel: spec.label, Input: input, Status: "queued", Policy: l.executor.Policy(spec.timeout), CreatedAt: time.Now().UTC()}
	InjectTrace(parent, run)
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	stored, created, err := l.store.InsertOrGet(ctx, run)
	if err != nil {
		return nil, err
	}
	_ = created // PostgreSQL outbox publisher owns Redis delivery.
	return stored, nil
}

func (l *DurableLab) RunTool(ctx context.Context, input ToolInput, idempotencyKey string) (*Run, error) {
	run, err := l.submitWithInput(ctx, ScenarioDiagnosePage, "", idempotencyKey, &input)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := l.store.Get(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		if current.Status == "passed" || current.Status == "failed" {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *DurableLab) SubmitSuite() ([]*Run, error) { return l.SubmitSuiteWithKey("") }

func (l *DurableLab) SubmitSuiteWithKey(idempotencyKey string) ([]*Run, error) {
	suiteID := domain.NewID("suite")
	runs := make([]*Run, 0, len(scenarioOrder))
	for _, scenarioName := range scenarioOrder {
		key := ""
		if idempotencyKey != "" {
			key = idempotencyKey + ":" + scenarioName
		}
		run, err := l.SubmitWithKey(scenarioName, suiteID, key)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func (l *DurableLab) List() []*Run {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runs, err := l.store.List(ctx)
	if err != nil {
		return []*Run{}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	return runs
}

func (l *DurableLab) InfraStatus() InfraStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status := InfraStatus{Mode: "durable", Postgres: "healthy", Redis: "healthy", Stream: sandboxStream, Worker: "unknown", RPC: "grpc"}
	count, err := l.store.Count(ctx)
	if err != nil {
		status.Postgres = "unavailable"
	} else {
		status.StoredRuns = count
	}
	outbox, err := l.store.OutboxBacklog(ctx)
	if err != nil {
		status.Postgres = "unavailable"
	} else {
		status.Outbox = outbox
	}
	pending, err := l.queue.Pending(ctx)
	if err != nil {
		status.Redis = "unavailable"
	} else {
		status.Pending = pending
	}
	dlq, err := l.queue.DeadLetterCount(ctx)
	if err != nil {
		status.Redis = "unavailable"
	} else {
		status.DeadLetter = dlq
	}
	if checker, ok := l.executor.(HealthChecker); ok {
		if err := checker.Health(ctx); err != nil {
			status.Worker = "unavailable"
		} else {
			status.Worker = "healthy"
		}
	}
	return status
}

func (l *DurableLab) Start(ctx context.Context) {
	go l.publishOutbox(ctx)
	for ctx.Err() == nil {
		messages, err := l.queue.claimStale(ctx)
		if err != nil && !errors.Is(err, redis.Nil) && ctx.Err() == nil {
			log.Printf("sandbox reclaim: %v", err)
		}
		if len(messages) == 0 {
			messages, err = l.queue.readNew(ctx)
		}
		if err != nil {
			if !errors.Is(err, redis.Nil) && ctx.Err() == nil {
				log.Printf("sandbox stream read: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		for _, message := range messages {
			l.handleMessage(ctx, message)
		}
	}
}

func (l *DurableLab) publishOutbox(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		ids, err := l.store.Unpublished(ctx, 50)
		if err == nil {
			for _, id := range ids {
				run, err := l.store.Get(ctx, id)
				if err != nil {
					break
				}
				if err := l.queue.Publish(ctx, run); err != nil {
					break
				}
				if err := l.store.MarkPublished(ctx, id); err != nil {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (l *DurableLab) handleMessage(ctx context.Context, message redis.XMessage) {
	ctx = ExtractTrace(ctx, fmt.Sprint(message.Values["traceparent"]), fmt.Sprint(message.Values["tracestate"]))
	ctx, span := otel.Tracer("liveguard/sandbox").Start(ctx, "redis.sandbox.consume", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()
	runID := fmt.Sprint(message.Values["run_id"])
	if runID == "" || runID == "<nil>" {
		if err := l.queue.DeadLetter(ctx, message, "missing run_id"); err != nil {
			log.Printf("sandbox dead letter %s: %v", message.ID, err)
		}
		return
	}
	run, claimed, err := l.store.Claim(ctx, runID, l.queue.consumer)
	if err != nil {
		log.Printf("sandbox run load %s: %v", runID, err)
		return
	}
	if !claimed {
		if _, getErr := l.store.Get(ctx, runID); errors.Is(getErr, sql.ErrNoRows) {
			if dlqErr := l.queue.DeadLetter(ctx, message, "sandbox run not found"); dlqErr != nil {
				log.Printf("sandbox dead letter %s: %v", message.ID, dlqErr)
			}
			return
		} else if getErr != nil {
			log.Printf("sandbox run verify %s: %v", runID, getErr)
			return
		}
		if ackErr := l.queue.Ack(ctx, message.ID); ackErr != nil {
			log.Printf("sandbox ack terminal run %s: %v", runID, ackErr)
		}
		return
	}
	if err := l.executor.Execute(ctx, run); err != nil {
		log.Printf("sandbox execute %s attempt %d: %v", run.ID, run.Attempt, err)
		return
	}
	if ctx.Err() != nil {
		// Leave the stream entry pending. A new consumer will XAUTOCLAIM it and
		// repeat the idempotent run after the visibility timeout.
		return
	}
	saved, err := l.store.SaveClaimed(context.Background(), run, l.queue.consumer)
	if err != nil {
		log.Printf("sandbox run save %s: %v", run.ID, err)
		return
	}
	if !saved {
		log.Printf("sandbox stale result rejected %s attempt %d", run.ID, run.Attempt)
		return
	}
	if err := l.queue.Ack(context.Background(), message.ID); err != nil {
		log.Printf("sandbox ack run %s: %v", run.ID, err)
	}
}
