package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"liveguard/internal/browser"
)

type soakReviewer struct {
	mu           sync.Mutex
	active, peak int
	perRun       map[string]int
	calls        map[string]int
	overlapped   bool
}

func (r *soakReviewer) Review(ctx context.Context, input UnexpectedEventInput) (UnexpectedEventDecision, error) {
	key := input.Goal
	r.mu.Lock()
	r.active++
	r.perRun[key]++
	r.calls[key]++
	r.peak = max(r.peak, r.active)
	r.overlapped = r.overlapped || r.perRun[key] > 1
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active--; r.perRun[key]--; r.mu.Unlock() }()
	select {
	case <-time.After(15 * time.Second):
		return UnexpectedEventDecision{Action: "wait", Confidence: .9, StopReason: "finished", Model: "slow-fixture"}, nil
	case <-ctx.Done():
		return UnexpectedEventDecision{}, ctx.Err()
	}
}

// Opt-in: real Chrome screenshots, real Tesseract and local file storage,
// with eight changing source pages and a deliberately slow model substitute.
// No platform login, user profile or paid API is used.
func TestEightBrowserMonitorsSustained(t *testing.T) {
	raw := os.Getenv("LIVEGUARD_STABILITY_DURATION")
	if raw == "" {
		t.Skip("set LIVEGUARD_STABILITY_DURATION=6m for Chrome/OCR soak")
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration < 30*time.Second {
		t.Fatal("stability duration must be at least 30s")
	}
	output := os.Getenv("LIVEGUARD_STABILITY_OUTPUT")
	if output == "" {
		output = t.TempDir()
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><title>Stability room %s</title></head><body style="font:20px Arial;background:white;color:black"><h1>Live monitor room %s 直播监控</h1><div style="display:grid;grid-template-columns:65%% 35%%;line-height:1.7"><section>MAIN SCREEN 主屏幕<div id="main"></div></section><section>CHAT AREA 聊天区<div id="messages"></div></section></div><script>let n=0;function tick(){n++;document.getElementById('main').innerText=Array.from({length:18},(_,i)=>'节目正常进行 CURRENT '+n+' ITEM '+i+' 商品展示 价格 '+(100+i)).join('\n');document.getElementById('messages').innerText=Array.from({length:18},(_,i)=>'用户 '+i+' MESSAGE '+n+' 新的评论正在出现').join('\n');}setInterval(tick,2000);tick();</script></body></html>`, r.URL.Path, r.URL.Path)
	}))
	defer server.Close()
	repo, err := OpenFileRepository(filepath.Join(output, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	capturer := &browser.LiveCapturer{ChromePath: `C:\Program Files\Google\Chrome\Application\chrome.exe`, ProfileDir: filepath.Join(output, "chrome-profile"), Headless: true, Warmup: 100 * time.Millisecond}
	defer capturer.Shutdown()
	s := NewService(repo, nil)
	defer s.StopAll()
	s.SetFrameCapturer(capturer)
	s.SetTextRecognizer(TesseractRecognizer{Binary: `C:\Program Files\Tesseract-OCR\tesseract.exe`, Language: "chi_sim+eng", Timeout: 8 * time.Second})
	reviewer := &soakReviewer{perRun: map[string]int{}, calls: map[string]int{}}
	s.SetUnexpectedEventReviewer(reviewer, 15*time.Second)
	var ids []string
	for i := 0; i < maxActiveMonitors; i++ {
		run, err := s.Start(context.Background(), StartInput{TargetURL: fmt.Sprintf("%s/room-%d", server.URL, i), Goal: fmt.Sprintf("实时关注直播间 %d 的重要消息", i), SourceMode: "browser_ocr"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	if _, err := s.Start(context.Background(), StartInput{TargetURL: server.URL + "/overflow", Goal: "extra", SourceMode: "browser_ocr"}); err != ErrCapacity {
		t.Fatalf("ninth room admission = %v", err)
	}
	started := time.Now()
	previous := map[string]uint64{}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for time.Since(started) < duration {
		<-ticker.C
		for _, id := range ids {
			run, err := s.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != RunRunning || run.Rooms[0].Health == SourceFailed || run.Rooms[0].Health == SourceNeedsLogin {
				t.Fatalf("source stopped: %#v", run.Rooms[0])
			}
			if time.Since(started) > 30*time.Second && (run.Sampling.CaptureCount <= previous[id] || time.Since(run.Rooms[0].LastCaptureAt) > 20*time.Second) {
				t.Fatalf("capture stalled: %s count=%d last=%s", id, run.Sampling.CaptureCount, run.Rooms[0].LastCaptureAt)
			}
			previous[id] = run.Sampling.CaptureCount
			if run.Pending != nil && (run.Pending.Waiting > maxPendingSignals || run.Pending.Failed > 0 || run.Pending.Expired > 0) {
				t.Fatalf("review backlog unhealthy: %s %#v", id, run.Pending)
			}
		}
		if int(time.Since(started).Seconds())%30 < 10 {
			t.Logf("eight rooms making progress: elapsed=%s", time.Since(started).Round(time.Second))
		}
	}
	reviewer.mu.Lock()
	peak, overlap := reviewer.peak, reviewer.overlapped
	for _, id := range ids {
		run, _ := s.Get(context.Background(), id)
		if reviewer.calls[run.Goal] < 2 {
			t.Errorf("room received fewer than two model turns: %s", id)
		}
	}
	reviewer.mu.Unlock()
	if peak != maxConcurrentReviews || overlap {
		t.Errorf("concurrency peak=%d overlap=%t", peak, overlap)
	}
	var runs []*Run
	for _, id := range ids {
		run, err := s.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Rooms[0].Health != SourceHealthy {
			t.Errorf("source did not recover healthy: %s", id)
		}
		runs = append(runs, run)
	}
	report := struct {
		DurationSeconds float64 `json:"duration_seconds"`
		Rooms           int     `json:"rooms"`
		PeakReviews     int     `json:"peak_reviews"`
		Overlap         bool    `json:"same_room_overlap"`
		OCRLanguage     string  `json:"ocr_language"`
		Runs            []*Run  `json:"runs"`
	}{time.Since(started).Seconds(), len(ids), peak, overlap, "chi_sim+eng", runs}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(output, "report.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("stability report: %s", filepath.Join(output, "report.json"))
}
