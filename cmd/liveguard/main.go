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
	"strconv"
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
	closeRuntimeLog := configureRuntimeLogging()
	defer closeRuntimeLog()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	exitSignals := make(chan os.Signal, 1)
	signal.Notify(exitSignals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(exitSignals)
	go func() {
		select {
		case received := <-exitSignals:
			log.Printf("LiveGuard shutdown requested: source=os_signal signal=%q", received.String())
			stop()
		case <-ctx.Done():
		}
	}()
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
	infraMode := strings.ToLower(envOr("LIVEGUARD_INFRA_MODE", "local"))
	chromePath := envOr("LIVEGUARD_CHROME_PATH", defaultChromePath())
	webServer := webapp.New("artifacts")
	webServer.SetShutdownHandler(func() {
		log.Print("LiveGuard shutdown requested: source=local_shutdown_api")
		stop()
	})
	var monitorRepository monitor.Repository
	if infraMode == "memory" {
		monitorRepository = monitor.NewMemoryRepository()
	} else if infraMode == "local" {
		localPath := envOr("LIVEGUARD_LOCAL_STATE_PATH", filepath.Join("runtime", "liveguard.json"))
		localRepository, err := monitor.OpenFileRepository(localPath)
		if err != nil {
			log.Fatalf("local monitor store: %v", err)
		}
		defer func() {
			if err := localRepository.Close(); err != nil {
				log.Printf("save local monitor state: %v", err)
			}
		}()
		monitorRepository = localRepository
		log.Printf("local monitor state persisted to %s", localPath)
	} else {
		log.Fatalf("unsupported LIVEGUARD_INFRA_MODE %q; use local or memory", infraMode)
	}
	monitorService := monitor.NewService(monitorRepository, func() {
		webServer.Publish(domain.Event{ID: domain.NewID("event"), Type: "monitor_update", Message: "monitor state updated", At: time.Now().UTC()})
	})
	monitorService.SetFrameArchiveDir(envOr("LIVEGUARD_FRAME_ARCHIVE_DIR", filepath.Join("runtime", "frames")))
	monitorLLMMode := strings.ToLower(envOr("LIVEGUARD_MONITOR_LLM_MODE", "deterministic"))
	monitorProvider := strings.ToLower(envOr("LIVEGUARD_MONITOR_PROVIDER", "minimax"))
	interval := envDuration("LIVEGUARD_MONITOR_REVIEW_INTERVAL", 15*time.Second)
	initialModelSettings := monitor.ModelSettingsInput{Provider: "rules"}
	if raw := strings.TrimSpace(os.Getenv("LIVEGUARD_MONITOR_SUPPORTS_VISION")); raw != "" {
		if vision, err := strconv.ParseBool(raw); err == nil {
			initialModelSettings.SupportsVision = &vision
		} else {
			log.Print("invalid LIVEGUARD_MONITOR_SUPPORTS_VISION; using declared default model capabilities")
		}
	}
	if monitorLLMMode == "auto" {
		initialModelSettings.Provider = monitorProvider
		initialModelSettings.Model = envOr("LIVEGUARD_MONITOR_MODEL", envOr("LIVEGUARD_MODEL", ""))
		initialModelSettings.Endpoint = strings.TrimSpace(os.Getenv("LIVEGUARD_MONITOR_ENDPOINT"))
		if monitorProvider == "minimax" {
			initialModelSettings.APIKey = strings.TrimSpace(os.Getenv("MINIMAX_API_KEY"))
		} else if monitorProvider == "openai" {
			initialModelSettings.APIKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		} else if monitorProvider == "deepseek" {
			initialModelSettings.APIKey = strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
		}
	}
	if _, modelErr := monitorService.ConfigureModel(initialModelSettings); modelErr != nil {
		log.Printf("model configuration unavailable; using local rules: %v", modelErr)
		_, _ = monitorService.ConfigureModel(monitor.ModelSettingsInput{Provider: "rules"})
	} else if initialModelSettings.Provider == "rules" {
		log.Print("unexpected livestream event review disabled; configure a model from the local settings window")
	} else {
		log.Printf("%s livestream model configured (minimum review interval %s)", initialModelSettings.Provider, interval)
	}
	monitorService.SetUnexpectedEventReviewerInterval(interval)
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
		monitorService.SetCaptureInterval(envDuration("LIVEGUARD_LIVE_CAPTURE_INTERVAL", 6*time.Second))
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

func allowedHosts() map[string]bool {
	// Empty means any valid HTTP(S) target. Deployments that need a tighter
	// browser navigation boundary can provide a comma-separated host allowlist.
	raw := strings.TrimSpace(os.Getenv("LIVEGUARD_ALLOWED_HOSTS"))
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
