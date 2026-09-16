package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"liveguard/internal/domain"
)

const (
	ScenarioNormal        = "normal"
	ScenarioTimeout       = "timeout"
	ScenarioWriteDenied   = "write_denied"
	ScenarioNetworkDenied = "network_denied"
	ScenarioDiagnosePage  = "diagnose_live_page"
	maxOutputBytes        = 16 << 10
)

var scenarioOrder = []string{ScenarioNormal, ScenarioTimeout, ScenarioWriteDenied, ScenarioNetworkDenied}

type Policy struct {
	Image      string `json:"image"`
	Memory     string `json:"memory"`
	CPUs       string `json:"cpus"`
	PIDs       int    `json:"pids"`
	Network    string `json:"network"`
	ReadOnly   bool   `json:"read_only"`
	NonRoot    bool   `json:"non_root"`
	CapDropAll bool   `json:"cap_drop_all"`
	TimeoutMS  int64  `json:"timeout_ms"`
}

type InfraStatus struct {
	Mode       string `json:"mode"`
	Postgres   string `json:"postgres"`
	Redis      string `json:"redis"`
	Stream     string `json:"stream"`
	Pending    int64  `json:"pending"`
	Outbox     int64  `json:"outbox"`
	DeadLetter int64  `json:"dead_letter"`
	StoredRuns int64  `json:"stored_runs"`
	Worker     string `json:"worker"`
	RPC        string `json:"rpc"`
}

type Run struct {
	ID              string     `json:"id"`
	IdempotencyKey  string     `json:"idempotency_key,omitempty"`
	RequestHash     string     `json:"request_hash,omitempty"`
	Attempt         int        `json:"attempt"`
	TraceID         string     `json:"trace_id,omitempty"`
	TraceParent     string     `json:"trace_parent,omitempty"`
	TraceState      string     `json:"trace_state,omitempty"`
	SuiteID         string     `json:"suite_id,omitempty"`
	Scenario        string     `json:"scenario"`
	ScenarioLabel   string     `json:"scenario_label"`
	Input           *ToolInput `json:"input,omitempty"`
	Status          string     `json:"status"`
	PolicyVerified  bool       `json:"policy_verified"`
	Policy          Policy     `json:"policy"`
	ContainerName   string     `json:"container_name,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	OOMKilled       bool       `json:"oom_killed"`
	TimedOut        bool       `json:"timed_out"`
	Cleaned         bool       `json:"cleaned"`
	Output          string     `json:"output,omitempty"`
	OutputTruncated bool       `json:"output_truncated"`
	Error           string     `json:"error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	DurationMS      int64      `json:"duration_ms"`
}

type ToolInput struct {
	Title       string `json:"title"`
	TextSnippet string `json:"text_snippet"`
	Objective   string `json:"objective"`
}

type scenario struct {
	label         string
	command       string
	timeout       time.Duration
	expectedExit  *int
	expectTimeout bool
	expectOutput  string
}

func intPtr(value int) *int { return &value }

var scenarios = map[string]scenario{
	ScenarioNormal: {
		label: "正常执行", command: `echo "sandbox ready"; id; echo "result=PASS"`, timeout: 8 * time.Second,
		expectedExit: intPtr(0), expectOutput: "result=PASS",
	},
	ScenarioTimeout: {
		label: "超时终止", command: `echo "long task started"; sleep 30`, timeout: 3 * time.Second,
		expectTimeout: true,
	},
	ScenarioWriteDenied: {
		label: "只读拦截", command: `touch /etc/liveguard-proof`, timeout: 8 * time.Second,
		expectedExit: intPtr(1), expectOutput: "Read-only file system",
	},
	ScenarioNetworkDenied: {
		label: "断网拦截", command: `if getent hosts example.com >/dev/null 2>&1; then echo "NETWORK_OPEN"; exit 9; else echo "NETWORK_BLOCKED"; exit 42; fi`, timeout: 8 * time.Second,
		expectedExit: intPtr(42), expectOutput: "NETWORK_BLOCKED",
	},
	ScenarioDiagnosePage: {
		label: "直播页面诊断", command: `page="$(printf '%s' "$LG_PAGE_B64" | base64 -d)"; echo "tool=diagnose_live_page"; case "$page" in *"活动入口已开启"*|*"活动入口：已开启"*) echo "activity_entry=present";; *) echo "activity_entry=missing"; echo "recommendation=check_activity_component_and_release_switch";; esac; case "$page" in *"直播中"*|*"正在直播"*) echo "live_status=present";; *) echo "live_status=unknown";; esac; echo "diagnosis_complete"`, timeout: 8 * time.Second,
		expectedExit: intPtr(0), expectOutput: "diagnosis_complete",
	},
}

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type OSCommandRunner struct{}

