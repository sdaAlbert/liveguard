package agent

import (
	"testing"
	"time"
)

func TestCircuitBreakerOpensAndAllowsSingleRecoveryProbe(t *testing.T) {
	now := time.Unix(100, 0)
	breaker := NewCircuitBreaker(1, 30*time.Second)
	breaker.now = func() time.Time { return now }
	if err := breaker.BeforeRequest(); err != nil {
		t.Fatal(err)
	}
	breaker.Failure()
	if err := breaker.BeforeRequest(); err == nil {
		t.Fatal("expected open circuit to reject request")
	}
	now = now.Add(31 * time.Second)
	if err := breaker.BeforeRequest(); err != nil {
		t.Fatalf("expected one half-open probe: %v", err)
	}
	if err := breaker.BeforeRequest(); err == nil {
		t.Fatal("expected concurrent half-open probe to be rejected")
	}
	breaker.Success()
	if err := breaker.BeforeRequest(); err != nil {
		t.Fatalf("success should close circuit: %v", err)
	}
}
