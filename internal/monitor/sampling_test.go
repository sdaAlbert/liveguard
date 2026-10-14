package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func samplingDecision(mode SamplingMode) UnexpectedEventDecision {
	return UnexpectedEventDecision{Action: "wait", Category: "custom_goal", Confidence: .9,
		Sampling:  &SamplingProposal{Mode: mode, Reason: "当前明确显示未开赛"},
		ToolCalls: []ReviewToolCall{{Tool: "search_observations", Succeeded: true}}}
}

func TestSamplingFootballWaitApproachAndCandidateExpiry(t *testing.T) {
	now := time.Now().UTC()
	plan := normalizeMonitoringPlan(defaultGoalPlan("足球开赛后提醒我", "test"), "足球开赛后提醒我")
	run := &Run{Goal: "足球开赛后提醒我", Plan: plan, Sampling: initialSampling(plan, now)}
	decision := samplingDecision(SamplingWaiting)
	for _, offset := range []time.Duration{20, 40} {
		applySamplingDecision(run, decision, now.Add(offset*time.Second))
		if run.Sampling.Mode != SamplingObserving || run.Sampling.CaptureIntervalSeconds != 6 {
			t.Fatalf("slowed down before three reviews: %+v", run.Sampling)
		}
	}
	eventAt := now.Add(10 * time.Minute)
	decision.Sampling.NextEventAt = &eventAt
	applySamplingDecision(run, decision, now.Add(65*time.Second))
	if run.Sampling.Mode != SamplingWaiting || run.Sampling.CaptureIntervalSeconds != 12 {
		t.Fatalf("stable waiting not applied: %+v", run.Sampling)
	}
	approach := samplingAt(run, eventAt.Add(-time.Minute))
	if approach.Mode != SamplingCandidate || approach.CaptureIntervalSeconds != 6 || approach.ReviewIntervalSeconds != 15 {
		t.Fatalf("did not accelerate near event: %+v", approach)
	}
	run.Sampling = approach
	afterEvent := samplingAt(run, eventAt.Add(3*time.Minute))
	if afterEvent.Mode != SamplingObserving {
		t.Fatalf("scheduled candidate never expired: %+v", afterEvent)
	}
	applySamplingDecision(run, samplingDecision(SamplingCandidate), now.Add(66*time.Second))
	if run.Sampling.Mode != SamplingCandidate || run.Sampling.CaptureIntervalSeconds != 6 {
		t.Fatalf("candidate should override cooldown: %+v", run.Sampling)
	}
	expired := samplingAt(run, now.Add(160*time.Second))
	if expired.Mode != SamplingObserving || expired.ReviewIntervalSeconds != plan.ReviewIntervalSeconds {
		t.Fatalf("candidate never expired: %+v", expired)
	}
}

func TestSamplingBoundsCooldownAndExplicitUrgency(t *testing.T) {
	now := time.Now().UTC()
	for _, goal := range []string{"重要消息出现时提醒我", "钓鱼直播有鱼上钩时提醒我", "实时监控足球开赛"} {
		plan := normalizeMonitoringPlan(defaultGoalPlan(goal, "test"), goal)
		run := &Run{Goal: goal, Plan: plan, Sampling: initialSampling(plan, now)}
		for i := 0; i < 3; i++ {
			applySamplingDecision(run, samplingDecision(SamplingWaiting), now.Add(65*time.Second))
		}
		first := run.Sampling.CaptureIntervalSeconds
		applySamplingDecision(run, samplingDecision(SamplingWaiting), now.Add(66*time.Second))
		if run.Sampling.CaptureIntervalSeconds != first {
			t.Fatal("cooldown did not stop repeated slowing")
		}
		for i := 2; i < 7; i++ {
			applySamplingDecision(run, samplingDecision(SamplingWaiting), now.Add(time.Duration(i)*65*time.Second))
		}
		want := 24
		if periodicVision(plan) {
			want = 12
		}
		if goalNeedsFastPlan(goal) {
			want = 6
		}
		if run.Sampling.CaptureIntervalSeconds != want {
			t.Fatalf("goal %q sampling=%+v want=%d", goal, run.Sampling, want)
		}
	}
}

func TestInvalidSamplingProposalsAndFailedReviewsCannotChangePolicy(t *testing.T) {
	now := time.Now().UTC()
	plan := normalizeMonitoringPlan(defaultGoalPlan("重要消息", "test"), "重要消息")
	bad := []UnexpectedEventDecision{samplingDecision("anything"), samplingDecision(SamplingWaiting), samplingDecision(SamplingWaiting), samplingDecision(SamplingWaiting)}
	bad[1].Confidence = .1
	bad[2].ToolCalls = nil
	past := now.Add(-time.Hour)
	bad[3].Sampling.NextEventAt = &past
	for _, d := range bad {
		run := &Run{Goal: "重要消息", Plan: plan, Sampling: initialSampling(plan, now)}
		applySamplingDecision(run, d, now.Add(time.Minute))
		if run.Sampling.RejectedProposals != 1 || run.Sampling.Mode != SamplingObserving {
			t.Fatalf("invalid proposal accepted: %+v", run.Sampling)
		}
	}
	run := &Run{Goal: "重要消息", Plan: plan, Sampling: initialSampling(plan, now)}
	d := samplingDecision(SamplingCandidate)
	d.Failure = "provider failed"
	applySamplingDecision(run, d, now.Add(time.Minute))
	if run.Sampling.Mode != SamplingObserving {
		t.Fatal("failed review changed sampling")
	}
}

