package agent

import (
	"context"
	"testing"

	"liveguard/internal/domain"
)

func TestDecideToolForUnverifiedActivityEntry(t *testing.T) {
	checks := []domain.CheckResult{{CheckSpec: domain.CheckSpec{Kind: "activity_entry"}, Status: domain.CheckUnverified}}
	decision := DecideTool("检查活动入口是否存在", checks, false)
	if decision == nil || decision.Name != "diagnose_live_page" {
		t.Fatalf("unexpected decision: %#v", decision)
	}
}

func TestToolPolicyApprovesOnlyEvidenceBackedAllowlistedCall(t *testing.T) {
	input := DecisionInput{Objective: "检查活动入口", Checks: []domain.CheckResult{{CheckSpec: domain.CheckSpec{Kind: "activity_entry"}, Status: domain.CheckUnverified}}}
	approved := (ToolPolicy{}).Evaluate(input, &ToolDecision{Name: "diagnose_live_page", Reason: "入口仍未确认"})
	if !approved.Approved {
		t.Fatalf("expected call to be approved: %#v", approved)
	}
	denied := (ToolPolicy{}).Evaluate(input, &ToolDecision{Name: "execute_shell", Reason: "run arbitrary command"})
	if denied.Approved {
		t.Fatalf("unknown tool escaped policy: %#v", denied)
	}
}

func TestDeterministicToolDeciderImplementsRuntimeContract(t *testing.T) {
	input := DecisionInput{Objective: "检查活动入口", Checks: []domain.CheckResult{{CheckSpec: domain.CheckSpec{Kind: "activity_entry"}, Status: domain.CheckUnverified}}}
	result, err := (DeterministicToolDecider{}).Decide(context.Background(), input)
	if err != nil || result.Decision == nil || result.Audit.Source != "deterministic" {
		t.Fatalf("unexpected decision result: %#v err=%v", result, err)
	}
}

func TestDecideToolStopsForHumanChallenge(t *testing.T) {
	checks := []domain.CheckResult{{CheckSpec: domain.CheckSpec{Kind: "activity_entry"}, Status: domain.CheckUnverified}}
	if decision := DecideTool("检查活动入口", checks, true); decision != nil {
		t.Fatalf("expected no tool call, got %#v", decision)
	}
}

func TestApplyDiagnosisTurnsUnknownIntoFailure(t *testing.T) {
	checks := []domain.CheckResult{{CheckSpec: domain.CheckSpec{Kind: "activity_entry"}, Status: domain.CheckUnverified}}
	updated, summary := ApplyDiagnosis(checks, "activity_entry=missing")
	if updated[0].Status != domain.CheckFailed || summary == "" {
		t.Fatalf("diagnosis not applied: %#v %q", updated, summary)
	}
}
