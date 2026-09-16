package worker

import (
	"testing"

	"liveguard/internal/domain"
)

func TestAppendExpectationsAndSummarizeBusinessVerdict(t *testing.T) {
	task := &domain.Task{ExpectedTexts: []string{"活动入口已开启"}, ExpectedLiveStatus: "live"}
	plan := appendExpectations([]domain.CheckSpec{{Key: "reachable", Kind: "page_reachable"}}, task)
	if len(plan) != 3 || plan[1].Kind != "contains_all" || plan[2].Kind != "expected_live_status" {
		t.Fatalf("unexpected plan: %#v", plan)
	}

	verdict, summary := summarizeChecks([]domain.CheckResult{{Status: domain.CheckFailed}, {Status: domain.CheckUnverified}}, false)
	if verdict != "failed" || summary != "发现 1 项不符合运营预期" {
		t.Fatalf("unexpected verdict %q summary %q", verdict, summary)
	}
	verdict, _ = summarizeChecks(nil, true)
	if verdict != "needs_human" {
		t.Fatalf("expected human verdict, got %q", verdict)
	}
}
