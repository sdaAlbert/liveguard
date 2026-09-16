package agent

import (
	"context"
	"testing"
)

func TestDeterministicPlanAlwaysProducesBoundedChecks(t *testing.T) {
	checks, source, err := (DeterministicPlanner{}).Plan(context.Background(), "https://www.douyin.com/", "检查主播昵称和直播状态")
	if err != nil {
		t.Fatal(err)
	}
	if source != "deterministic" || len(checks) < 2 || len(checks) > 6 {
		t.Fatalf("unexpected plan source=%s count=%d", source, len(checks))
	}
	if err := validateChecks(checks); err != nil {
		t.Fatalf("fallback generated an invalid plan: %v", err)
	}
}

func TestValidateChecksRejectsUnknownCapability(t *testing.T) {
	checks, _, _ := (DeterministicPlanner{}).Plan(context.Background(), "https://www.douyin.com/", "检查直播状态")
	checks[0].Kind = "execute_shell"
	if err := validateChecks(checks); err == nil {
		t.Fatal("expected unknown capability to be rejected")
	}
}

func TestMeaningfulTermsExtractsChineseProductConcepts(t *testing.T) {
	terms := meaningfulTerms("检查主播昵称、直播状态和互动入口")
	if len(terms) < 3 || terms[0] != "主播昵称" || terms[1] != "主播" || terms[2] != "直播状态" {
		t.Fatalf("unexpected terms: %#v", terms)
	}
}
