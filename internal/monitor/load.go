package monitor

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type SyntheticLoadReport struct {
	Source                   string  `json:"source"`
	GoVersion                string  `json:"go_version"`
	LogicalCPUs              int     `json:"logical_cpus"`
	Monitors                 int     `json:"monitors"`
	Concurrency              int     `json:"concurrency"`
	Requests                 int     `json:"requests"`
	Accepted                 int     `json:"accepted"`
	Errors                   int     `json:"errors"`
	MockReviewLatencyMS      int64   `json:"mock_review_latency_ms"`
	ModelReviews             int     `json:"model_reviews"`
	Backpressure             int     `json:"backpressure"`
	MaxInFlight              int64   `json:"max_in_flight"`
	ElapsedMS                int64   `json:"elapsed_ms"`
	P95IngestMS              int64   `json:"p95_ingest_ms"`
	RequestsPerSecond        float64 `json:"requests_per_second"`
	StoppedRejectsLateSignal bool    `json:"stopped_rejects_late_signal"`
}

type loadReviewer struct {
	active, peak atomic.Int64
	delay        time.Duration
}

type asyncLoadReviewer struct {
	active, peak atomic.Int64
	delay        time.Duration
	mu           sync.Mutex
	submitted    map[string]time.Time
	queueWait    []int64
	reviewTime   []int64
	endToEnd     []int64
}

