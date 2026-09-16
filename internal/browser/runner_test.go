package browser

import (
	"context"
	"strings"
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

func TestEvaluateOperationalExpectations(t *testing.T) {
	runner := &Runner{}
	plan := []domain.CheckSpec{
		{Key: "copy", Label: "运营预期文案", Kind: "contains_all", Terms: []string{"海边电台", "活动入口已开启"}},
		{Key: "status", Label: "预期正在直播", Kind: "expected_live_status", Terms: []string{"live"}},
	}
	checks, needsHuman := runner.Evaluate(plan, Result{Title: "直播间", BodyText: "海边电台 正在直播 活动入口已开启"})
	if needsHuman || checks[0].Status != domain.CheckPassed || checks[1].Status != domain.CheckPassed {
		t.Fatalf("expected both operational checks to pass: %#v", checks)
	}

	checks, _ = runner.Evaluate(plan, Result{Title: "直播间", BodyText: "海边电台 直播已结束"})
	if checks[0].Status != domain.CheckFailed || !strings.Contains(checks[0].Observed, "活动入口已开启") {
		t.Fatalf("expected missing copy to fail with evidence: %#v", checks[0])
	}
	if checks[1].Status != domain.CheckFailed {
		t.Fatalf("expected live status mismatch to fail: %#v", checks[1])
	}
}

func TestEvaluateRecognizesDouyinOnlineAudienceAsLive(t *testing.T) {
	runner := &Runner{}
	plan := []domain.CheckSpec{
		{Key: "status", Label: "直播状态", Kind: "live_status"},
		{Key: "expected", Label: "预期正在直播", Kind: "expected_live_status", Terms: []string{"live"}},
	}
	checks, _ := runner.Evaluate(plan, Result{BodyText: "在线观众 · 151"})
	if checks[0].Status != domain.CheckPassed || checks[1].Status != domain.CheckPassed {
		t.Fatalf("expected Douyin audience marker to prove live status: %#v", checks)
	}
}

func TestAuthenticatedInspectionRequiresClosedLoginWindow(t *testing.T) {
	runner := &Runner{ProfileDir: t.TempDir(), SessionProfileDir: t.TempDir(), ArtifactDir: t.TempDir(), AllowedHosts: map[string]bool{"localhost": true}, sessionOpen: true}
	_, err := runner.Inspect(context.Background(), "task-one", "http://localhost/demo", true)
	if err == nil || !strings.Contains(err.Error(), "关闭窗口") {
		t.Fatalf("expected actionable profile lock error, got %v", err)
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
