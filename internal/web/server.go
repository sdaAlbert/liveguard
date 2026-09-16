package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"liveguard/internal/agent"
	"liveguard/internal/domain"
	"liveguard/internal/sandbox"
	"liveguard/internal/store"
	"liveguard/internal/worker"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	store       *store.Store
	worker      *worker.Worker
	sandboxLab  SandboxService
	artifactDir string
	evalRunner  func(context.Context) agent.EvalReport
	lastEval    *agent.EvalReport
	mu          sync.Mutex
	subscribers map[chan domain.Event]struct{}
}

type SandboxService interface {
	List() []*sandbox.Run
	InfraStatus() sandbox.InfraStatus
	SubmitWithKey(scenarioName, suiteID, idempotencyKey string) (*sandbox.Run, error)
	SubmitSuiteWithKey(idempotencyKey string) ([]*sandbox.Run, error)
}

func New(s *store.Store, artifactDir string) *Server {
	return &Server{store: s, artifactDir: artifactDir, subscribers: map[chan domain.Event]struct{}{}}
}

func (s *Server) SetWorker(w *worker.Worker) { s.worker = w }

func (s *Server) SetSandboxLab(lab SandboxService) { s.sandboxLab = lab }

func (s *Server) SetAgentEval(runner func(context.Context) agent.EvalReport) { s.evalRunner = runner }

func (s *Server) Publish(event domain.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(assets, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.Handle("/artifacts/", http.StripPrefix("/artifacts/", http.FileServer(http.Dir(s.artifactDir))))
	mux.HandleFunc("/api/tasks", s.tasks)
	mux.HandleFunc("/api/tasks/", s.task)
	mux.HandleFunc("/api/events", s.events)
	mux.HandleFunc("/api/sandbox/runs", s.sandboxRuns)
	mux.HandleFunc("/api/sandbox/suite", s.sandboxSuite)
	mux.HandleFunc("/api/infra/status", s.infraStatus)
	mux.HandleFunc("/api/evals/agent", s.agentEval)
	mux.HandleFunc("/demo/live", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/demo-live.html")
	})
	mux.HandleFunc("/demo/live-broken", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/demo-live-broken.html")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, assets, "static/index.html")
	})
	return securityHeaders(otelhttp.NewHandler(mux, "liveguard.http"))
}

func (s *Server) agentEval(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		last := s.lastEval
		s.mu.Unlock()
		if last == nil {
			writeError(w, http.StatusNotFound, "尚未运行 Agent Eval")
			return
		}
		writeJSON(w, http.StatusOK, last)
	case http.MethodPost:
		if s.evalRunner == nil {
			writeError(w, http.StatusServiceUnavailable, "Agent Eval 未启用")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		report := s.evalRunner(ctx)
		s.mu.Lock()
		s.lastEval = &report
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, report)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) infraStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.sandboxLab == nil {
		writeError(w, http.StatusServiceUnavailable, "Sandbox Lab 未启用")
		return
	}
	writeJSON(w, http.StatusOK, s.sandboxLab.InfraStatus())
}

func (s *Server) sandboxRuns(w http.ResponseWriter, r *http.Request) {
	if s.sandboxLab == nil {
		writeError(w, http.StatusServiceUnavailable, "Sandbox Lab 未启用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.sandboxLab.List())
	case http.MethodPost:
		var input struct {
			Scenario string `json:"scenario"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请求格式无效")
			return
		}
		run, err := s.sandboxLab.SubmitWithKey(strings.TrimSpace(input.Scenario), "", strings.TrimSpace(r.Header.Get("Idempotency-Key")))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) sandboxSuite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.sandboxLab == nil {
		writeError(w, http.StatusServiceUnavailable, "Sandbox Lab 未启用")
		return
	}
	runs, err := s.sandboxLab.SubmitSuiteWithKey(strings.TrimSpace(r.Header.Get("Idempotency-Key")))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, runs)
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.store.List())
	case http.MethodPost:
		var input struct {
			URL       string `json:"url"`
			Objective string `json:"objective"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请求格式无效")
			return
		}
		input.URL, input.Objective = strings.TrimSpace(input.URL), strings.TrimSpace(input.Objective)
		parsed, err := url.Parse(input.URL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			writeError(w, http.StatusBadRequest, "请输入有效的 http/https 地址")
			return
		}
		if len([]rune(input.Objective)) < 4 || len([]rune(input.Objective)) > 500 {
			writeError(w, http.StatusBadRequest, "巡检目标应为 4-500 个字符")
			return
		}
		now := time.Now().UTC()
		carrier := propagation.MapCarrier{}
		otel.GetTextMapPropagator().Inject(r.Context(), carrier)
		traceID := ""
		if spanContext := trace.SpanContextFromContext(r.Context()); spanContext.IsValid() {
			traceID = spanContext.TraceID().String()
		}
		task := &domain.Task{ID: domain.NewID("task"), URL: input.URL, Objective: input.Objective, Status: domain.StatusQueued, CreatedAt: now, UpdatedAt: now, Version: 1, TraceID: traceID, TraceParent: carrier.Get("traceparent"), TraceState: carrier.Get("tracestate")}
		task.AddEvent("created", "任务已创建并进入执行队列")
		if err := s.store.Create(task); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Publish(task.Events[len(task.Events)-1])
		if s.worker != nil {
			s.worker.Notify()
		}
		writeJSON(w, http.StatusCreated, task)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	remainder := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		task, err := s.store.Update(id, func(t *domain.Task) error {
			if t.Status == domain.StatusCompleted || t.Status == domain.StatusFailed {
				return errors.New("已结束的任务不能取消")
			}
			t.Status = domain.StatusCancelled
			t.AddEvent("cancelled", "用户取消了任务")
			return nil
		})
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if s.worker != nil {
			s.worker.Cancel(id)
		}
		writeJSON(w, http.StatusOK, task)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	task, err := s.store.Get(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan domain.Event, 8)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscribers, ch); s.mu.Unlock() }()
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: update\ndata: %s\n\n", data)
			flusher.Flush()
		case <-time.After(15 * time.Second):
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
