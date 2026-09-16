package sandboxrpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	sandboxv1 "liveguard/internal/gen/sandboxv1"
	"liveguard/internal/sandbox"
)

type Server struct {
	sandboxv1.UnimplementedSandboxExecutorServer
	executor *sandbox.Executor
	slots    chan struct{}
}

func NewServer(executor *sandbox.Executor, concurrency int) *Server {
	if concurrency < 1 {
		concurrency = 2
	}
	return &Server{executor: executor, slots: make(chan struct{}, concurrency)}
}

func (s *Server) Execute(ctx context.Context, request *sandboxv1.ExecuteRequest) (*sandboxv1.ExecuteResponse, error) {
	if request == nil || !strings.HasPrefix(request.GetRunId(), "sbx_") || !sandbox.IsAllowedScenario(request.GetScenario()) {
		return nil, status.Error(codes.InvalidArgument, "invalid run id or unsupported scenario")
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return nil, status.Error(codes.ResourceExhausted, "sandbox worker concurrency limit reached")
	}
	run := &sandbox.Run{ID: request.GetRunId(), Scenario: request.GetScenario(), Attempt: int(request.GetAttempt()), Status: "running"}
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		run.TraceID = spanContext.TraceID().String()
	}
	if input := request.GetInput(); input != nil {
		run.Input = &sandbox.ToolInput{Title: input.GetTitle(), TextSnippet: input.GetTextSnippet(), Objective: input.GetObjective()}
	}
	if err := s.executor.Execute(ctx, run); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			return nil, status.Error(codes.DeadlineExceeded, err.Error())
		case strings.Contains(err.Error(), "docker"):
			return nil, status.Error(codes.Unavailable, err.Error())
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	return toResponse(run), nil
}

type Client struct {
	connection *grpc.ClientConn
	client     sandboxv1.SandboxExecutorClient
	health     grpc_health_v1.HealthClient
	image      string
}

func Dial(target, image string) (*Client, error) {
	connection, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return nil, err
	}
	return &Client{connection: connection, client: sandboxv1.NewSandboxExecutorClient(connection), health: grpc_health_v1.NewHealthClient(connection), image: image}, nil
}

func (c *Client) Close() error { return c.connection.Close() }

func (c *Client) Health(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	response, err := c.health.Check(checkCtx, &grpc_health_v1.HealthCheckRequest{Service: "liveguard.sandbox.v1.SandboxExecutor"})
	if err != nil {
		return err
	}
	if response.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		return fmt.Errorf("sandbox worker is %s", response.GetStatus())
	}
	return nil
}

func (c *Client) Policy(timeout time.Duration) sandbox.Policy {
	return sandbox.Policy{Image: c.image, Memory: "128 MiB", CPUs: "0.5", PIDs: 32, Network: "none", ReadOnly: true, NonRoot: true, CapDropAll: true, TimeoutMS: timeout.Milliseconds()}
}

func (c *Client) Execute(ctx context.Context, run *sandbox.Run) error {
	rpcCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	request := &sandboxv1.ExecuteRequest{RunId: run.ID, Scenario: run.Scenario, Attempt: int32(run.Attempt)}
	if run.Input != nil {
		request.Input = &sandboxv1.ToolInput{Title: run.Input.Title, TextSnippet: run.Input.TextSnippet, Objective: run.Input.Objective}
	}
	response, err := c.client.Execute(rpcCtx, request)
	if err != nil {
		return fmt.Errorf("sandbox RPC: %w", err)
	}
	applyResponse(run, response)
	return nil
}

func toResponse(run *sandbox.Run) *sandboxv1.ExecuteResponse {
	response := &sandboxv1.ExecuteResponse{
		RunId: run.ID, Attempt: int32(run.Attempt), Scenario: run.Scenario, ScenarioLabel: run.ScenarioLabel,
		Status: run.Status, PolicyVerified: run.PolicyVerified, Policy: toPolicy(run.Policy), ContainerName: run.ContainerName,
		OomKilled: run.OOMKilled, TimedOut: run.TimedOut, Cleaned: run.Cleaned, Output: run.Output,
		OutputTruncated: run.OutputTruncated, Error: run.Error, DurationMs: run.DurationMS, TraceId: run.TraceID,
	}
	if run.ExitCode != nil {
		value := int32(*run.ExitCode)
		response.ExitCode = &value
	}
	if run.StartedAt != nil {
		response.StartedAtUnixMs = run.StartedAt.UnixMilli()
	}
	if run.FinishedAt != nil {
		response.FinishedAtUnixMs = run.FinishedAt.UnixMilli()
	}
	return response
}

func applyResponse(run *sandbox.Run, response *sandboxv1.ExecuteResponse) {
	run.Attempt, run.Scenario, run.ScenarioLabel = int(response.GetAttempt()), response.GetScenario(), response.GetScenarioLabel()
	run.Status, run.PolicyVerified, run.Policy = response.GetStatus(), response.GetPolicyVerified(), fromPolicy(response.GetPolicy())
	run.ContainerName, run.OOMKilled, run.TimedOut, run.Cleaned = response.GetContainerName(), response.GetOomKilled(), response.GetTimedOut(), response.GetCleaned()
	run.Output, run.OutputTruncated, run.Error, run.DurationMS = response.GetOutput(), response.GetOutputTruncated(), response.GetError(), response.GetDurationMs()
	run.TraceID = response.GetTraceId()
	if response.ExitCode != nil {
		value := int(response.GetExitCode())
		run.ExitCode = &value
	}
	if value := response.GetStartedAtUnixMs(); value > 0 {
		started := time.UnixMilli(value).UTC()
		run.StartedAt = &started
	}
	if value := response.GetFinishedAtUnixMs(); value > 0 {
		finished := time.UnixMilli(value).UTC()
		run.FinishedAt = &finished
	}
}

func toPolicy(policy sandbox.Policy) *sandboxv1.SandboxPolicy {
	return &sandboxv1.SandboxPolicy{Image: policy.Image, Memory: policy.Memory, Cpus: policy.CPUs, Pids: int32(policy.PIDs), Network: policy.Network, ReadOnly: policy.ReadOnly, NonRoot: policy.NonRoot, CapDropAll: policy.CapDropAll, TimeoutMs: policy.TimeoutMS}
}

func fromPolicy(policy *sandboxv1.SandboxPolicy) sandbox.Policy {
	if policy == nil {
		return sandbox.Policy{}
	}
	return sandbox.Policy{Image: policy.GetImage(), Memory: policy.GetMemory(), CPUs: policy.GetCpus(), PIDs: int(policy.GetPids()), Network: policy.GetNetwork(), ReadOnly: policy.GetReadOnly(), NonRoot: policy.GetNonRoot(), CapDropAll: policy.GetCapDropAll(), TimeoutMS: policy.GetTimeoutMs()}
}
