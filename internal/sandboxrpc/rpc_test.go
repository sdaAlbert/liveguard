package sandboxrpc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	sandboxv1 "liveguard/internal/gen/sandboxv1"
	"liveguard/internal/sandbox"
)

type fakeDocker struct{}

func (fakeDocker) Run(_ context.Context, _ string, args ...string) (string, error) {
	switch {
	case len(args) > 0 && args[0] == "ps":
		return "", nil
	case len(args) > 0 && args[0] == "create":
		return "fake-container", nil
	case len(args) > 1 && args[0] == "start" && args[1] == "-a":
		return "sandbox ready\nuid=65534(nobody) gid=65534(nogroup)\nresult=PASS", nil
	case len(args) > 0 && args[0] == "inspect":
		return `{"ExitCode":0,"OOMKilled":false}`, nil
	case len(args) > 0 && args[0] == "rm":
		return "removed", nil
	default:
		return "", nil
	}
}

func TestResponseRoundTripPreservesEvidence(t *testing.T) {
	exit := 42
	started, finished := time.Now().UTC(), time.Now().UTC().Add(time.Second)
	original := &sandbox.Run{ID: "sbx_test", Attempt: 2, Scenario: sandbox.ScenarioNetworkDenied, ScenarioLabel: "断网拦截", Status: "passed", PolicyVerified: true, Policy: sandbox.Policy{Image: "golang:1.26", Network: "none"}, ExitCode: &exit, TimedOut: false, Cleaned: true, Output: "NETWORK_BLOCKED", StartedAt: &started, FinishedAt: &finished, DurationMS: 1000}
	copy := &sandbox.Run{}
	applyResponse(copy, toResponse(original))
	if copy.ID != "" {
		// ID is owned by the caller and intentionally not overwritten.
		t.Fatalf("unexpected caller ID mutation: %s", copy.ID)
	}
	if copy.Status != original.Status || copy.ExitCode == nil || *copy.ExitCode != exit || !copy.Cleaned || copy.Policy.Network != "none" {
		t.Fatalf("round trip lost execution evidence: %#v", copy)
	}
}

func TestGRPCExecuteCrossesProcessBoundaryAndPreservesEvidence(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	executor := sandbox.NewExecutor("test-image")
	executor.Docker = fakeDocker{}
	sandboxv1.RegisterSandboxExecutorServer(server, NewServer(executor, 1))
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	response, err := sandboxv1.NewSandboxExecutorClient(connection).Execute(ctx, &sandboxv1.ExecuteRequest{
		RunId: "sbx_rpc_integration", Scenario: sandbox.ScenarioNormal, Attempt: 3,
	})
	if err != nil {
		t.Fatalf("Execute RPC failed: %v", err)
	}
	if response.GetStatus() != "passed" || !response.GetCleaned() || response.GetExitCode() != 0 {
		t.Fatalf("unexpected RPC evidence: %#v", response)
	}
	if !strings.Contains(response.GetOutput(), "uid=65534") || response.GetAttempt() != 3 {
		t.Fatalf("RPC lost output or fencing token: %#v", response)
	}
}
