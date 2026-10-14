package monitor

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrInvestigationNotWaiting = errors.New("investigation is not waiting for user input")

// AnswerInvestigation records a user's answer and resumes only that
// investigation asynchronously. The monitor and its signal queue keep running.
func (s *Service) AnswerInvestigation(ctx context.Context, runID, investigationID, answer string) (*Run, error) {
	answer = strings.TrimSpace(answer)
	if length := len([]rune(answer)); length < 1 || length > 1000 {
		return nil, errors.New("answer must contain 1-1000 characters")
	}
	_, reviewer, _ := s.agentConfig()
	if reviewer == nil {
		return nil, errors.New("AI 判断当前不可用，请检查模型设置")
	}
	updated, err := s.repository.Update(ctx, runID, func(run *Run) error {
		if run.Status != RunRunning {
			return ErrInvalidTransition
		}
		for i := range run.Investigations {
			inv := &run.Investigations[i]
			if inv.ID != investigationID {
				continue
			}
			if inv.Status != InvestigationWaitingUser || inv.UserAnswer != "" {
				return ErrInvestigationNotWaiting
			}
			inv.UserAnswer = answer
			if err := TransitionInvestigation(inv, InvestigationReviewing); err != nil {
				return err
			}
			inv.Conclusion = "已收到补充，Agent 正在结合证据重新判断"
			inv.UpdatedAt = time.Now().UTC()
			return nil
		}
		return ErrNotFound
	})
	if err != nil {
		return nil, err
	}
	s.onUpdate()
	go s.resumeClarifiedInvestigation(runID, investigationID)
	return updated, nil
}

func (s *Service) resumeClarifiedInvestigation(runID, investigationID string) {
	ctx, releaseMonitor := s.monitorContext(context.Background(), runID)
	defer releaseMonitor()
	_, reviewer, _ := s.agentConfig()
	if reviewer == nil {
		s.finishClarifiedInvestigation(runID, investigationID, UnexpectedEventDecision{Failure: "AI 判断当前不可用，请检查模型设置"})
		return
	}
	releaseReview, err := s.awaitReview(ctx, runID, -1)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.finishClarifiedInvestigation(runID, investigationID, UnexpectedEventDecision{Failure: "等待 Agent 继续判断失败，监控仍在运行"})
		}
		return
	}
	defer releaseReview()
	run, err := s.repository.Get(ctx, runID)
	if err != nil || run.Status != RunRunning {
		return
	}
	index := -1
	for i := range run.Investigations {
		if run.Investigations[i].ID == investigationID && run.Investigations[i].Status == InvestigationReviewing {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	inv := run.Investigations[index]
	event, recent := clarifiedEvidence(run, inv)
	if event.ID == "" {
		s.finishClarifiedInvestigation(runID, investigationID, UnexpectedEventDecision{Failure: "原始证据已不可用，补充已保存但本次无法重审"})
		return
	}
	if !currentEvidence(event.ObservedAt, event.ValidUntil, event.GoalVersion, run.GoalVersion, run.GoalUpdatedAt, time.Now().UTC()) {
		s.finishClarifiedWaiting(runID, investigationID, "原始信号已过期，不会基于它发送实时提醒；你的补充保留在这次调查记录中")
		return
	}
	input := UnexpectedEventInput{
		EvidenceAt: time.Now().UTC(), GoalVersion: run.GoalVersion, GoalUpdatedAt: run.GoalUpdatedAt,
		Goal: run.Goal, UserClarification: inv.UserAnswer, Plan: planForReviewer(run.Plan, run.Goal, reviewer),
		RecentEvidence: recent, RecentAlerts: run.Alerts, TriggerFrameObservedAt: event.ObservedAt,
		SearchObservations: func(queryCtx context.Context, query ObservationQuery) (ObservationPage, error) {
			ledger, ok := s.repository.(RunLedger)
			if !ok {
				return ObservationPage{}, errors.New("repository does not support observation search")
			}
			return ledger.SearchObservations(queryCtx, runID, query)
		},
		ArchivedFrame: func(queryCtx context.Context, observationID string) (FrameAsset, error) {
			return s.archivedObservationFrame(queryCtx, runID, observationID)
		},
	}
	decision, reviewErr := reviewer.Review(ctx, input)
	if reviewErr != nil {
		decision.Failure = "AI 暂时无法完成这次复核，监控会继续运行"
		if errors.Is(reviewErr, ErrModelQuotaExhausted) {
			decision.Failure = quotaStopMessage
		}
	} else if reviewDecisionIncomplete(decision) {
		decision.Failure = "本次复核未形成完整结论，监控会继续运行"
	}
	_, err = s.repository.Update(ctx, runID, func(current *Run) error {
		if current.Status != RunRunning || current.GoalVersion != inv.GoalVersion {
			return ErrGoalChanged
		}
		currentIndex := -1
		for i := range current.Investigations {
			if current.Investigations[i].ID == investigationID && current.Investigations[i].Status == InvestigationReviewing {
				currentIndex = i
				break
			}
		}
		if currentIndex < 0 {
			return ErrInvestigationNotWaiting
		}
		if reviewErr == nil && decision.Action == "alert" {
			for _, newer := range current.Events {
				if newer.Sequence > event.Sequence && newer.RoomID == event.RoomID && !newer.ObservedAt.Before(event.ObservedAt) && (pendingPriority(newer) > 0 || numericRevision(event, newer)) {
					decision.Failure = "用户回复期间出现更新的取消、更正或数值变化；旧结论未提交，继续监听新信号"
					break
				}
			}
		}
		applyUnexpectedReviewTo(current, event, decision, investigationID)
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
}

func (s *Service) finishClarifiedInvestigation(runID, investigationID string, decision UnexpectedEventDecision) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		for i := range run.Investigations {
			if run.Investigations[i].ID == investigationID && run.Investigations[i].Status == InvestigationReviewing {
				inv := &run.Investigations[i]
				if err := TransitionInvestigation(inv, InvestigationFailed); err != nil {
					return err
				}
				inv.Conclusion = decision.Failure
				inv.UpdatedAt = time.Now().UTC()
				return nil
			}
		}
		return ErrNotFound
	})
	if err == nil {
		s.onUpdate()
	}
}

