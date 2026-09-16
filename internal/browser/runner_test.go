package browser

import (
	"testing"

	"liveguard/internal/domain"
)

func TestValidateURLUsesHostBoundary(t *testing.T) {
	runner := &Runner{AllowedHosts: map[string]bool{"douyin.com": true, "localhost": true}}
	for _, target := range []string{"https://www.douyin.com/video/1", "http://localhost:8080/demo/live"} {
		if err := runner.validateURL(target); err != nil {
			t.Fatalf("expected %s to be allowed: %v", target, err)
		}
	}
	for _, target := range []string{"https://douyin.com.evil.example/", "file:///etc/passwd", "javascript:alert(1)"} {
		if err := runner.validateURL(target); err == nil {
			t.Fatalf("expected %s to be rejected", target)
		}
	}
}

func TestEvaluateSeparatesHumanInterventionFromFailure(t *testing.T) {
	runner := &Runner{}
	plan := []domain.CheckSpec{
		{Key: "reachable", Label: "页面可以访问", Kind: "page_reachable"},
		{Key: "challenge", Label: "没有验证阻塞", Kind: "challenge_absent"},
		{Key: "status", Label: "直播状态", Kind: "live_status"},
	}
	checks, needsHuman := runner.Evaluate(plan, Result{Title: "直播间", BodyText: "安全验证 请完成滑块验证", Screenshot: "/shot.png"})
	if !needsHuman {
		t.Fatal("expected human intervention")
	}
	if checks[1].Status != domain.CheckNeedsHuman {
		t.Fatalf("expected needs_human, got %s", checks[1].Status)
	}
	if checks[2].Status != domain.CheckUnverified {
		t.Fatalf("expected live status to remain unverified, got %s", checks[2].Status)
	}
}

func TestEvaluateDetectsChallengeFromTitle(t *testing.T) {
	runner := &Runner{}
	plan := []domain.CheckSpec{{Key: "challenge", Label: "没有验证阻塞", Kind: "challenge_absent"}}
	checks, needsHuman := runner.Evaluate(plan, Result{Title: "验证码中间页", BodyText: ""})
	if !needsHuman || checks[0].Status != domain.CheckNeedsHuman {
		t.Fatalf("expected title-based challenge detection, got needsHuman=%v status=%s", needsHuman, checks[0].Status)
	}
}

func TestExtractDocumentEvidence(t *testing.T) {
	document := `<html><head><title>海边 &amp; 电台</title><style>.x{}</style></head><body><h1>直播中</h1><script>secret()</script></body></html>`
	if got := extractTitle(document); got != "海边 & 电台" {
		t.Fatalf("unexpected title %q", got)
	}
	if got := extractText(document); got != "海边 & 电台 直播中" {
		t.Fatalf("unexpected visible text %q", got)
	}
}