type frameCheckReviewer struct {
	calls  int
	inputs []UnexpectedEventInput
}

func (r *frameCheckReviewer) Review(_ context.Context, input UnexpectedEventInput) (UnexpectedEventDecision, error) {
	r.calls++
	r.inputs = append(r.inputs, input)
	if r.calls == 1 {
		return samplingDecision(SamplingWaiting), nil
	}
	return UnexpectedEventDecision{Action: "alert", Category: "custom_goal", Summary: "画面中鱼已被提起", Confidence: .95, VisualFinding: "鱼在主播手中，已经离开水面", ToolCalls: []ReviewToolCall{{Tool: "search_observations", Succeeded: true}, {Tool: "search_recent_alerts", Succeeded: true}, {Tool: "inspect_live_frame", Succeeded: true}, {Tool: "create_alert", Succeeded: true}}}, nil
}

func TestFishingOnlyNovelOCRTriggersAgentFromCapturedFrames(t *testing.T) {
	for _, text := range []string{"直播进行中", ""} {
		t.Run(text, func(t *testing.T) {
			s := NewService(NewMemoryRepository(), nil)
			r := &frameCheckReviewer{}
			s.SetUnexpectedEventReviewer(r, time.Nanosecond)
			run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.douyin.com/fish", Goal: "鱼出水时提醒我", SourceMode: "ocr_upload"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.StopAll()
			var deduper OCRLineDeduper
			frame := SignalEvent{RoomID: run.Rooms[0].ID, Source: SignalOCR, Text: text, Confidence: .9, FramePath: "owned-test-frame.png", ObservedAt: time.Now().UTC()}
			if err := s.ingestCapturedFrame(context.Background(), run.ID, frame, &deduper); err != nil {
				t.Fatal(err)
			}
			wantCalls := 0
			if text != "" {
				wantCalls = 1
			}
			if r.calls != wantCalls {
				t.Fatalf("first captured frame calls=%d want=%d: %+v", r.calls, wantCalls, r.inputs)
			}
			// Repeated frames remain archive-only even after the visual cadence elapses.
			frame.ID = ""
			frame.ObservedAt = time.Now().UTC()
			if err := s.ingestCapturedFrame(context.Background(), run.ID, frame, &deduper); err != nil {
				t.Fatal(err)
			}
			if r.calls != wantCalls {
				t.Fatal("repeated OCR frame triggered Agent")
			}
			s.mu.Lock()
			s.lastUnexpectedReview[run.ID] = time.Now().Add(-2 * time.Minute)
			s.mu.Unlock()
			_, _ = s.repository.Update(context.Background(), run.ID, func(current *Run) error {
				current.Sampling.LastVisualAttemptAt = time.Now().Add(-2 * time.Minute)
				return nil
			})
			frame.ObservedAt = time.Now().UTC()
			if err := s.ingestCapturedFrame(context.Background(), run.ID, frame, &deduper); err != nil {
				t.Fatal(err)
			}
			current, _ := s.Get(context.Background(), run.ID)
			if r.calls != wantCalls || len(current.Alerts) != 0 || current.Sampling.VisualCount != 0 {
				t.Fatalf("unchanged OCR frame was not kept out of Agent review: %+v", current)
			}
			if _, err = s.Stop(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.ingestCapturedFrame(context.Background(), run.ID, frame, &deduper); err == nil {
				t.Fatal("stopped monitor accepted frame")
			}
		})
	}
}

type lateGoalReviewer struct{ started, release chan struct{} }

func (r lateGoalReviewer) Review(ctx context.Context, _ UnexpectedEventInput) (UnexpectedEventDecision, error) {
	close(r.started)
	select {
	case <-r.release:
	case <-ctx.Done():
		return UnexpectedEventDecision{}, ctx.Err()
	}
	d := samplingDecision(SamplingCandidate)
	d.Action = "alert"
	d.Summary = "旧目标命中"
	return d, nil
}

func TestUserGoalUpdateDiscardsLateReviewAndResetsSampling(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	r := lateGoalReviewer{make(chan struct{}), make(chan struct{})}
	s.SetUnexpectedEventReviewer(r, time.Nanosecond)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.douyin.com/test", Goal: "重要消息提醒", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.StopAll()
	done := make(chan error, 1)
	go func() {
		done <- s.Ingest(context.Background(), run.ID, SignalEvent{Source: SignalOCR, Text: "旧目标公告", Confidence: .9})
	}()
	select {
	case <-r.started:
	case <-time.After(time.Second):
		t.Fatal("review not started")
	}
	updated, err := s.UpdateGoal(context.Background(), run.ID, "有人钓到鱼时提醒我")
	if err != nil {
		t.Fatal(err)
	}
	close(r.release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	current, _ := s.Get(context.Background(), run.ID)
	if current.GoalVersion != updated.GoalVersion || len(current.Alerts) != 0 || len(current.Investigations) != 0 || current.Sampling.Mode != SamplingObserving {
		t.Fatalf("old review affected new user goal: %+v", current)
	}
}

func TestFrameMarkerIsNotPublicEvidenceAndCannotAlertWithoutVision(t *testing.T) {
	s := NewService(NewMemoryRepository(), nil)
	run, err := s.Start(context.Background(), StartInput{TargetURL: "https://live.douyin.com/test", Goal: "钓鱼提醒", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.StopAll()
	if err = s.Ingest(context.Background(), run.ID, SignalEvent{Source: SignalFrame, Text: "fake frame"}); err == nil {
		t.Fatal("public frame marker accepted")
	}
	s.SetUnexpectedEventReviewer(&testUnexpectedReviewer{decision: UnexpectedEventDecision{Action: "alert", Category: "custom_goal", Confidence: .95, Summary: "invented fish"}}, time.Nanosecond)
	if err = s.Ingest(context.Background(), run.ID, SignalEvent{Source: SignalFrame, Text: "sampling only", FramePath: "owned.png"}); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Get(context.Background(), run.ID)
	if len(current.Alerts) != 0 || !strings.Contains(current.Investigations[0].Conclusion, "看图证据") {
		t.Fatalf("fabricated alert: %+v", current)
	}
}

func TestStructuredDecisionRejectsUnknownSamplingFields(t *testing.T) {
	_, err := parseReviewDecision(`{"action":"wait","category":"other","reason":"待观察","confidence":0.9,"sampling":{"mode":"stable_wait","reason":"未开赛","shell":"anything"}}`)
	if err == nil {
		t.Fatal("arbitrary nested sampling policy accepted")
	}
}

func TestChatScopeCannotBeBypassedByChangingAlertCategory(t *testing.T) {
	for _, goal := range []string{"有人问哪里能报名时提醒我", "聊天区出现优惠券时提醒我"} {
		plan := normalizeMonitoringPlan(defaultGoalPlan(goal, "test"), goal)
		if plan.AlertScope != "chat" {
			t.Fatal("chat goal lost its evidence scope")
		}
		input := UnexpectedEventInput{Goal: goal, Plan: plan, RecentEvidence: []SignalEvent{{RoomID: "room", Source: SignalOCR, Text: "评论：活动在哪里报名？", Confidence: .9}}}
		state := miniMaxReviewState{readEvidenceTurn: 0, searchAlertsTurn: 0, searchedCategory: "custom_goal"}
		block := miniMaxContentBlock{ID: "create", Name: "create_alert", Input: json.RawMessage(`{"category":"custom_goal","summary":"有人问报名","reason":"OCR提到评论","confidence":0.95,"visual_finding":""}`)}
		result, _, terminal := (&MiniMaxUnexpectedEventReviewer{}).executeTool(context.Background(), block, input, nil, &state, 1)
		if !result.IsError || terminal {
			t.Fatal("category change bypassed region confirmation")
		}
		run := &Run{Goal: goal, Plan: plan, Rooms: []Room{{ID: "room"}}, Events: input.RecentEvidence}
		applyUnexpectedReview(run, input.RecentEvidence[0], UnexpectedEventDecision{Action: "alert", Category: "custom_goal", Summary: "有人问报名", Confidence: .95})
		if len(run.Alerts) != 0 {
			t.Fatal("commit layer accepted unverified chat alert")
		}
		decision := UnexpectedEventDecision{Action: "alert", Category: "custom_goal", Summary: "有人问报名", Confidence: .95, VisualFinding: "猜测在聊天区"}
		applyUnexpectedReview(run, input.RecentEvidence[0], decision)
		if len(run.Alerts) != 0 {
			t.Fatal("invented visual finding bypassed actual tool receipt")
		}
	}
}

func TestFrameTicksDoNotDeduplicateDifferentVisualEvents(t *testing.T) {
	now := time.Now().UTC()
	event := SignalEvent{RoomID: "room", Source: SignalFrame, Text: "定时画面检查", ObservedAt: now}
	alerts := []Alert{{RoomID: "room", Summary: "红色瓶子出现", CreatedAt: now.Add(-time.Second), Evidence: []Evidence{{Source: string(SignalFrame), Text: event.Text}}}}
	if hasRecentSemanticAlert(alerts, event, "proactive_custom_goal", "蓝色杯子出现") {
		t.Fatal("sampling metadata suppressed a new visual event")
	}
	if !hasRecentSemanticAlert(alerts, event, "proactive_custom_goal", "红色瓶子出现") {
		t.Fatal("identical event summary was not deduplicated")
	}
}