func (OSCommandRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output := &boundedBuffer{limit: maxOutputBytes}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	return output.String(), err
}

type boundedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(input []byte) (int, error) {
	original := len(input)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(input) > remaining {
			input = input[:remaining]
			b.truncated = true
		}
		_, _ = b.Buffer.Write(input)
	} else if len(input) > 0 {
		b.truncated = true
	}
	return original, nil
}

type ExecutionEngine interface {
	Policy(time.Duration) Policy
	Execute(context.Context, *Run) error
}

type HealthChecker interface {
	Health(context.Context) error
}

type Executor struct {
	Image  string
	Docker CommandRunner
}

func NewExecutor(image string) *Executor {
	if strings.TrimSpace(image) == "" {
		image = "golang:1.26"
	}
	return &Executor{Image: image, Docker: OSCommandRunner{}}
}

func (e *Executor) Policy(timeout time.Duration) Policy {
	return Policy{Image: e.Image, Memory: "128 MiB", CPUs: "0.5", PIDs: 32, Network: "none", ReadOnly: true, NonRoot: true, CapDropAll: true, TimeoutMS: timeout.Milliseconds()}
}

func (e *Executor) CreateArgs(containerName, command string) []string {
	return e.createArgs(containerName, command, nil)
}

func (e *Executor) createArgs(containerName, command string, input *ToolInput) []string {
	args := []string{
		"create", "--name", containerName, "--pull", "never",
		"--network", "none", "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
		"--memory", "128m", "--memory-swap", "128m", "--cpus", "0.5", "--pids-limit", "32",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534", "--workdir", "/tmp",
	}
	if input != nil {
		text := input.TextSnippet
		if len(text) > 4096 {
			text = text[:4096]
		}
		args = append(args, "--env", "LG_PAGE_B64="+base64.StdEncoding.EncodeToString([]byte(text)))
	}
	return append(args, e.Image, "/bin/sh", "-c", command)
}

func (e *Executor) createArgsForRun(run *Run, containerName, command string) []string {
	args := e.createArgs(containerName, command, run.Input)
	imageIndex := len(args) - 4
	labels := []string{"--label", "liveguard.run_id=" + run.ID, "--label", fmt.Sprintf("liveguard.attempt=%d", run.Attempt)}
	result := append([]string{}, args[:imageIndex]...)
	result = append(result, labels...)
	return append(result, args[imageIndex:]...)
}

type containerState struct {
	ExitCode  int  `json:"ExitCode"`
	OOMKilled bool `json:"OOMKilled"`
}

