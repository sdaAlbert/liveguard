package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"liveguard/internal/agent"
	"liveguard/internal/browser"
	"liveguard/internal/domain"
	"liveguard/internal/sandbox"
	"liveguard/internal/store"
)

type Worker struct {
	store   *store.Store
	planner agent.Planner
	browser *browser.Runner
	tools   ToolRunner
	decider agent.ToolDecider
	policy  agent.ToolPolicy
	wake    chan struct{}
	mu      sync.Mutex
	running map[string]context.CancelFunc
	onEvent func(domain.Event)
}

type ToolRunner interface {
	RunTool(context.Context, sandbox.ToolInput, string) (*sandbox.Run, error)
}

func New(s *store.Store, planner agent.Planner, browserRunner *browser.Runner, tools ToolRunner, onEvent func(domain.Event)) *Worker {
	return &Worker{store: s, planner: planner, browser: browserRunner, tools: tools, decider: agent.DeterministicToolDecider{}, policy: agent.ToolPolicy{}, wake: make(chan struct{}, 1), running: map[string]context.CancelFunc{}, onEvent: onEvent}
}

func (w *Worker) SetToolDecider(decider agent.ToolDecider) {
	if decider != nil {
		w.decider = decider
	}
}

func (w *Worker) Start(ctx context.Context) {
	w.Notify()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
			w.dispatch(ctx)
		case <-time.After(2 * time.Second):
			w.dispatch(ctx)
		}
	}
}

