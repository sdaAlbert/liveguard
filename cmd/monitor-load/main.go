package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"liveguard/internal/monitor"
	"os"
	"path/filepath"
	"time"
)

func main() {
	mode := flag.String("mode", "sync", "load mode: sync (Ingest) or async (SubmitSignal)")
	count := flag.Int("requests", 160, "synthetic request count (8-10000)")
	out := flag.String("out", ".cache/monitor-load.json", "JSON report path")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var report any
	var err error
	switch *mode {
	case "sync":
		report, err = monitor.RunSyntheticLoad(ctx, *count)
	case "async":
		report, err = monitor.RunAsyncSyntheticLoad(ctx, *count)
	default:
		err = fmt.Errorf("mode must be sync or async")
	}
	if err == nil {
		var body []byte
		body, err = json.MarshalIndent(report, "", "  ")
		if err == nil {
			err = os.MkdirAll(filepath.Dir(*out), 0700)
		}
		if err == nil {
			err = os.WriteFile(*out, body, 0600)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Synthetic/%s mock report written: %s\n", *mode, *out)
	switch r := report.(type) {
	case monitor.SyntheticLoadReport:
		fmt.Printf("accepted=%d/%d reviews=%d peak=%d backpressure=%d ingest_p95=%dms stopped_rejects=%t\n", r.Accepted, r.Requests, r.ModelReviews, r.MaxInFlight, r.Backpressure, r.P95IngestMS, r.StoppedRejectsLateSignal)
		if r.Errors > 0 || r.MaxInFlight > 4 || !r.StoppedRejectsLateSignal {
			os.Exit(1)
		}
	case monitor.AsyncSyntheticLoadReport:
		fmt.Printf("accepted=%d/%d queue_processed=%d model_reviews=%d peak=%d waiting_peak=%d backpressure=%d submit_p95=%dms review_queue_p95=%dms review_p95=%dms submit_to_review_p95=%dms throughput=%.1f/s\n", r.Accepted, r.Requests, r.QueueItemsProcessed, r.ModelReviewsCompleted, r.MaxInFlight, r.MaxWaitingPerMonitor, r.BackpressureAttempts, r.P95SubmitMS, r.P95QueueWaitMS, r.P95ReviewMS, r.P95SubmitToReviewDoneMS, r.RequestsPerSecond)
	}
}
