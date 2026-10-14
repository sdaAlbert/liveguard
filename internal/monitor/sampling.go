package monitor

import (
	"slices"
	"strings"
	"time"
)

type SamplingMode string

const (
	SamplingObserving     SamplingMode = "observing"
	SamplingWaiting       SamplingMode = "stable_wait"
	SamplingCandidate     SamplingMode = "candidate"
	stableReviewsRequired              = 3
	samplingCooldown                   = time.Minute
)

// A model proposal carries no authority to change the user goal or runtime
// limits. It is applied only after a successful, evidence-backed review.
type SamplingProposal struct {
	Mode        SamplingMode `json:"mode"`
	Reason      string       `json:"reason"`
	NextEventAt *time.Time   `json:"next_event_at,omitempty"`
}

type SamplingState struct {
	Mode                   SamplingMode `json:"mode"`
	CaptureIntervalSeconds int          `json:"capture_interval_seconds"`
	ReviewIntervalSeconds  int          `json:"review_interval_seconds"`
	VisionIntervalSeconds  int          `json:"vision_interval_seconds"`
	StableReviews          int          `json:"stable_reviews"`
	ChangedAt              time.Time    `json:"changed_at"`
	CandidateUntil         time.Time    `json:"candidate_until,omitempty"`
	NextEventAt            *time.Time   `json:"next_event_at,omitempty"`
	Reason                 string       `json:"reason"`
	LastVisualAttemptAt    time.Time    `json:"last_visual_attempt_at,omitempty"`
	CaptureCount           uint64       `json:"capture_count"`
	PerceptionDropped      uint64       `json:"perception_dropped"`
	ReviewCount            uint64       `json:"review_count"`
	VisualCount            uint64       `json:"visual_count"`
	RejectedProposals      uint64       `json:"rejected_proposals"`
}

func initialSampling(plan MonitoringPlan, now time.Time) SamplingState {
	return SamplingState{Mode: SamplingObserving, CaptureIntervalSeconds: plan.CaptureIntervalSeconds,
		ReviewIntervalSeconds: plan.ReviewIntervalSeconds, VisionIntervalSeconds: max(30, plan.ReviewIntervalSeconds),
		ChangedAt: now, Reason: "按用户目标的初始计划观察"}
}

func periodicVision(plan MonitoringPlan) bool {
	return (plan.ModelCapabilities == nil || plan.ModelCapabilities.Vision) && slices.Contains(plan.Signals, "vision")
}

func samplingAt(run *Run, now time.Time) SamplingState {
	state := run.Sampling
	if state.Mode == "" {
		state = initialSampling(run.Plan, now)
	}
	if state.Mode == SamplingCandidate && !state.CandidateUntil.IsZero() && !now.Before(state.CandidateUntil) {
		base := initialSampling(run.Plan, now)
		state.Mode, state.CaptureIntervalSeconds, state.ReviewIntervalSeconds = base.Mode, base.CaptureIntervalSeconds, base.ReviewIntervalSeconds
		state.VisionIntervalSeconds = base.VisionIntervalSeconds
		state.CandidateUntil = time.Time{}
		state.Reason = "候选加速已到期，恢复初始节奏"
	}
	if state.NextEventAt != nil && state.NextEventAt.Sub(now) <= 2*time.Minute && now.Sub(*state.NextEventAt) < 2*time.Minute {
		state.Mode, state.CaptureIntervalSeconds, state.ReviewIntervalSeconds = SamplingCandidate, 6, 15
		state.VisionIntervalSeconds = 30
		state.CandidateUntil = state.NextEventAt.Add(2 * time.Minute)
		state.Reason = "临近已知事件时间，加快检查"
	}
	return state
}

func visualReviewDue(run *Run, now time.Time) bool {
	state := samplingAt(run, now)
	return periodicVision(run.Plan) && (state.LastVisualAttemptAt.IsZero() || now.Sub(state.LastVisualAttemptAt) >= time.Duration(state.VisionIntervalSeconds)*time.Second)
}

func applySamplingDecision(run *Run, decision UnexpectedEventDecision, now time.Time) {
	if decision.Failure != "" || reviewDecisionIncomplete(decision) {
		run.Sampling.StableReviews = 0
		return
	}
	run.Sampling = samplingAt(run, now)
	proposal := decision.Sampling
	if proposal == nil {
		run.Sampling.StableReviews = 0
		return
	}
	validMode := proposal.Mode == SamplingObserving || proposal.Mode == SamplingWaiting || proposal.Mode == SamplingCandidate
	validTime := proposal.NextEventAt == nil || (proposal.NextEventAt.After(now) && proposal.NextEventAt.Sub(now) <= 24*time.Hour)
	if !validMode || !validTime || strings.TrimSpace(proposal.Reason) == "" || len([]rune(proposal.Reason)) > 200 || decision.Confidence < .72 || !hasReviewTool(decision.ToolCalls, "search_observations") {
		run.Sampling.RejectedProposals++
		run.Sampling.StableReviews = 0
		return
	}
	state := &run.Sampling
	if state.Mode == "" {
		*state = initialSampling(run.Plan, now)
	}
	state.NextEventAt = proposal.NextEventAt
	if proposal.Mode == SamplingWaiting {
		state.StableReviews++
		if state.StableReviews < stableReviewsRequired || now.Sub(state.ChangedAt) < samplingCooldown || decision.Action == "alert" {
			return
		}
		// Explicit urgency and short-lived visual events constrain savings.
		if goalNeedsFastPlan(run.Goal) {
			state.Reason = "用户要求及时提醒，保持较快采样"
			return
		}
		ceiling := 24
		if periodicVision(run.Plan) {
			ceiling = 12
		}
		state.CaptureIntervalSeconds = min(ceiling, max(6, state.CaptureIntervalSeconds*2))
		state.ReviewIntervalSeconds = 60
		state.VisionIntervalSeconds = 60
		state.Mode = SamplingWaiting
		state.CandidateUntil = time.Time{}
	} else {
		state.StableReviews = 0
		state.Mode = proposal.Mode
		if proposal.Mode == SamplingCandidate {
			state.CaptureIntervalSeconds, state.ReviewIntervalSeconds, state.VisionIntervalSeconds = 6, 15, 30
			state.CandidateUntil = now.Add(90 * time.Second)
		} else {
			base := initialSampling(run.Plan, now)
			state.CaptureIntervalSeconds, state.ReviewIntervalSeconds, state.VisionIntervalSeconds = base.CaptureIntervalSeconds, base.ReviewIntervalSeconds, base.VisionIntervalSeconds
			state.CandidateUntil = time.Time{}
		}
	}
	state.ChangedAt, state.Reason = now, proposal.Reason
}

func (s *Service) effectiveSampling(run *Run, now time.Time) SamplingState {
	_, reviewer, _ := s.agentConfig()
	effectiveRun := *run
	effectiveRun.Plan = planForReviewer(run.Plan, run.Goal, reviewer)
	state := samplingAt(&effectiveRun, now)
	if s.captureGap >= time.Second {
		state.CaptureIntervalSeconds = max(state.CaptureIntervalSeconds, durationSecondsCeil(s.captureGap))
	}
	_, _, reviewGap := s.agentConfig()
	state.ReviewIntervalSeconds = max(state.ReviewIntervalSeconds, durationSecondsCeil(reviewGap))
	return state
}
