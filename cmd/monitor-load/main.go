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
	count := flag.Int("requests", 160, "synthetic request count (8-10000)")
	out := flag.String("out", ".cache/monitor-load.json", "JSON report path")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := monitor.RunSyntheticLoad(ctx, *count)
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
	fmt.Printf("Synthetic/mock only: accepted=%d/%d reviews=%d peak=%d backpressure=%d P95=%dms stopped_rejects=%t\nReport: %s\n", report.Accepted, report.Requests, report.ModelReviews, report.MaxInFlight, report.Backpressure, report.P95IngestMS, report.StoppedRejectsLateSignal, *out)
	if report.Errors > 0 || report.MaxInFlight > 4 || !report.StoppedRejectsLateSignal {
		os.Exit(1)
	}
}