func (e *Executor) Execute(ctx context.Context, run *Run) (executeErr error) {
	ctx, span := otel.Tracer("liveguard/sandbox").Start(ctx, "docker.sandbox.execute", trace.WithAttributes(
		attribute.String("sandbox.run_id", run.ID), attribute.String("sandbox.scenario", run.Scenario), attribute.Int("sandbox.attempt", run.Attempt),
	))
	defer func() {
		span.SetAttributes(attribute.Bool("sandbox.cleaned", run.Cleaned), attribute.Bool("sandbox.timed_out", run.TimedOut), attribute.String("sandbox.status", run.Status))
		if executeErr != nil {
			span.RecordError(executeErr)
			span.SetStatus(codes.Error, executeErr.Error())
		}
		span.End()
	}()
	spec, ok := scenarios[run.Scenario]
	if !ok {
		run.Status, run.Error = "failed", "unknown sandbox scenario"
		return errors.New("unknown sandbox scenario")
	}
	run.ScenarioLabel = spec.label
	now := time.Now().UTC()
	run.StartedAt, run.Status = &now, "running"
	run.Policy = e.Policy(spec.timeout)
	if run.Attempt < 1 {
		run.Attempt = 1
	}
	name := fmt.Sprintf("liveguard-sbx-%s-a%d", strings.TrimPrefix(run.ID, "sbx_"), run.Attempt)
	run.ContainerName = name
	e.removeOrphans(run.ID)

	createCtx, cancelCreate := context.WithTimeout(ctx, 15*time.Second)
	createOutput, createErr := e.Docker.Run(createCtx, "docker", e.createArgsForRun(run, name, spec.command)...)
	cancelCreate()
	if createErr != nil {
		run.Output, run.OutputTruncated = truncate(createOutput)
		run.Error = "docker create: " + compactError(createErr, createOutput)
		run.Status = "failed"
		e.finish(run, now)
		return fmt.Errorf("docker create: %w", createErr)
	}

	startCtx, cancelStart := context.WithTimeout(ctx, spec.timeout)
	output, startErr := e.Docker.Run(startCtx, "docker", "start", "-a", name)
	timedOut := errors.Is(startCtx.Err(), context.DeadlineExceeded)
	cancelStart()
	if timedOut {
		run.TimedOut = true
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = e.Docker.Run(cleanupCtx, "docker", "kill", name)
		cleanupCancel()
	}
	run.Output, run.OutputTruncated = truncate(output)

	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 5*time.Second)
	stateOutput, inspectErr := e.Docker.Run(inspectCtx, "docker", "inspect", "--format", "{{json .State}}", name)
	inspectCancel()
	if inspectErr == nil {
		var state containerState
		if json.Unmarshal([]byte(strings.TrimSpace(stateOutput)), &state) == nil {
			run.ExitCode, run.OOMKilled = intPtr(state.ExitCode), state.OOMKilled
		}
	}

	removeCtx, removeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, removeErr := e.Docker.Run(removeCtx, "docker", "rm", "-f", name)
	removeCancel()
	run.Cleaned = removeErr == nil
	run.PolicyVerified = evaluate(spec, run)
	if run.PolicyVerified {
		run.Status = "passed"
	} else {
		run.Status = "failed"
		if timedOut != spec.expectTimeout && startErr != nil {
			run.Error = compactError(startErr, output)
		} else {
			run.Error = "sandbox policy did not produce the expected result"
		}
	}
	e.finish(run, now)
	if ctx.Err() != nil && !timedOut {
		return ctx.Err()
	}
	if !run.Cleaned {
		return errors.New("docker container cleanup failed")
	}
	return nil
}

func (e *Executor) removeOrphans(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	output, err := e.Docker.Run(ctx, "docker", "ps", "-aq", "--filter", "label=liveguard.run_id="+runID)
	if err != nil {
		return
	}
	for _, containerID := range strings.Fields(output) {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = e.Docker.Run(removeCtx, "docker", "rm", "-f", containerID)
		removeCancel()
	}
}

func (e *Executor) finish(run *Run, started time.Time) {
	finished := time.Now().UTC()
	run.FinishedAt = &finished
	run.DurationMS = finished.Sub(started).Milliseconds()
}

func evaluate(spec scenario, run *Run) bool {
	if !run.Cleaned || run.OOMKilled || run.TimedOut != spec.expectTimeout {
		return false
	}
	if spec.expectTimeout {
		return true
	}
	if run.ExitCode == nil || spec.expectedExit == nil || *run.ExitCode != *spec.expectedExit {
		return false
	}
	return spec.expectOutput == "" || strings.Contains(run.Output, spec.expectOutput)
}

func truncate(value string) (string, bool) {
	if len(value) <= maxOutputBytes {
		return value, false
	}
	return value[:maxOutputBytes] + "\n… output truncated by LiveGuard …", true
}

func compactError(err error, output string) string {
	message := strings.TrimSpace(output)
	if message == "" {
		message = err.Error()
	}
	if len(message) > 400 {
		message = message[:400] + "…"
	}
	return message
}

type Lab struct {
	executor ExecutionEngine
	mu       sync.RWMutex
	runs     map[string]*Run
}

