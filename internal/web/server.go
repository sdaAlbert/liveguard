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
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"liveguard/internal/agent"
	"liveguard/internal/browser"
	"liveguard/internal/domain"
	"liveguard/internal/monitor"
	"liveguard/internal/sandbox"
	"liveguard/internal/store"
	"liveguard/internal/worker"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	store               store.TaskRepository
	worker              *worker.Worker
	sandboxLab          SandboxService
	monitorService      MonitorService
	artifactDir         string
	evalRunner          func(context.Context) agent.EvalReport
	openBrowserSession  func(string) error
	browserSessionState func() browser.SessionState
	lastEval            *agent.EvalReport
	mu                  sync.Mutex
	subscribers         map[chan domain.Event]struct{}
}

type SandboxService interface {
	List() []*sandbox.Run
	InfraStatus() sandbox.InfraStatus
	SubmitWithKey(scenarioName, suiteID, idempotencyKey string) (*sandbox.Run, error)
	SubmitSuiteWithKey(idempotencyKey string) ([]*sandbox.Run, error)
}

type MonitorService interface {
	Start(context.Context, monitor.StartInput) (*monitor.Run, error)
	List(context.Context) ([]monitor.Summary, error)
	Get(context.Context, string) (*monitor.Run, error)
	Stop(context.Context, string) (*monitor.Run, error)
}

func New(s store.TaskRepository, artifactDir string) *Server {
	return &Server{store: s, artifactDir: artifactDir, subscribers: map[chan domain.Event]struct{}{}}
}

func (s *Server) SetWorker(w *worker.Worker) { s.worker = w }

func (s *Server) SetSandboxLab(lab SandboxService) { s.sandboxLab = lab }

func (s *Server) SetMonitorService(service MonitorService) { s.monitorService = service }

func (s *Server) SetAgentEval(runner func(context.Context) agent.EvalReport) { s.evalRunner = runner }

func (s *Server) SetBrowserSession(open func(string) error, state func() browser.SessionState) {
	s.openBrowserSession = open
	s.browserSessionState = state
}

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
	mux.HandleFunc("/api/campaigns", s.campaigns)
	mux.HandleFunc("/api/campaigns/", s.campaign)
	mux.HandleFunc("/api/monitors", s.monitors)
	mux.HandleFunc("/api/monitors/", s.monitorRun)
	mux.HandleFunc("/api/events", s.events)
	mux.HandleFunc("/api/sandbox/runs", s.sandboxRuns)
	mux.HandleFunc("/api/sandbox/suite", s.sandboxSuite)
	mux.HandleFunc("/api/infra/status", s.infraStatus)
	mux.HandleFunc("/api/evals/agent", s.agentEval)
	mux.HandleFunc("/api/browser/session", s.browserSession)
	mux.HandleFunc("/demo/live", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/demo-live.html")
	})
	mux.HandleFunc("/demo/live-broken", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/demo-live-broken.html")
	})
	mux.HandleFunc("/monitor", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/monitor.html")
	})
	mux.HandleFunc("/inspect", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/index.html")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, assets, "static/monitor.html")
	})
	return securityHeaders(otelhttp.NewHandler(mux, "liveguard.http"))
}

