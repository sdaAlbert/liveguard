package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

	"liveguard/internal/domain"
)

type ModelCallRecord struct {
	ID             string    `json:"id"`
	RunID          string    `json:"run_id,omitempty"`
	EventID        string    `json:"event_id,omitempty"`
	AttemptID      string    `json:"attempt_id,omitempty"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	Outcome        string    `json:"outcome"`
	FailureKind    string    `json:"failure_kind,omitempty"`
	InputTokens    int64     `json:"input_tokens"`
	OutputTokens   int64     `json:"output_tokens"`
	LatencyMS      int64     `json:"latency_ms"`
	TransportRetry int       `json:"transport_retries"`
	CreatedAt      time.Time `json:"created_at"`
}

type ModelCallRepository interface {
	RecordModelCall(context.Context, ModelCallRecord) error
	ModelCallSummary(context.Context, time.Time) (ModelCallSummary, error)
}

type ModelCallSummary struct {
	Calls          int64               `json:"calls"`
	Failures       int64               `json:"failures"`
	ByFailure      map[string]int64    `json:"by_failure"`
	InputTokens    int64               `json:"input_tokens"`
	OutputTokens   int64               `json:"output_tokens"`
	EstimatedCost  *float64            `json:"estimated_cost,omitempty"`
	CostKnownCalls int64               `json:"cost_known_calls"`
	UsageByModel   map[string][3]int64 `json:"-"`
}

type QueueRunMetric struct {
	RunID        string `json:"run_id"`
	Name         string `json:"name"`
	Waiting      int    `json:"waiting"`
	OldestWaitMS int64  `json:"oldest_wait_ms"`
	Reviewing    bool   `json:"reviewing"`
}

type RuntimeMetrics struct {
	GeneratedAt              time.Time        `json:"generated_at"`
	WindowStart              time.Time        `json:"window_start"`
	GoRuntime                GoRuntimeMetrics `json:"go_runtime"`
	QueueWaiting             int              `json:"queue_waiting"`
	QueueOldestWaitMS        int64            `json:"queue_oldest_wait_ms"`
	Queues                   []QueueRunMetric `json:"queues"`
	ModelCalls               ModelCallSummary `json:"model_calls"`
	AlertSamples             int64            `json:"alert_samples"`
	AverageReceivedToAlertMS int64            `json:"average_received_to_alert_ms"`
	AverageSignalToAlertMS   int64            `json:"average_signal_to_alert_ms"`
	AlertHistoryLimited      bool             `json:"alert_history_limited"`
}

// GoRuntimeMetrics is a point-in-time snapshot of the current Go process.
// These values are sampled when /api/metrics is requested; they are not a
// historical time series by themselves.
type GoRuntimeMetrics struct {
	Goroutines int    `json:"goroutines"`
	HeapAlloc  uint64 `json:"heap_alloc_bytes"`
	HeapInuse  uint64 `json:"heap_inuse_bytes"`
	HeapSys    uint64 `json:"heap_sys_bytes"`
	Sys        uint64 `json:"sys_bytes"`
	TotalAlloc uint64 `json:"total_alloc_bytes"`
	NumGC      uint32 `json:"num_gc"`
}

type ModelPricing struct {
	InputPerMillion  float64 `json:"input_per_million"`
	OutputPerMillion float64 `json:"output_per_million"`
}

func modelFailureKind(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, ErrModelQuotaExhausted) {
		return "quota"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "429") || strings.Contains(message, "rate limit") || strings.Contains(message, "too many requests") {
		return "rate_limited"
	}
	if strings.Contains(message, "decode") || strings.Contains(message, "invalid") || strings.Contains(message, "missing assistant") {
		return "format"
	}
	return "network_or_provider"
}

func loadModelPricing() map[string]ModelPricing {
	var prices map[string]ModelPricing
	if json.Unmarshal([]byte(os.Getenv("LIVEGUARD_MODEL_PRICING")), &prices) != nil {
		return nil
	}
	return prices
}

func (s *Service) observeModelCall(provider, configuredModel string) func(context.Context, ModelReply, error, time.Duration) {
	return func(ctx context.Context, reply ModelReply, err error, duration time.Duration) {
		store, ok := s.repository.(ModelCallRepository)
		if !ok {
			return
		}
		trace := modelCallTraceFromContext(ctx)
		model := strings.TrimSpace(reply.Model)
		if model == "" {
			model = configuredModel
		}
		record := ModelCallRecord{ID: domain.NewID("model_call"), RunID: trace.RunID, EventID: trace.ReviewID, AttemptID: trace.AttemptID, Provider: provider, Model: model, Outcome: "success", InputTokens: reply.InputTokens, OutputTokens: reply.OutputTokens, LatencyMS: duration.Milliseconds(), TransportRetry: reply.TransportRetries, CreatedAt: time.Now().UTC()}
		if err != nil {
			record.Outcome = "failure"
			record.FailureKind = modelFailureKind(ctx, err)
		}
		persistCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if persistErr := store.RecordModelCall(persistCtx, record); persistErr != nil {
			log.Printf("persist model call metric failed: run=%s provider=%s model=%s error=%q", record.RunID, record.Provider, record.Model, persistErr)
		}
	}
}

func summarizeCost(summary *ModelCallSummary) {
	prices := loadModelPricing()
	if len(prices) == 0 {
		return
	}
	var total float64
	var known int64
	// Cost is calculated from provider-reported token usage. Failed calls with
	// no usage data may still have incurred provider charges.
	for model, usage := range summary.UsageByModel {
		price, ok := prices[model]
		if !ok {
			continue
		}
		total += float64(usage[0])*price.InputPerMillion/1_000_000 + float64(usage[1])*price.OutputPerMillion/1_000_000
		known += usage[2]
	}
	summary.CostKnownCalls = known
	if known > 0 {
		summary.EstimatedCost = &total
	}
}

// RuntimeMetrics returns a compact operational view. Queue age is live; model
// calls are persisted; alert latency uses retained Run history and is therefore
// a recent sample, not a lifetime counter.
func (s *Service) RuntimeMetrics(ctx context.Context) (RuntimeMetrics, error) {
	now := time.Now().UTC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	metrics := RuntimeMetrics{
		GeneratedAt: now,
		WindowStart: now.Add(-24 * time.Hour),
		GoRuntime: GoRuntimeMetrics{
			Goroutines: runtime.NumGoroutine(),
			HeapAlloc:  mem.HeapAlloc,
			HeapInuse:  mem.HeapInuse,
			HeapSys:    mem.HeapSys,
			Sys:        mem.Sys,
			TotalAlloc: mem.TotalAlloc,
			NumGC:      mem.NumGC,
		},
		Queues: []QueueRunMetric{},
	}
	runs, err := s.repository.List(ctx)
	if err != nil {
		return metrics, err
	}
	for _, run := range runs {
		if run.Status == RunStopped || run.Status == RunCompleted {
			continue
		}
		s.mu.Lock()
		q := s.pending[run.ID]
		s.mu.Unlock()
		if q == nil {
			continue
		}
		status := q.stats()
		metrics.QueueWaiting += status.Waiting
		metrics.QueueOldestWaitMS = max(metrics.QueueOldestWaitMS, status.OldestWaitMS)
		metrics.Queues = append(metrics.Queues, QueueRunMetric{RunID: run.ID, Name: run.Name, Waiting: status.Waiting, OldestWaitMS: status.OldestWaitMS, Reviewing: status.Reviewing})
	}
	if store, ok := s.repository.(ModelCallRepository); ok {
		metrics.ModelCalls, err = store.ModelCallSummary(ctx, metrics.WindowStart)
		if err != nil {
			return metrics, err
		}
	}
	var receivedTotal, signalTotal int64
	for _, run := range runs {
		for _, alert := range run.Alerts {
			if alert.CreatedAt.Before(metrics.WindowStart) {
				continue
			}
			if alert.ReceivedAt.IsZero() {
				continue
			}
			metrics.AlertSamples++
			receivedTotal += alert.ReceivedToAlertMS
			signalTotal += alert.SignalToAlertMS
		}
		metrics.AlertHistoryLimited = metrics.AlertHistoryLimited || run.HistoryTruncated
	}
	if metrics.AlertSamples > 0 {
		metrics.AverageReceivedToAlertMS = receivedTotal / metrics.AlertSamples
		metrics.AverageSignalToAlertMS = signalTotal / metrics.AlertSamples
	}
	return metrics, nil
}