func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) Cancel(id string) {
	w.mu.Lock()
	cancel := w.running[id]
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *Worker) dispatch(parent context.Context) {
	for _, task := range w.store.List() {
		if task.Status != domain.StatusQueued {
			continue
		}
		w.mu.Lock()
		_, exists := w.running[task.ID]
		if exists {
			w.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithTimeout(parent, 75*time.Second)
		w.running[task.ID] = cancel
		w.mu.Unlock()
		go w.run(ctx, task.ID)
	}
}

func (w *Worker) run(ctx context.Context, id string) {
	defer func() {
		w.mu.Lock()
		if cancel := w.running[id]; cancel != nil {
			cancel()
		}
		delete(w.running, id)
		w.mu.Unlock()
	}()
	task, err := w.store.Get(id)
	if err != nil {
		return
	}
	ctx = sandbox.ExtractTrace(ctx, task.TraceParent, task.TraceState)
	ctx, runSpan := otel.Tracer("liveguard/agent").Start(ctx, "agent.run")
	runSpan.SetAttributes(attribute.String("task.id", id), attribute.String("task.url", task.URL))
	defer runSpan.End()
	w.transition(id, domain.StatusPlanning, "planning", "正在把巡检目标转换为受约束的检查计划")
	planCtx, planSpan := otel.Tracer("liveguard/agent").Start(ctx, "agent.plan")
	plan, plannerSource, err := w.planner.Plan(planCtx, task.URL, task.Objective)
	planSpan.End()
	if err != nil {
		w.fail(id, err)
		return
	}
	_, err = w.store.Update(id, func(t *domain.Task) error {
		if t.Status == domain.StatusCancelled {
			return errors.New("task cancelled")
		}
		t.Plan = plan
		t.Status = domain.StatusRunning
		t.AddEvent("plan", fmt.Sprintf("已生成 %d 项检查；规划器：%s", len(plan), plannerSource))
		return nil
	})
	if err != nil {
		return
	}
	w.publishLatest(id)
	w.addEvent(id, "browser", "正在启动独立 Chrome 会话并访问目标页面")
	browserCtx, browserSpan := otel.Tracer("liveguard/agent").Start(ctx, "browser.inspect")
	page, err := w.browser.Inspect(browserCtx, id, task.URL)
	browserSpan.End()
	if err != nil {
		w.fail(id, fmt.Errorf("browser inspection: %w", err))
		return
	}
	w.addEvent(id, "evidence", "页面读取完成，截图证据已保存")
	checks, needsHuman := w.browser.Evaluate(plan, page)
	status := domain.StatusCompleted
	summary := "巡检完成；请结合每项证据判断是否可以继续上线流程"
	var toolCalls []domain.ToolCall
	decisionInput := agent.DecisionInput{Objective: task.Objective, PageTitle: page.Title, PageText: page.BodyText, Checks: checks, NeedsHuman: needsHuman}
	decisionCtx, decisionSpan := otel.Tracer("liveguard/agent").Start(ctx, "agent.decide_tool")
	decisionResult, decisionErr := w.decider.Decide(decisionCtx, decisionInput)
	if decisionErr != nil {
		decisionSpan.RecordError(decisionErr)
		decisionSpan.End()
		w.fail(id, fmt.Errorf("agent tool decision: %w", decisionErr))
		return
	}
	policyResult := w.policy.Evaluate(decisionInput, decisionResult.Decision)
	decisionResult.Audit.Approved = policyResult.Approved
	decisionResult.Audit.PolicyReason = policyResult.Reason
	decisionSpan.SetAttributes(
		attribute.String("agent.decision.source", decisionResult.Audit.Source),
		attribute.Bool("agent.policy.approved", policyResult.Approved),
		attribute.String("agent.policy.reason", policyResult.Reason),
	)
	if decisionResult.Decision != nil {
		decisionSpan.SetAttributes(attribute.String("tool.name", decisionResult.Decision.Name), attribute.String("tool.reason", decisionResult.Decision.Reason))
	}
	decisionSpan.End()
	if decisionResult.Audit.ModelError != "" {
		w.addEvent(id, "agent_fallback", "模型工具决策不可用，已安全降级到确定性决策")
	}
	decision := decisionResult.Decision
	if decision == nil {
		w.addEvent(id, "agent_decision", "Agent 未请求工具；来源："+decisionResult.Audit.Source)
	} else if !policyResult.Approved {
		w.addEvent(id, "agent_policy", "Policy Gateway 拒绝工具 "+decision.Name+"；原因："+policyResult.Reason)
	} else if w.tools != nil {
		w.addEvent(id, "agent_decision", "Agent 请求工具："+decision.Name+"；来源："+decisionResult.Audit.Source+"；原因："+decision.Reason)
		w.addEvent(id, "tool_queued", "诊断工具已写入 Redis Stream，等待 Sandbox Worker 执行")
		toolRun, toolErr := w.tools.RunTool(ctx, sandbox.ToolInput{Title: page.Title, TextSnippet: page.BodyText, Objective: task.Objective}, "agent:"+id+":"+decision.Name)
		if toolErr != nil {
			w.fail(id, fmt.Errorf("sandbox tool %s: %w", decision.Name, toolErr))
			return
		}
		toolCalls = append(toolCalls, domain.ToolCall{Name: decision.Name, Reason: decision.Reason, RunID: toolRun.ID, Status: toolRun.Status, Output: toolRun.Output, DurationMS: toolRun.DurationMS, Cleaned: toolRun.Cleaned, TraceID: toolRun.TraceID})
		w.addEvent(id, "tool_result", fmt.Sprintf("Sandbox 返回结果：%s，耗时 %d ms，容器已清理：%t", toolRun.Status, toolRun.DurationMS, toolRun.Cleaned))
		if toolRun.Status != "passed" {
			w.fail(id, fmt.Errorf("sandbox tool failed: %s", toolRun.Error))
			return
		}
		checks, summary = agent.ApplyDiagnosis(checks, toolRun.Output)
		w.addEvent(id, "agent_synthesis", "Agent 已结合浏览器证据和 Sandbox 工具结果生成最终结论")
	}
	if needsHuman {
		status = domain.StatusNeedsHuman
		summary = "检测到登录或安全验证，需要人工接管后继续"
	}
	_, err = w.store.Update(id, func(t *domain.Task) error {
		if t.Status == domain.StatusCancelled {
			return errors.New("task cancelled")
		}
		t.Status = status
		t.Report = &domain.Report{Title: page.Title, FinalURL: page.FinalURL, Screenshot: page.Screenshot, Planner: plannerSource, Summary: summary, Checks: checks, ToolCalls: toolCalls, Decision: &decisionResult.Audit, CompletedAt: time.Now().UTC()}
		t.AddEvent("report", summary)
		return nil
	})
	if err == nil {
		w.publishLatest(id)
	}
}

func (w *Worker) transition(id string, status domain.Status, eventType, message string) {
	_, err := w.store.Update(id, func(t *domain.Task) error {
		if t.Status == domain.StatusCancelled {
			return errors.New("task cancelled")
		}
		t.Status = status
		t.Error = ""
		t.AddEvent(eventType, message)
		return nil
	})
	if err == nil {
		w.publishLatest(id)
	}
}

func (w *Worker) addEvent(id, eventType, message string) {
	_, err := w.store.Update(id, func(t *domain.Task) error { t.AddEvent(eventType, message); return nil })
	if err == nil {
		w.publishLatest(id)
	}
}

func (w *Worker) fail(id string, cause error) {
	status := domain.StatusFailed
	message := cause.Error()
	if errors.Is(cause, context.Canceled) {
		status, message = domain.StatusCancelled, "任务已取消"
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		message = "任务超过 75 秒执行预算"
	}
	_, err := w.store.Update(id, func(t *domain.Task) error {
		if t.Status == domain.StatusCancelled {
			return nil
		}
		t.Status = status
		t.Error = message
		t.AddEvent("error", message)
		return nil
	})
	if err == nil {
		w.publishLatest(id)
	}
}

func (w *Worker) publishLatest(id string) {
	if w.onEvent == nil {
		return
	}
	task, err := w.store.Get(id)
	if err == nil && len(task.Events) > 0 {
		w.onEvent(task.Events[len(task.Events)-1])
	}
}