func (s *Server) monitors(w http.ResponseWriter, r *http.Request) {
	if s.monitorService == nil {
		writeError(w, http.StatusServiceUnavailable, "直播信号监控未启用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		runs, err := s.monitorService.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, runs)
	case http.MethodPost:
		var input monitor.StartInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请输入直播地址和关注需求")
			return
		}
		input.TargetURL = strings.TrimSpace(input.TargetURL)
		input.Goal = strings.TrimSpace(input.Goal)
		parsed, parseErr := url.Parse(input.TargetURL)
		if parseErr != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			writeError(w, http.StatusBadRequest, "请输入有效的直播 http/https 地址")
			return
		}
		goalLength := len([]rune(input.Goal))
		if goalLength < 2 || goalLength > 240 {
			writeError(w, http.StatusBadRequest, "关注需求应为 2-240 个字符")
			return
		}
		run, err := s.monitorService.Start(r.Context(), input)
		if err != nil {
			if errors.Is(err, monitor.ErrCapacity) {
				writeError(w, http.StatusConflict, "最多同时打开 8 个监控窗口，请先停止一个")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) monitorRun(w http.ResponseWriter, r *http.Request) {
	if s.monitorService == nil {
		writeError(w, http.StatusServiceUnavailable, "直播信号监控未启用")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/monitors/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
		run, err := s.monitorService.Stop(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}
	if len(parts) != 1 || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	run, err := s.monitorService.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, monitor.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
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
			URL                     string   `json:"url"`
			Objective               string   `json:"objective"`
			ExpectedTexts           []string `json:"expected_texts"`
			ExpectedLiveStatus      string   `json:"expected_live_status"`
			UseAuthenticatedSession bool     `json:"use_authenticated_session"`
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
		expectedTexts, err := validateExpectedTexts(input.ExpectedTexts)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		input.ExpectedLiveStatus = strings.ToLower(strings.TrimSpace(input.ExpectedLiveStatus))
		if input.ExpectedLiveStatus != "" && input.ExpectedLiveStatus != "any" && input.ExpectedLiveStatus != "live" && input.ExpectedLiveStatus != "offline" {
			writeError(w, http.StatusBadRequest, "直播状态预期仅支持 any、live 或 offline")
			return
		}
		if input.ExpectedLiveStatus == "any" {
			input.ExpectedLiveStatus = ""
		}
		if input.UseAuthenticatedSession && s.browserSessionState != nil {
			state := s.browserSessionState()
			if state.Open {
				writeError(w, http.StatusConflict, "请先关闭抖音登录窗口，再开始巡检")
				return
			}
			if state.Inspecting {
				writeError(w, http.StatusConflict, "另一个登录态巡检正在执行，请稍候")
				return
			}
		}
		task, err := s.createTask(r.Context(), &domain.Task{URL: input.URL, Objective: input.Objective, ExpectedTexts: expectedTexts, ExpectedLiveStatus: input.ExpectedLiveStatus, UseAuthenticatedSession: input.UseAuthenticatedSession}, "任务已创建并进入执行队列")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
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
	if len(parts) == 2 && parts[1] == "retry" && r.Method == http.MethodPost {
		original, err := s.store.Get(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if original.Status == domain.StatusQueued || original.Status == domain.StatusPlanning || original.Status == domain.StatusRunning {
			writeError(w, http.StatusConflict, "任务仍在执行，结束后才能复测")
			return
		}
		clone := &domain.Task{CampaignID: original.CampaignID, CampaignName: original.CampaignName, URL: original.URL, Objective: original.Objective, ExpectedTexts: append([]string(nil), original.ExpectedTexts...), ExpectedLiveStatus: original.ExpectedLiveStatus, UseAuthenticatedSession: original.UseAuthenticatedSession, ParentTaskID: original.ID, MaxAttempts: 3}
		retry, err := s.createTask(r.Context(), clone, "复测任务已创建，来源："+original.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, retry)
		return
	}
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

type campaignSummary struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Total     int            `json:"total"`
	Queued    int            `json:"queued"`
	Running   int            `json:"running"`
	Passed    int            `json:"passed"`
	Attention int            `json:"attention"`
	Retrying  int            `json:"retrying"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Tasks     []*domain.Task `json:"tasks,omitempty"`
}

func (s *Server) campaigns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, summarizeCampaigns(s.store.List(), false))
	case http.MethodPost:
		var input struct {
			Name                    string   `json:"name"`
			URLs                    []string `json:"urls"`
			ExpectedTexts           []string `json:"expected_texts"`
			ExpectedLiveStatus      string   `json:"expected_live_status"`
			UseAuthenticatedSession bool     `json:"use_authenticated_session"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请求格式无效")
			return
		}
		urls, err := validateCampaignURLs(input.URLs)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		expectedTexts, err := validateExpectedTexts(input.ExpectedTexts)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		liveStatus, err := validateLiveStatus(input.ExpectedLiveStatus)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.validateBrowserAvailability(input.UseAuthenticatedSession); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		name := strings.TrimSpace(input.Name)
		if name == "" {
			name = "直播巡检 " + time.Now().Format("01-02 15:04")
		}
		if len([]rune(name)) > 80 {
			writeError(w, http.StatusBadRequest, "批次名称不能超过 80 个字符")
			return
		}
		campaignID := domain.NewID("campaign")
		tasks := make([]*domain.Task, 0, len(urls))
		for _, targetURL := range urls {
			task := &domain.Task{CampaignID: campaignID, CampaignName: name, URL: targetURL, Objective: "批量检查直播间是否可以正常访问，识别直播状态，并验证指定页面内容。", ExpectedTexts: append([]string(nil), expectedTexts...), ExpectedLiveStatus: liveStatus, UseAuthenticatedSession: input.UseAuthenticatedSession, MaxAttempts: 3}
			s.prepareTask(r.Context(), task, "批次任务已创建并进入执行队列")
			tasks = append(tasks, task)
		}
		if err := s.store.CreateMany(tasks); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, task := range tasks {
			s.Publish(task.Events[len(task.Events)-1])
		}
		if s.worker != nil {
			s.worker.Notify()
		}
		summaries := summarizeCampaigns(tasks, true)
		writeJSON(w, http.StatusCreated, summaries[0])
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) campaign(w http.ResponseWriter, r *http.Request) {
	remainder := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/campaigns/"), "/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	matching := campaignTasks(s.store.List(), id)
	if len(matching) == 0 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "retry-failed" && r.Method == http.MethodPost {
		var retries []*domain.Task
		for _, original := range latestCampaignTasks(matching) {
			if !needsAttention(original) {
				continue
			}
			clone := &domain.Task{CampaignID: original.CampaignID, CampaignName: original.CampaignName, URL: original.URL, Objective: original.Objective, ExpectedTexts: append([]string(nil), original.ExpectedTexts...), ExpectedLiveStatus: original.ExpectedLiveStatus, UseAuthenticatedSession: original.UseAuthenticatedSession, ParentTaskID: original.ID, MaxAttempts: 3}
			s.prepareTask(r.Context(), clone, "异常任务已重新进入批次队列")
			retries = append(retries, clone)
		}
		if len(retries) == 0 {
			writeError(w, http.StatusConflict, "当前批次没有需要复测的任务")
			return
		}
		if err := s.store.CreateMany(retries); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, task := range retries {
			s.Publish(task.Events[len(task.Events)-1])
		}
		if s.worker != nil {
			s.worker.Notify()
		}
		writeJSON(w, http.StatusCreated, map[string]any{"campaign_id": id, "created": len(retries)})
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, summarizeCampaigns(matching, true)[0])
}

func validateCampaignURLs(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > 50 {
		return nil, errors.New("每个批次需要 1-50 个直播间地址")
	}
	seen := map[string]bool{}
	urls := make([]string, 0, len(input))
	for _, raw := range input {
		raw = strings.TrimSpace(raw)
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("无效的直播间地址：%s", raw)
		}
		if !seen[raw] {
			seen[raw] = true
			urls = append(urls, raw)
		}
	}
	return urls, nil
}

func validateLiveStatus(input string) (string, error) {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" || input == "any" {
		return "", nil
	}
	if input != "live" && input != "offline" {
		return "", errors.New("直播状态预期仅支持 any、live 或 offline")
	}
	return input, nil
}

func (s *Server) validateBrowserAvailability(useAuthenticated bool) error {
	if !useAuthenticated || s.browserSessionState == nil {
		return nil
	}
	state := s.browserSessionState()
	if state.Open {
		return errors.New("请先关闭抖音登录窗口，再开始巡检")
	}
	if state.Inspecting {
		return errors.New("另一个登录态巡检正在执行，请稍候")
	}
	return nil
}

func campaignTasks(tasks []*domain.Task, id string) []*domain.Task {
	var result []*domain.Task
	for _, task := range tasks {
		if task.CampaignID == id {
			result = append(result, task)
		}
	}
	return result
}

// latestCampaignTasks collapses manual retries so that a campaign continues to
// represent rooms, rather than every historical attempt for those rooms.
func latestCampaignTasks(tasks []*domain.Task) []*domain.Task {
	byURL := map[string]*domain.Task{}
	for _, task := range tasks {
		current := byURL[task.URL]
		if current == nil || task.CreatedAt.After(current.CreatedAt) {
			byURL[task.URL] = task
		}
	}
	result := make([]*domain.Task, 0, len(byURL))
	for _, task := range byURL {
		result = append(result, task)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

func summarizeCampaigns(tasks []*domain.Task, includeTasks bool) []campaignSummary {
	grouped := map[string][]*domain.Task{}
	for _, task := range tasks {
		if task.CampaignID == "" {
			continue
		}
		grouped[task.CampaignID] = append(grouped[task.CampaignID], task)
	}
	byID := map[string]*campaignSummary{}
	for id, history := range grouped {
		latest := latestCampaignTasks(history)
		if len(latest) == 0 {
			continue
		}
		first := latest[0]
		summary := &campaignSummary{ID: id, Name: first.CampaignName, CreatedAt: first.CreatedAt, UpdatedAt: first.UpdatedAt}
		byID[id] = summary
		for _, task := range latest {
			summary.Total++
			if task.CreatedAt.Before(summary.CreatedAt) {
				summary.CreatedAt = task.CreatedAt
			}
			if task.UpdatedAt.After(summary.UpdatedAt) {
				summary.UpdatedAt = task.UpdatedAt
			}
			switch task.Status {
			case domain.StatusQueued:
				summary.Queued++
				if task.Error != "" {
					summary.Retrying++
				}
			case domain.StatusPlanning, domain.StatusRunning:
				summary.Running++
			}
			if task.Report != nil && task.Report.Verdict == "passed" {
				summary.Passed++
			}
			if needsAttention(task) {
				summary.Attention++
			}
			if includeTasks {
				summary.Tasks = append(summary.Tasks, task)
			}
		}
	}
	result := make([]campaignSummary, 0, len(byID))
	for _, summary := range byID {
		result = append(result, *summary)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

func needsAttention(task *domain.Task) bool {
	if task.Status == domain.StatusFailed || task.Status == domain.StatusNeedsHuman {
		return true
	}
	return task.Report != nil && (task.Report.Verdict == "failed" || task.Report.Verdict == "unverified" || task.Report.Verdict == "needs_human")
}

func (s *Server) browserSession(w http.ResponseWriter, r *http.Request) {
	if s.browserSessionState == nil || s.openBrowserSession == nil {
		writeError(w, http.StatusServiceUnavailable, "专用浏览器会话未启用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.browserSessionState())
	case http.MethodPost:
		var input struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请求格式无效")
			return
		}
		if input.URL = strings.TrimSpace(input.URL); input.URL == "" {
			input.URL = "https://www.douyin.com/"
		}
		if err := s.openBrowserSession(input.URL); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, s.browserSessionState())
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) createTask(ctx context.Context, task *domain.Task, eventMessage string) (*domain.Task, error) {
	s.prepareTask(ctx, task, eventMessage)
	if err := s.store.Create(task); err != nil {
		return nil, err
	}
	s.Publish(task.Events[len(task.Events)-1])
	if s.worker != nil {
		s.worker.Notify()
	}
	return task, nil
}

func (s *Server) prepareTask(ctx context.Context, task *domain.Task, eventMessage string) {
	now := time.Now().UTC()
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	traceID := ""
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		traceID = spanContext.TraceID().String()
	}
	task.ID = domain.NewID("task")
	task.Status = domain.StatusQueued
	if task.MaxAttempts <= 0 {
		task.MaxAttempts = 3
	}
	task.CreatedAt, task.UpdatedAt, task.Version = now, now, 1
	task.TraceID, task.TraceParent, task.TraceState = traceID, carrier.Get("traceparent"), carrier.Get("tracestate")
	task.AddEvent("created", eventMessage)
}

func validateExpectedTexts(input []string) ([]string, error) {
	if len(input) > 5 {
		return nil, errors.New("最多设置 5 条预期文案")
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(input))
	for _, text := range input {
		text = strings.TrimSpace(text)
		if text == "" || seen[text] {
			continue
		}
		if len([]rune(text)) > 80 {
			return nil, errors.New("每条预期文案不能超过 80 个字符")
		}
		seen[text] = true
		result = append(result, text)
	}
	return result, nil
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
