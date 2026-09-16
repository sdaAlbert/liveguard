package agent

import (
	"fmt"
	"sync"
	"time"
)

type CircuitBreaker struct {
	mu          sync.Mutex
	threshold   int
	cooldown    time.Duration
	failures    int
	openUntil   time.Time
	probeActive bool
	now         func() time.Time
}

func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold < 1 {
		threshold = 1
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &CircuitBreaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

func (b *CircuitBreaker) BeforeRequest() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.openUntil.IsZero() {
		return nil
	}
	if now.Before(b.openUntil) {
		return fmt.Errorf("openai circuit open; retry after %s", b.openUntil.Sub(now).Round(time.Second))
	}
	if b.probeActive {
		return fmt.Errorf("openai circuit half-open; recovery probe already running")
	}
	b.probeActive = true
	return nil
}

func (b *CircuitBreaker) Success() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.failures, b.openUntil, b.probeActive = 0, time.Time{}, false
	b.mu.Unlock()
}

func (b *CircuitBreaker) Failure() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.failures++
	b.probeActive = false
	if b.failures >= b.threshold {
		b.openUntil = b.now().Add(b.cooldown)
	}
	b.mu.Unlock()
}
