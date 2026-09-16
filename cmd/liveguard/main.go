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
	"liveguard/internal/sandbox"
	"liveguard/internal/sandboxrpc"
	"liveguard/internal/store"
	"liveguard/internal/telemetry"
	webapp "liveguard/internal/web"
	"liveguard/internal/worker"
)

func main() {
	loadEnvFiles(".env.local", filepath.Join("..", ".env.local"))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTrace, err := telemetry.Setup(ctx, "liveguard-control-plane", envOr("LIVEGUARD_OTEL_ENDPOINT", "127.0.0.1:4317"))
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTrace(shutdownCtx)
	}()
	stateStore, err := store.Open(filepath.Join("state", "tasks.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	if err := stateStore.RecoverInterrupted(); err != nil {
		log.Fatal(err)
	}
	planner := agent.ResilientPlanner{Deterministic: agent.DeterministicPlanner{}}
	toolDecider := agent.ResilientToolDecider{Deterministic: agent.DeterministicToolDecider{}}
	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" && !strings.EqualFold(os.Getenv("LIVEGUARD_LLM_MODE"), "deterministic") {
		model := envOr("LIVEGUARD_MODEL", "gpt-5.6-luna")
		breaker := agent.NewCircuitBreaker(1, 30*time.Second)
		planner.OpenAI = &agent.OpenAIPlanner{APIKey: key, Model: model, Client: agent.DefaultHTTPClient(), Breaker: breaker}
		toolDecider.OpenAI = &agent.OpenAIToolDecider{APIKey: key, Model: model, Client: agent.DefaultHTTPClient(), Breaker: breaker}
		log.Printf("LLM planner enabled with model %s", model)
	} else {
		log.Print("deterministic planner enabled")
	}
	chromePath := envOr("LIVEGUARD_CHROME_PATH", defaultChromePath())
	browserRunner := &browser.Runner{
		ChromePath: chromePath, ProfileDir: filepath.Join("runtime", "chrome-profile"), SessionProfileDir: filepath.Join("runtime", "operator-profile"), ArtifactDir: "artifacts",
		Headless: strings.EqualFold(os.Getenv("LIVEGUARD_HEADLESS"), "true"), AllowedHosts: allowedHosts(),
	}
	webServer := webapp.New(stateStore, "artifacts")
	webServer.SetBrowserSession(browserRunner.OpenSession, browserRunner.SessionState)
	var toolRunner worker.ToolRunner
	if strings.EqualFold(os.Getenv("LIVEGUARD_INFRA_MODE"), "durable") {
		infraCtx, cancelInfra := context.WithTimeout(ctx, 10*time.Second)
		postgresStore, err := sandbox.OpenPostgresRunStore(infraCtx, envOr("LIVEGUARD_DATABASE_URL", "postgres://liveguard:liveguard_local@127.0.0.1:5433/liveguard?sslmode=disable"))
		if err != nil {
			cancelInfra()
			log.Fatalf("durable sandbox postgres: %v", err)
		}
		redisQueue, err := sandbox.OpenRedisStreamQueue(infraCtx, envOr("LIVEGUARD_REDIS_ADDR", "127.0.0.1:6380"), domain.NewID("worker"))
		cancelInfra()
		if err != nil {
			_ = postgresStore.Close()
			log.Fatalf("durable sandbox redis: %v", err)
		}
		defer postgresStore.Close()
		defer redisQueue.Close()
		rpcClient, err := sandboxrpc.Dial(envOr("LIVEGUARD_SANDBOX_RPC_TARGET", "127.0.0.1:9090"), envOr("LIVEGUARD_SANDBOX_IMAGE", "golang:1.26"))
		if err != nil {
			log.Fatalf("sandbox RPC client: %v", err)
		}
		defer rpcClient.Close()
		durableLab := sandbox.NewDurableLab(rpcClient, postgresStore, redisQueue)
		toolRunner = durableLab
		webServer.SetSandboxLab(durableLab)
		go durableLab.Start(ctx)
		log.Print("durable Sandbox Lab enabled with PostgreSQL, Redis Streams and gRPC Sandbox Worker")
	} else {
		sandboxExecutor := sandbox.NewExecutor(envOr("LIVEGUARD_SANDBOX_IMAGE", "golang:1.26"))
		memoryLab := sandbox.NewLab(sandboxExecutor)
		toolRunner = memoryLab
		webServer.SetSandboxLab(memoryLab)
		log.Print("in-memory Sandbox Lab enabled")
	}
	taskWorker := worker.New(stateStore, planner, browserRunner, toolRunner, webServer.Publish)
	taskWorker.SetToolDecider(toolDecider)
	webServer.SetAgentEval(func(evalCtx context.Context) agent.EvalReport {
		return agent.RunEval(evalCtx, toolDecider, agent.ToolPolicy{})
	})
	webServer.SetWorker(taskWorker)
	go taskWorker.Start(ctx)
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
