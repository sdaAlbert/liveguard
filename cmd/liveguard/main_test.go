package main

import (
	"os"
	"testing"
)

func TestNormalizeLocalProxyEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")
	normalizeLocalProxyEnv()
	if got := os.Getenv("HTTP_PROXY"); got != "http://127.0.0.1:7897" {
		t.Fatalf("stale local proxy was not normalized: %q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
	normalizeLocalProxyEnv()
	if got := os.Getenv("HTTPS_PROXY"); got != "http://proxy.example:8080" {
		t.Fatalf("non-local proxy should be preserved, got %q", got)
	}
}
