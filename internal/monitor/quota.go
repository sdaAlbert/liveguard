package monitor

import (
	"context"
	"log"
	"time"
)

// Quota exhaustion is a terminal account condition, not a Run time limit.
// A new model configuration creates a fresh reviewer; historical binding
// failures must not intentionally terminate a replacement configuration.
func (s *Service) endQuotaExhaustedRuns(triggerID string, failedReviewer UnexpectedEventReviewer) {
	ids := []string{}
	if triggerID != "" {
		ids = append(ids, triggerID)
	}
	binding, shared := failedReviewer.(*AgentReviewer)
	if shared {
		_, current, _ := s.agentConfig()
		if active, ok := current.(*AgentReviewer); !ok || active != binding {
			return
		}
		binding.quotaUnavailable.Store(true)
		s.mu.Lock()
		ids = ids[:0]
		for id := range s.cancels {
			ids = append(ids, id)
		}
		s.mu.Unlock()
	}
	for _, id := range ids {
		if shared {
			_, current, _ := s.agentConfig()
			if active, ok := current.(*AgentReviewer); !ok || active != binding {
				return
			}
		}
		s.mu.Lock()
		queue := s.pending[id]
		s.mu.Unlock()
		var pending *PendingStatus
		if queue != nil {
			status := queue.stats()
			status.Waiting, status.Reviewing = 0, false
			status.LastIssue = quotaStopMessage
			pending = &status
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		updated, err := s.repository.Update(ctx, id, func(run *Run) error {
			if run.Status != RunRunning {
				return nil
			}
			now := time.Now().UTC()
			run.Status = RunFailed
			run.StopReason, run.LastError = quotaStopReason, quotaStopMessage
			run.CompletedAt = &now
			run.Pending = pending
			for i := range run.Rooms {
				run.Rooms[i].Status = "stopped"
			}
			return nil
		})
		cancel()
		if err != nil {
			log.Printf("cannot persist API-quota termination: monitor=%s err=%v", id, err)
			continue
		}
		if updated.Status != RunFailed || updated.StopReason != quotaStopReason {
			continue
		}
		// Stop cancels capture/review/queue ownership while preserving the
		// already committed failed status and explicit termination reason.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, stopErr := s.Stop(stopCtx, id)
		stopCancel()
		if stopErr != nil {
			log.Printf("cannot release quota-terminated monitor: monitor=%s err=%v", id, stopErr)
		}
		if s.capturer != nil {
			s.capturer.Close(id)
		}
	}
}
