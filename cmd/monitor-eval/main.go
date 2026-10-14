package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"liveguard/internal/monitor"
)

func main() {
	out := flag.String("out", ".cache/monitor-eval.json", "JSON report path")
	split := flag.String("split", "dev", "dataset split: dev or holdout-v1")
	flag.Parse()
	key := os.Getenv("MINIMAX_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "MINIMAX_API_KEY is required; use scripts/verify-minimax-live.ps1 -Evaluate to reuse your existing configuration")
		os.Exit(1)
	}
	model := os.Getenv("LIVEGUARD_MONITOR_MODEL")
	if model == "" {
		model = "MiniMax-M3"
	}
	r := &monitor.MiniMaxUnexpectedEventReviewer{APIKey: key, Model: model, Endpoint: os.Getenv("LIVEGUARD_MONITOR_ENDPOINT")}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cases, ok := monitor.SignalEvalCasesForSplit(*split)
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown dataset split; choose dev or holdout-v1")
		os.Exit(2)
	}
	report := monitor.RunSignalEvalCases(ctx, r, *split, cases)
	report.Source = "fixed_text_signals_real_minimax"
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(*out), 0700)
	}
	if err == nil {
		err = os.WriteFile(*out, encoded, 0600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "save eval report:", err)
		os.Exit(1)
	}
	fmt.Printf("Fixed text evaluation: %d/%d passed; precision=%.2f recall=%.2f false_positive_rate=%.2f errors=%d P95=%dms tokens=%d/%d tools=%d\nReport: %s\n", report.Passed, len(report.Cases), report.Precision, report.Recall, report.FalsePositiveRate, report.Errors, report.P95LatencyMS, report.InputTokens, report.OutputTokens, report.ToolCalls, *out)
	if report.Passed != len(cases) {
		os.Exit(1)
	}
}