func (r *asyncLoadReviewer) Review(ctx context.Context, input UnexpectedEventInput) (UnexpectedEventDecision, error) {
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for {
		peak := r.peak.Load()
		if active <= peak || r.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	started := time.Now()
	if len(input.RecentEvidence) > 0 {
		r.mu.Lock()
		if submitted, ok := r.submitted[input.RecentEvidence[0].ID]; ok {
			r.queueWait = append(r.queueWait, started.Sub(submitted).Milliseconds())
		}
		r.mu.Unlock()
	}
	if !wait(ctx, r.delay) {
		return UnexpectedEventDecision{}, ctx.Err()
	}
	finished := time.Now()
	r.mu.Lock()
	r.reviewTime = append(r.reviewTime, finished.Sub(started).Milliseconds())
	if len(input.RecentEvidence) > 0 {
		if submitted, ok := r.submitted[input.RecentEvidence[0].ID]; ok {
			r.endToEnd = append(r.endToEnd, finished.Sub(submitted).Milliseconds())
		}
	}
	r.mu.Unlock()
	return UnexpectedEventDecision{Action: "ignore", Category: "other", Reason: "模拟普通内容", Confidence: .9}, nil
}

type AsyncSyntheticLoadReport struct {
	Source                  string  `json:"source"`
	GoVersion               string  `json:"go_version"`
	LogicalCPUs             int     `json:"logical_cpus"`
	Monitors                int     `json:"monitors"`
	Concurrency             int     `json:"concurrency"`
	Requests                int     `json:"requests"`
	Accepted                int     `json:"accepted"`
	Errors                  int     `json:"errors"`
	MockReviewLatencyMS     int64   `json:"mock_review_latency_ms"`
	QueueItemsProcessed     int     `json:"queue_items_processed"`
	QueueItemsFailed        int     `json:"queue_items_failed"`
	ModelReviewsCompleted   int     `json:"model_reviews_completed"`
	BackpressureAttempts    int     `json:"backpressure_attempts"`
	Retries                 int     `json:"retries"`
	MaxInFlight             int64   `json:"max_in_flight"`
	MaxWaitingPerMonitor    int     `json:"max_waiting_per_monitor"`
	ElapsedMS               int64   `json:"elapsed_ms"`
	RequestsPerSecond       float64 `json:"requests_per_second"`
	P95SubmitMS             int64   `json:"p95_submit_ms"`
	P95QueueWaitMS          int64   `json:"p95_queue_wait_ms"`
	P95ReviewMS             int64   `json:"p95_review_ms"`
	P95SubmitToReviewDoneMS int64   `json:"p95_submit_to_review_done_ms"`
}

// RunAsyncSyntheticLoad measures the production SubmitSignal admission and
// per-monitor queue path with a deterministic mock reviewer. It excludes OCR,
// browser capture, storage, network and real model latency/tokens.
func RunAsyncSyntheticLoad(ctx context.Context, requests int) (AsyncSyntheticLoadReport, error) {
	if requests < 8 || requests > 10000 {
		return AsyncSyntheticLoadReport{}, fmt.Errorf("requests must be 8-10000")
	}
	report := AsyncSyntheticLoadReport{Source: "synthetic_submit_signal_mock_model_no_capture", GoVersion: runtime.Version(), LogicalCPUs: runtime.NumCPU(), Monitors: 8, Concurrency: 8, Requests: requests, MockReviewLatencyMS: 40}
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	reviewer := &asyncLoadReviewer{delay: 40 * time.Millisecond, submitted: make(map[string]time.Time, requests)}
	s.SetUnexpectedEventReviewer(reviewer, time.Nanosecond)
	ids := make([]string, 8)
	for i := range ids {
		run, err := s.Start(ctx, StartInput{TargetURL: fmt.Sprintf("https://example.com/async-load/%d", i), Goal: "实时重要公告提醒", SourceMode: "ocr_upload"})
		if err != nil {
			return report, err
		}
		ids[i] = run.ID
	}
	var mu sync.Mutex
	submitLatencies := make([]int64, 0, requests)
	var accepted, failures, backpressure, retries atomic.Int64
	var maxWaiting atomic.Int64
	var workers sync.WaitGroup
	start := time.Now()
	gate := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			<-gate
			for i := worker; i < requests; i += 8 {
				event := SignalEvent{ID: fmt.Sprintf("async-load-%d", i), Source: SignalOCR, Text: "直播普通文字 标记 " + loadLabel(i), Confidence: .9, ValidUntil: time.Now().Add(30 * time.Second)}
				for {
					attemptAt := time.Now()
					reviewer.mu.Lock()
					reviewer.submitted[event.ID] = attemptAt
					reviewer.mu.Unlock()
					err := s.SubmitSignal(ctx, ids[worker], event)
					if err == nil {
						accepted.Add(1)
						mu.Lock()
						submitLatencies = append(submitLatencies, time.Since(attemptAt).Milliseconds())
						mu.Unlock()
						break
					}
					if err != ErrPendingFull || ctx.Err() != nil {
						failures.Add(1)
						break
					}
					backpressure.Add(1)
					retries.Add(1)
					if !wait(ctx, 5*time.Millisecond) {
						failures.Add(1)
						break
					}
				}
				s.mu.Lock()
				q := s.pending[ids[worker]]
				s.mu.Unlock()
				if q != nil {
					waiting := int64(q.stats().Waiting)
					for peak := maxWaiting.Load(); waiting > peak && !maxWaiting.CompareAndSwap(peak, waiting); peak = maxWaiting.Load() {
					}
				}
			}
		}(worker)
	}
	close(gate)
	workers.Wait()
	// Wait for actual queue processing to finish, not just the admission receipts.
	for {
		allDone := true
		for _, id := range ids {
			s.mu.Lock()
			q := s.pending[id]
			s.mu.Unlock()
			if q != nil {
				status := q.stats()
				if status.Waiting > 0 || status.Reviewing {
					allDone = false
					break
				}
			}
		}
		if allDone || ctx.Err() != nil || !wait(ctx, time.Millisecond) {
			break
		}
	}
	elapsed := time.Since(start)
	report.Accepted, report.Errors = int(accepted.Load()), int(failures.Load())
	report.BackpressureAttempts, report.Retries = int(backpressure.Load()), int(retries.Load())
	report.MaxInFlight, report.MaxWaitingPerMonitor = reviewer.peak.Load(), int(maxWaiting.Load())
	report.ElapsedMS = elapsed.Milliseconds()
	if elapsed > 0 {
		report.RequestsPerSecond = float64(report.Accepted) / elapsed.Seconds()
	}
	sort.Slice(submitLatencies, func(i, j int) bool { return submitLatencies[i] < submitLatencies[j] })
	reviewer.mu.Lock()
	queueWait := append([]int64(nil), reviewer.queueWait...)
	reviewTime := append([]int64(nil), reviewer.reviewTime...)
	endToEnd := append([]int64(nil), reviewer.endToEnd...)
	reviewer.mu.Unlock()
	report.ModelReviewsCompleted = len(reviewTime)
	for _, id := range ids {
		s.mu.Lock()
		q := s.pending[id]
		s.mu.Unlock()
		if q != nil {
			status := q.stats()
			report.QueueItemsProcessed += int(status.Processed)
			report.QueueItemsFailed += int(status.Failed)
		}
	}
	report.P95SubmitMS = percentile95(submitLatencies)
	report.P95QueueWaitMS = percentile95(queueWait)
	report.P95ReviewMS = percentile95(reviewTime)
	report.P95SubmitToReviewDoneMS = percentile95(endToEnd)
	if report.Errors > 0 || report.Accepted != requests || report.QueueItemsProcessed != requests || report.QueueItemsFailed > 0 || report.MaxInFlight > maxConcurrentReviews || report.MaxWaitingPerMonitor > maxPendingSignals {
		return report, fmt.Errorf("async synthetic load invariant failed: accepted=%d/%d errors=%d processed=%d failed=%d model_reviews=%d peak=%d waiting=%d", report.Accepted, requests, report.Errors, report.QueueItemsProcessed, report.QueueItemsFailed, report.ModelReviewsCompleted, report.MaxInFlight, report.MaxWaitingPerMonitor)
	}
	return report, ctx.Err()
}

