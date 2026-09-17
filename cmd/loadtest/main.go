package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type task struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Report    *struct {
		CompletedAt time.Time `json:"completed_at"`
	} `json:"report"`
}

type campaign struct {
	ID        string `json:"id"`
	Total     int    `json:"total"`
	Queued    int    `json:"queued"`
	Running   int    `json:"running"`
	Passed    int    `json:"passed"`
	Attention int    `json:"attention"`
	Retrying  int    `json:"retrying"`
	Tasks     []task `json:"tasks"`
}

type report struct {
	CampaignID       string  `json:"campaign_id"`
	Requested        int     `json:"requested"`
	Passed           int     `json:"passed"`
	Attention        int     `json:"attention"`
	WallTimeMS       int64   `json:"wall_time_ms"`
	ThroughputPerSec float64 `json:"throughput_per_sec"`
	LatencyP50MS     int64   `json:"latency_p50_ms"`
	LatencyP95MS     int64   `json:"latency_p95_ms"`
	MeasuredAt       string  `json:"measured_at"`
}

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "LiveGuard base URL")
	count := flag.Int("count", 12, "number of rooms (1-50)")
	timeout := flag.Duration("timeout", 4*time.Minute, "time allowed for the campaign")
	output := flag.String("output", "", "optional JSON output path")
	flag.Parse()
	if *count < 1 || *count > 50 {
		fatalf("count must be between 1 and 50")
	}
	baseURL := strings.TrimRight(*base, "/")
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		fatalf("invalid base URL: %v", err)
	}
	urls := make([]string, *count)
	stamp := time.Now().UnixNano()
	for index := range urls {
		urls[index] = fmt.Sprintf("%s/demo/live?load=%d-%d", baseURL, stamp, index+1)
	}
	payload := map[string]any{
		"name":                 fmt.Sprintf("并发验收 %d 个直播间", *count),
		"urls":                 urls,
		"expected_texts":       []string{"海边电台", "互动入口已开启"},
		"expected_live_status": "live",
	}
	started := time.Now()
	var current campaign
	doJSON(http.MethodPost, baseURL+"/api/campaigns", payload, &current)
	deadline := time.Now().Add(*timeout)
	for current.Queued+current.Running > 0 {
		if time.Now().After(deadline) {
			fatalf("campaign %s timed out: queued=%d running=%d", current.ID, current.Queued, current.Running)
		}
		time.Sleep(500 * time.Millisecond)
		doJSON(http.MethodGet, baseURL+"/api/campaigns/"+url.PathEscape(current.ID), nil, &current)
	}
	elapsed := time.Since(started)
	latencies := make([]time.Duration, 0, len(current.Tasks))
	for _, item := range current.Tasks {
		finished := item.UpdatedAt
		if item.Report != nil && !item.Report.CompletedAt.IsZero() {
			finished = item.Report.CompletedAt
		}
		latencies = append(latencies, finished.Sub(item.CreatedAt))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	result := report{
		CampaignID: current.ID, Requested: current.Total, Passed: current.Passed, Attention: current.Attention,
		WallTimeMS: elapsed.Milliseconds(), ThroughputPerSec: float64(current.Total) / elapsed.Seconds(),
		LatencyP50MS: percentile(latencies, 0.50).Milliseconds(), LatencyP95MS: percentile(latencies, 0.95).Milliseconds(),
		MeasuredAt: time.Now().UTC().Format(time.RFC3339),
	}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(encoded))
	if *output != "" {
		if err := os.WriteFile(*output, append(encoded, '\n'), 0o644); err != nil {
			fatalf("write report: %v", err)
		}
	}
	if result.Attention > 0 || result.Passed != result.Requested {
		os.Exit(1)
	}
}

func percentile(values []time.Duration, quantile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)-1)*quantile + 0.5)
	return values[index]
}

func doJSON(method, target string, input any, output any) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			fatalf("encode request: %v", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		fatalf("request %s: %v", target, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		fatalf("read response: %v", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		fatalf("request %s returned %s: %s", target, response.Status, data)
	}
	if err := json.Unmarshal(data, output); err != nil {
		fatalf("decode response: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
