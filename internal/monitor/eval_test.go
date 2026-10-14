package monitor

import (
	"context"
	"testing"
)

func TestEvaluationSeparatesIncompleteDecisionsAndReportsBaseline(t *testing.T) {
	cases := []SignalEvalCase{{Name: "cancelled", Goal: "抽奖开始提醒", Text: "抽奖取消", Confidence: .95}, {Name: "positive", Goal: "抽奖开始提醒", Text: "抽奖开始", Confidence: .95, ExpectAlert: true}}
	r := &testUnexpectedReviewer{decision: UnexpectedEventDecision{Action: "wait", StopReason: "turn_budget"}}
	report := RunSignalEvalCases(context.Background(), r, "test", cases)
	if !report.Completed || report.ExpectedCases != 2 || report.Passed != 0 || report.Baseline.Passed != 2 {
		t.Fatalf("bad report: %+v", report)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report = RunSignalEvalCases(ctx, r, "test", cases)
	if report.Completed || len(report.Cases) != 0 {
		t.Fatalf("cancelled evaluation claimed completion: %+v", report)
	}
}

func TestSyntheticLoadKeepsBoundedReviewsAndStops(t *testing.T) {
	report, err := RunSyntheticLoad(context.Background(), 32)
	if err != nil {
		t.Fatal(err)
	}
	if report.Accepted != 32 || report.Errors != 0 || report.MaxInFlight > 4 || report.ModelReviews < 1 || !report.StoppedRejectsLateSignal {
		t.Fatalf("bad load report: %+v", report)
	}
}
