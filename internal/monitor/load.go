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