func loadLabel(value int) string {
	var label []byte
	for value >= 0 {
		label = append(label, byte('A'+value%26))
		value = value/26 - 1
	}
	for i, j := 0, len(label)-1; i < j; i, j = i+1, j-1 {
		label[i], label[j] = label[j], label[i]
	}
	return string(label)
}

func percentile95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[(95*len(values)+99)/100-1]
}

func (r *loadReviewer) Review(ctx context.Context, _ UnexpectedEventInput) (UnexpectedEventDecision, error) {
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for {
		peak := r.peak.Load()
		if active <= peak || r.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	if !wait(ctx, r.delay) {
		return UnexpectedEventDecision{}, ctx.Err()
	}
	return UnexpectedEventDecision{Action: "ignore", Category: "other", Reason: "模拟普通内容", Confidence: .9}, nil
}

// This measures the real Ingest/single-flight/backpressure path with synthetic
// signals and a deliberately slow mock provider. Browser, OCR, ASR, network,
// tokens and actual LLM throughput are excluded.
func RunSyntheticLoad(ctx context.Context, requests int) (SyntheticLoadReport, error) {
	if requests < 8 || requests > 10000 {
		return SyntheticLoadReport{}, fmt.Errorf("requests must be 8-10000")
	}
	report := SyntheticLoadReport{Source: "synthetic_signals_mock_model_no_capture", GoVersion: runtime.Version(), LogicalCPUs: runtime.NumCPU(), Monitors: 8, Concurrency: 8, Requests: requests, MockReviewLatencyMS: 40}
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	reviewer := &loadReviewer{delay: 40 * time.Millisecond}
	s.SetUnexpectedEventReviewer(reviewer, time.Nanosecond)
	ids := make([]string, 8)
	for i := range ids {
		run, err := s.Start(ctx, StartInput{TargetURL: fmt.Sprintf("https://example.com/live/%d", i), Goal: "实时重要公告提醒", SourceMode: "ocr_upload"})
		if err != nil {
			return report, err
		}
		ids[i] = run.ID
	}
	var mu sync.Mutex
	latencies := make([]int64, 0, requests)
	var accepted, failed atomic.Int64
	var workers sync.WaitGroup
	start := time.Now()
	gate := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			<-gate
			for i := worker; i < requests; i += 8 {
				at := time.Now()
				err := s.Ingest(ctx, ids[worker], SignalEvent{ID: fmt.Sprintf("load-%d", i), Source: SignalOCR, Text: fmt.Sprintf("直播普通文字 %d", i), Confidence: .9})
				if err == nil {
					accepted.Add(1)
				} else {
					failed.Add(1)
				}
				mu.Lock()
				latencies = append(latencies, time.Since(at).Milliseconds())
				mu.Unlock()
			}
		}(worker)
	}
	close(gate)
	workers.Wait()
	elapsed := time.Since(start)
	report.Accepted, report.Errors = int(accepted.Load()), int(failed.Load())
	report.ElapsedMS = elapsed.Milliseconds()
	report.MaxInFlight = reviewer.peak.Load()
	report.RequestsPerSecond = float64(report.Accepted) / elapsed.Seconds()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	report.P95IngestMS = latencies[(95*len(latencies)+99)/100-1]
	for _, id := range ids {
		run, err := s.Get(ctx, id)
		if err != nil {
			return report, err
		}
		report.ModelReviews += int(run.Sampling.ReviewCount)
		report.Backpressure += run.ReviewBackpressureCount
	}
	if _, err := s.Stop(ctx, ids[0]); err != nil {
		return report, err
	}
	report.StoppedRejectsLateSignal = s.Ingest(ctx, ids[0], SignalEvent{Source: SignalOCR, Text: "停止后的迟到信号", Confidence: .9}) != nil
	return report, ctx.Err()
}