func (s *Service) finishClarifiedWaiting(runID, investigationID, conclusion string) {
	_, err := s.repository.Update(context.Background(), runID, func(run *Run) error {
		for i := range run.Investigations {
			if run.Investigations[i].ID == investigationID && run.Investigations[i].Status == InvestigationReviewing {
				inv := &run.Investigations[i]
				if err := TransitionInvestigation(inv, InvestigationWaiting); err != nil {
					return err
				}
				inv.Conclusion = conclusion
				inv.UpdatedAt = time.Now().UTC()
				return nil
			}
		}
		return ErrNotFound
	})
	if err == nil {
		s.onUpdate()
	}
}

func clarifiedEvidence(run *Run, inv Investigation) (SignalEvent, []SignalEvent) {
	byID := make(map[string]SignalEvent, len(run.Events))
	for _, event := range run.Events {
		byID[event.ID] = event
	}
	var candidates []SignalEvent
	for _, evidence := range inv.Evidence {
		event, ok := byID[evidence.EventID]
		if !ok {
			event = SignalEvent{ID: evidence.EventID, RoomID: evidence.RoomID, GoalVersion: evidence.GoalVersion,
				Source: SignalSource(evidence.Source), Text: evidence.Text, Confidence: evidence.Confidence,
				TextRegions: evidence.TextRegions, ObservedAt: evidence.ObservedAt, ValidUntil: evidence.ValidUntil,
				FrameArchiveID: evidence.FrameArchiveID}
		}
		if event.GoalVersion == 0 {
			event.GoalVersion = run.GoalVersion
		}
		candidates = append(candidates, event)
	}
	if len(candidates) == 0 {
		return SignalEvent{}, nil
	}
	primary := 0
	for i, event := range candidates {
		if event.Source == SignalOCR || event.Source == SignalASR {
			primary = i
			break
		}
	}
	recent := []SignalEvent{candidates[primary]}
	for i, event := range candidates {
		if i != primary && len(recent) < 6 {
			recent = append(recent, event)
		}
	}
	return candidates[primary], recent
}
