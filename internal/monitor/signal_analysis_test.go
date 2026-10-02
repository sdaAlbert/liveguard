package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestLocalSignalAnalysisBoundsCountsAndCancellation(t *testing.T) {
	a := LocalSignalAnalyzer{}
	result, err := a.Analyze(context.Background(), []SignalEvent{{Text: " a  b\na b\ncancelled\ncaptcha\n"}})
	if err != nil || result.Lines != 4 || result.UniqueLines != 3 || result.NegationLines != 1 || result.VerificationLines != 1 {
		t.Fatalf("unexpected statistics: %#v %v", result, err)
	}
	events := make([]SignalEvent, 7)
	for i := range events {
		events[i].Text = "a"
	}
	result, err = a.Analyze(context.Background(), events)
	if err != nil || result.Lines != 6 {
		t.Fatalf("event bound: %#v %v", result, err)
	}
	result, err = a.Analyze(context.Background(), []SignalEvent{{Text: strings.Repeat("x", 1100) + "\ncaptcha"}})
	if err != nil || result.VerificationLines != 0 {
		t.Fatalf("text bound: %#v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Analyze(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
}

type failingSignalAnalyzer struct{}

func (failingSignalAnalyzer) Analyze(context.Context, []SignalEvent) (SignalAnalysis, error) {
	return SignalAnalysis{}, errors.New("processor failed")
}

func TestLocalAnalysisToolRequiresEvidenceAndRejectsArbitraryArguments(t *testing.T) {
	r := MiniMaxUnexpectedEventReviewer{SignalAnalyzer: LocalSignalAnalyzer{}}
	state := miniMaxReviewState{readEvidenceTurn: -1}
	input := UnexpectedEventInput{RecentEvidence: []SignalEvent{{Text: "announcement"}}}
	block := miniMaxContentBlock{ID: "analysis", Name: "analyze_signal_window", Input: json.RawMessage(`{}`)}
	result, _, _ := r.executeTool(context.Background(), block, input, nil, &state, 1)
	if !result.IsError || state.analysisAttempted {
		t.Fatal("analysis ran before reading evidence")
	}
	state.readEvidenceTurn = 0
	block.Input = json.RawMessage(`{"command":"echo hacked"}`)
	result, _, _ = r.executeTool(context.Background(), block, input, nil, &state, 1)
	if !result.IsError || state.analysisAttempted {
		t.Fatal("arbitrary arguments accepted")
	}
	block.Input = json.RawMessage(`{}`)
	result, _, _ = r.executeTool(context.Background(), block, input, nil, &state, 1)
	if result.IsError || !state.analysisReady {
		t.Fatalf("local result unavailable: %#v", result)
	}
	result, _, _ = r.executeTool(context.Background(), block, input, nil, &state, 2)
	if !result.IsError {
		t.Fatal("repeated analysis accepted")
	}
}

func TestFailedTextAnalysisCannotBeBypassedToAlert(t *testing.T) {
	r := MiniMaxUnexpectedEventReviewer{SignalAnalyzer: failingSignalAnalyzer{}}
	state := miniMaxReviewState{readEvidenceTurn: 0, searchAlertsTurn: 0, searchedCategory: "custom_goal"}
	input := UnexpectedEventInput{RecentEvidence: []SignalEvent{{Text: "announcement"}}}
	block := miniMaxContentBlock{ID: "analysis", Name: "analyze_signal_window", Input: json.RawMessage(`{}`)}
	result, _, _ := r.executeTool(context.Background(), block, input, nil, &state, 1)
	if !result.IsError || !state.analysisAttempted || state.analysisReady {
		t.Fatal("failure was not recorded")
	}
	block = miniMaxContentBlock{ID: "create", Name: "create_alert", Input: json.RawMessage(`{"category":"custom_goal","summary":"announcement","reason":"confirmed","confidence":0.9,"visual_finding":""}`)}
	result, _, terminal := r.executeTool(context.Background(), block, input, nil, &state, 2)
	if !result.IsError || terminal {
		t.Fatal("failed analysis bypassed")
	}
}