func NewLab(executor ExecutionEngine) *Lab {
	return &Lab{executor: executor, runs: map[string]*Run{}}
}

func ScenarioNames() []string { return append([]string(nil), scenarioOrder...) }

func IsAllowedScenario(name string) bool {
	_, ok := scenarios[name]
	return ok
}

func (l *Lab) Submit(scenarioName, suiteID string) (*Run, error) {
	return l.SubmitWithKey(scenarioName, suiteID, "")
}

func (l *Lab) SubmitWithKey(scenarioName, suiteID, idempotencyKey string) (*Run, error) {
	return l.submitWithInput(context.Background(), scenarioName, suiteID, idempotencyKey, nil)
}

func (l *Lab) submitWithInput(parent context.Context, scenarioName, suiteID, idempotencyKey string, input *ToolInput) (*Run, error) {
	spec, ok := scenarios[scenarioName]
	if !ok {
		return nil, fmt.Errorf("unsupported scenario %q", scenarioName)
	}
	run := &Run{ID: domain.NewID("sbx"), IdempotencyKey: strings.TrimSpace(idempotencyKey), RequestHash: hashRequest(scenarioName, input), SuiteID: suiteID, Scenario: scenarioName, ScenarioLabel: spec.label, Input: input, Status: "queued", Policy: l.executor.Policy(spec.timeout), CreatedAt: time.Now().UTC()}
	InjectTrace(parent, run)
	l.mu.Lock()
	l.runs[run.ID] = run
	l.mu.Unlock()
	go func() {
		l.mu.Lock()
		copyRun := *l.runs[run.ID]
		copyRun.Attempt++
		l.mu.Unlock()
		_ = l.executor.Execute(parent, &copyRun)
		l.mu.Lock()
		l.runs[run.ID] = &copyRun
		l.mu.Unlock()
	}()
	return cloneRun(run), nil
}

func (l *Lab) RunTool(ctx context.Context, input ToolInput, idempotencyKey string) (*Run, error) {
	run, err := l.submitWithInput(ctx, ScenarioDiagnosePage, "", idempotencyKey, &input)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		l.mu.RLock()
		current := cloneRun(l.runs[run.ID])
		l.mu.RUnlock()
		if current.Status == "passed" || current.Status == "failed" {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *Lab) SubmitSuite() ([]*Run, error) {
	return l.SubmitSuiteWithKey("")
}

func (l *Lab) SubmitSuiteWithKey(idempotencyKey string) ([]*Run, error) {
	suiteID := domain.NewID("suite")
	runs := make([]*Run, 0, len(scenarioOrder))
	for _, scenarioName := range scenarioOrder {
		key := ""
		if idempotencyKey != "" {
			key = idempotencyKey + ":" + scenarioName
		}
		run, err := l.SubmitWithKey(scenarioName, suiteID, key)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func (l *Lab) List() []*Run {
	l.mu.RLock()
	defer l.mu.RUnlock()
	runs := make([]*Run, 0, len(l.runs))
	for _, run := range l.runs {
		runs = append(runs, cloneRun(run))
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	return runs
}

func (l *Lab) InfraStatus() InfraStatus {
	return InfraStatus{Mode: "in_memory", Postgres: "disabled", Redis: "disabled", Stream: "disabled", StoredRuns: int64(len(l.List())), Worker: "embedded", RPC: "disabled"}
}

func cloneRun(run *Run) *Run {
	copyRun := *run
	return &copyRun
}

func hashRequest(scenarioName string, input *ToolInput) string {
	payload, _ := json.Marshal(struct {
		Scenario string     `json:"scenario"`
		Input    *ToolInput `json:"input,omitempty"`
	}{Scenario: scenarioName, Input: input})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func InjectTrace(ctx context.Context, run *Run) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	run.TraceParent = carrier.Get("traceparent")
	run.TraceState = carrier.Get("tracestate")
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		run.TraceID = spanContext.TraceID().String()
	}
}

func ExtractTrace(ctx context.Context, traceParent, traceState string) context.Context {
	carrier := propagation.MapCarrier{"traceparent": traceParent, "tracestate": traceState}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
