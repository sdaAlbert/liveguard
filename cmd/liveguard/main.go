package main

import (
	"bufio"
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"liveguard/internal/agent"
	"liveguard/internal/browser"
	"liveguard/internal/domain"
	"liveguard/internal/monitor"
	"liveguard/internal/telemetry"
	webapp "liveguard/internal/web"
)

func main() {
	loadEnvFiles(".env.local", filepath.Join("..", ".env.local"))
	normalizeLocalProxyEnv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTrace := func(context.Context) error { return nil }
	if endpoint := strings.TrimSpace(os.Getenv("LIVEGUARD_OTEL_ENDPOINT")); endpoint != "" && !strings.EqualFold(endpoint, "off") && !strings.EqualFold(endpoint, "disabled") {
		var err error
		shutdownTrace, err = telemetry.Setup(ctx, "liveguard-control-plane", endpoint)
		if err != nil {
			log.Fatalf("telemetry: %v", err)
		}
	} else {
		log.Print("OpenTelemetry export disabled; set LIVEGUARD_OTEL_ENDPOINT to an externally managed OTLP Collector to enable traces")
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTrace(shutdownCtx)
	}()
	durable := strings.EqualFold(os.Getenv("LIVEGUARD_INFRA_MODE"), "durable")
	databaseURL := envOr("LIVEGUARD_DATABASE_URL", "postgres://liveguard:liveguard_local@127.0.0.1:5433/liveguard?sslmode=disable")
	chromePath := envOr("LIVEGUARD_CHROME_PATH", defaultChromePath())
	webServer := webapp.New("artifacts")
	var monitorRepository monitor.Repository
	if durable {
		monitorCtx, cancelMonitor := context.WithTimeout(ctx, 10*time.Second)
		postgresMonitors, monitorErr := monitor.OpenPostgres(monitorCtx, databaseURL)
		cancelMonitor()
		if monitorErr != nil {
			log.Fatalf("durable monitor store: %v", monitorErr)
		}
		defer postgresMonitors.Close()
		monitorRepository = postgresMonitors
	} else {
		monitorRepository = monitor.NewMemoryRepository()
	}
	monitorService := monitor.NewService(monitorRepository, func() {
		webServer.Publish(domain.Event{ID: domain.NewID("event"), Type: "monitor_update", Message: "monitor state updated", At: time.Now().UTC()})
	})
	monitorLLMMode := strings.ToLower(envOr("LIVEGUARD_MONITOR_LLM_MODE", "deterministic"))
	monitorProvider := strings.ToLower(envOr("LIVEGUARD_MONITOR_PROVIDER", "openai"))
	interval := envDuration("LIVEGUARD_MONITOR_REVIEW_INTERVAL", 15*time.Second)
	switch {
	case monitorLLMMode != "auto":
		log.Print("unexpected livestream event review disabled; set LIVEGUARD_MONITOR_LLM_MODE=auto to enable")
	case monitorProvider == "minimax":
		if key := strings.TrimSpace(os.Getenv("MINIMAX_API_KEY")); key != "" {
			model := envOr("LIVEGUARD_MONITOR_MODEL", "MiniMax-M3")
			endpoint := envOr("LIVEGUARD_MONITOR_ENDPOINT", "https://api.minimaxi.com/anthropic/v1/messages")
			monitorService.SetGoalPlanner(&monitor.MiniMaxGoalPlanner{
				APIKey: key, Model: model, Endpoint: endpoint, Client: agent.DefaultHTTPClient(),
			})
			monitorService.SetUnexpectedEventReviewer(&monitor.MiniMaxUnexpectedEventReviewer{
				APIKey: key, Model: model, Endpoint: envOr("LIVEGUARD_MONITOR_ENDPOINT", "https://api.minimaxi.com/anthropic/v1/messages"),
				Client: agent.DefaultHTTPClient(), Breaker: agent.NewCircuitBreaker(2, 30*time.Second),
				SignalAnalyzer: monitor.LocalSignalAnalyzer{},
			}, interval)
			log.Printf("MiniMax goal planning and unexpected livestream review enabled with model %s (hard minimum interval %s)", model, interval)
		} else {
			log.Print("MiniMax event review disabled; MINIMAX_API_KEY is not configured")
		}
	case monitorProvider == "openai":
		if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
			model := envOr("LIVEGUARD_MONITOR_MODEL", envOr("LIVEGUARD_MODEL", "gpt-5.6-luna"))
			monitorService.SetUnexpectedEventReviewer(&monitor.OpenAIUnexpectedEventReviewer{
				APIKey: key, Model: model, Endpoint: os.Getenv("LIVEGUARD_MONITOR_ENDPOINT"), Client: agent.DefaultHTTPClient(),
				Breaker: agent.NewCircuitBreaker(2, 30*time.Second),
			}, interval)
			log.Printf("unexpected livestream event review enabled with OpenAI model %s (max once per %s per monitor)", model, interval)
		} else {
			log.Print("OpenAI event review disabled; OPENAI_API_KEY is not configured")
		}
	default:
		log.Printf("unexpected livestream event review disabled; unsupported provider %q", monitorProvider)
	}
	if monitorProvider == "minimax" && !strings.EqualFold(envOr("LIVEGUARD_ASR_MODE", "auto"), "off") {
		if key := strings.TrimSpace(os.Getenv("MINIMAX_API_KEY")); key != "" {
			monitorService.SetTranscriber(monitor.MiniMaxTranscriber{
				APIKey: key, Model: envOr("LIVEGUARD_ASR_MODEL", "asr-1.0"),
				Endpoint: envOr("LIVEGUARD_ASR_ENDPOINT", "https://api.minimaxi.com/v1/speech_to_text"),
				Language: envOr("LIVEGUARD_ASR_LANGUAGE", "zh"), FFmpegPath: envOr("LIVEGUARD_FFMPEG_BIN", "ffmpeg"),
				Client: agent.DefaultHTTPClient(),
			})
			log.Print("MiniMax ASR enabled; audio is processed only after the user shares live audio from a monitor card")
		}
	}
	liveCapturer := &browser.LiveCapturer{
		ChromePath: chromePath, ProfileDir: filepath.Join("runtime", "live-profile"),
		AllowedHosts: allowedHosts(), Warmup: envDuration("LIVEGUARD_LIVE_CAPTURE_WARMUP", 7*time.Second),
	}
	webServer.SetBrowserSession(liveCapturer.OpenLoginWindow, liveCapturer.SessionState)
	defer liveCapturer.Shutdown()
	monitorService.SetFrameCapturer(liveCapturer)
	if strings.TrimSpace(os.Getenv("LIVEGUARD_LIVE_CAPTURE_INTERVAL")) != "" {
		monitorService.SetCaptureInterval(envDuration("LIVEGUARD_LIVE_CAPTURE_INTERVAL", 12*time.Second))
	}
	if binary := strings.TrimSpace(os.Getenv("LIVEGUARD_TESSERACT_BIN")); binary != "" {
		monitorService.SetTextRecognizer(monitor.TesseractRecognizer{
			Binary: binary, Language: envOr("LIVEGUARD_OCR_LANGUAGE", "chi_sim+eng"), Timeout: 8 * time.Second,
		})
		log.Printf("local OCR enabled with %s", binary)
	} else {
		log.Print("local OCR disabled; set LIVEGUARD_TESSERACT_BIN to enable image signals")
	}
	if err := monitorService.RecoverInterrupted(ctx); err != nil {
		log.Fatalf("recover monitor runs: %v", err)
	}
	defer monitorService.StopAll()
	webServer.SetMonitorService(monitorService)
	log.Print("personal livestream monitor ready")
	server := &http.Server{Addr: envOr("LIVEGUARD_ADDR", "127.0.0.1:8080"), Handler: webServer.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("LiveGuard control plane listening on http://%s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

// normalizeLocalProxyEnv repairs only the stale loopback ports this project
// has encountered. Other proxy values are left untouched.
func normalizeLocalProxyEnv() {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "http://127.0.0.1:7890" || value == "http://127.0.0.1:7890/" || value == "http://127.0.0.1:9" || value == "http://127.0.0.1:9/" {
			_ = os.Setenv(name, "http://127.0.0.1:7897")
		}
	}
}

func allowedHosts() map[string]bool {
	raw := envOr("LIVEGUARD_ALLOWED_HOSTS", "douyin.com,www.douyin.com,localhost,127.0.0.1")
	result := map[string]bool{}
	for _, host := range strings.Split(raw, ",") {
		if host = strings.TrimSpace(strings.ToLower(host)); host != "" {
			result[host] = true
		}
	}
	return result
}

func defaultChromePath() string {
	if runtime.GOOS == "windows" {
		for _, candidate := range []string{`C:\Program Files\Google\Chrome\Application\chrome.exe`, `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return "google-chrome"
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		log.Printf("invalid %s=%q; using %s", key, value, fallback)
		return fallback
	}
	return duration
}

func loadEnvFiles(paths ...string) {
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, found := strings.Cut(line, "=")
			if !found || os.Getenv(strings.TrimSpace(key)) != "" {
				continue
			}
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			_ = os.Setenv(strings.TrimSpace(key), value)
		}
		_ = file.Close()
	}
}
