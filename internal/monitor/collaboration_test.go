package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type clarificationFlowReviewer struct {
	calls  atomic.Int32
	inputs chan UnexpectedEventInput
}

func (r *clarificationFlowReviewer) Review(_ context.Context, input UnexpectedEventInput) (UnexpectedEventDecision, error) {
	r.inputs <- input
	if r.calls.Add(1) == 1 {
		return UnexpectedEventDecision{
			Action: "ask_user", Category: "other", UserQuestion: "你能接受的到手价上限是多少？",
			UserOptions: []string{"500 元", "700 元"}, Reason: "需要用户定义价格偏好", Confidence: .9,
			ToolCalls: []ReviewToolCall{{Tool: "request_user_input", Succeeded: true}},
		}, nil
	}
	return UnexpectedEventDecision{Action: "wait", Category: "other", Reason: "已结合用户补充完成判断，继续观察", Confidence: .9}, nil
}

func TestClarificationAnswerResumesSameInvestigationWhileMonitorKeepsRunning(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	defer s.StopAll()
	reviewer := &clarificationFlowReviewer{inputs: make(chan UnexpectedEventInput, 2)}
	s.SetUnexpectedEventReviewer(reviewer, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{
		TargetURL: "https://example.com/live", Goal: "耳机上架时提醒我，价格按我的预算判断；不知道预算就先问我。", SourceMode: "ocr_upload",
	})
	if err != nil {
		t.Fatal(err)
	}
	event := SignalEvent{ID: "clarification-signal", Source: SignalOCR, Text: "新款耳机上架，到手价 599 元", Confidence: .95, ValidUntil: time.Now().Add(30 * time.Second)}
	if err := s.SubmitSignal(context.Background(), run.ID, event); err != nil {
		t.Fatal(err)
	}
	first := waitForClarificationInput(t, reviewer.inputs)
	if len(first.RecentEvidence) == 0 || first.RecentEvidence[0].ID != event.ID {
		t.Fatalf("first review did not receive submitted evidence: %+v", first.RecentEvidence)
	}
	waiting := waitForInvestigation(t, s, run.ID, InvestigationWaitingUser)
	if len(waiting.Investigations) == 0 {
		t.Fatal("clarification did not create an investigation")
	}
	var investigationID string
	for _, investigation := range waiting.Investigations {
		if investigation.Status == InvestigationWaitingUser {
			investigationID = investigation.ID
			if investigation.UserQuestion != "你能接受的到手价上限是多少？" {
				t.Fatalf("unexpected question: %q", investigation.UserQuestion)
			}
		}
	}
	if investigationID == "" {
		t.Fatal("waiting investigation ID not found")
	}
	if _, err := s.AnswerInvestigation(context.Background(), run.ID, investigationID, "到手价 700 元以内都可以"); err != nil {
		t.Fatal(err)
	}
	second := waitForClarificationInput(t, reviewer.inputs)
	if second.UserClarification != "到手价 700 元以内都可以" {
		t.Fatalf("review did not receive user answer: %q", second.UserClarification)
	}
	if len(second.RecentEvidence) == 0 || second.RecentEvidence[0].ID != event.ID {
		t.Fatalf("clarification review did not reuse original evidence: %+v", second.RecentEvidence)
	}
	finished := waitForInvestigation(t, s, run.ID, InvestigationWaiting)
	if finished.Status != RunRunning {
		t.Fatalf("monitor stopped after clarification: status=%q", finished.Status)
	}
	for _, investigation := range finished.Investigations {
		if investigation.ID == investigationID {
			if investigation.UserAnswer != "到手价 700 元以内都可以" {
				t.Fatalf("user answer not retained: %q", investigation.UserAnswer)
			}
			return
		}
	}
	t.Fatalf("same investigation %q was not retained after resume", investigationID)
}

func waitForClarificationInput(t *testing.T, inputs <-chan UnexpectedEventInput) UnexpectedEventInput {
	t.Helper()
	select {
	case input := <-inputs:
		return input
	case <-time.After(3 * time.Second):
		t.Fatal("agent review did not start")
		return UnexpectedEventInput{}
	}
}

func waitForInvestigation(t *testing.T, s *Service, runID, status string) *Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		run, err := s.Get(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		for _, investigation := range run.Investigations {
			if investigation.Status == status {
				return run
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	run, _ := s.Get(context.Background(), runID)
	var statuses []string
	for _, investigation := range run.Investigations {
		statuses = append(statuses, fmt.Sprintf("%s:%s", investigation.ID, investigation.Status))
	}
	t.Fatal("investigation did not reach " + status + "; current=" + strings.Join(statuses, ","))
	return nil
}
