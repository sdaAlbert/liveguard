package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	sandboxv1 "liveguard/internal/gen/sandboxv1"
	"liveguard/internal/sandbox"
	"liveguard/internal/sandboxrpc"
	"liveguard/internal/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTrace, err := telemetry.Setup(ctx, "liveguard-sandbox-worker", envOr("LIVEGUARD_OTEL_ENDPOINT", "127.0.0.1:4317"))
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTrace(shutdownCtx)
	}()
	address := envOr("LIVEGUARD_SANDBOX_RPC_ADDR", "127.0.0.1:9090")
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal(err)
	}
	server := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	executor := sandbox.NewExecutor(envOr("LIVEGUARD_SANDBOX_IMAGE", "golang:1.26"))
	sandboxv1.RegisterSandboxExecutorServer(server, sandboxrpc.NewServer(executor, 2))
	healthServer := health.NewServer()
	healthServer.SetServingStatus("liveguard.sandbox.v1.SandboxExecutor", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	reflection.Register(server)
	go func() {
		log.Printf("Sandbox Worker gRPC listening on %s", address)
		if err := server.Serve(listener); err != nil {
			log.Printf("Sandbox Worker stopped: %v", err)
		}
	}()
	<-ctx.Done()
	server.GracefulStop()
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
