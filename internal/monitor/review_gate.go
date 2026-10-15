package monitor

import (
	"context"
	"sync"
	"time"
)

type reviewWaiter struct {
	runID    string
	interval time.Duration
	ready    chan struct{}
	granted  bool
	release  func()
}

// Ready windows receive slots in FIFO order. A window still in cooldown is
// skipped until eligible so it cannot block other windows.
func (s *Service) dispatchReviewsLocked(now time.Time) {
	for len(s.reviewSlots) < cap(s.reviewSlots) {
		index := -1
		for i, waiter := range s.reviewWaiters {
			previous := s.lastUnexpectedReview[waiter.runID]
			if !s.reviewInFlight[waiter.runID] && (previous.IsZero() || now.Sub(previous) >= waiter.interval) {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		waiter := s.reviewWaiters[index]
		s.reviewWaiters = append(s.reviewWaiters[:index], s.reviewWaiters[index+1:]...)
		s.reviewSlots <- struct{}{}
		s.reviewInFlight[waiter.runID] = true
		s.lastUnexpectedReview[waiter.runID] = now
		waiter.granted = true
		waiter.release = s.reviewReleaseLocked(waiter.runID)
		close(waiter.ready)
	}
}

func (s *Service) reviewReleaseLocked(runID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			<-s.reviewSlots
			delete(s.reviewInFlight, runID)
			// A slow multi-turn investigation also needs a cooldown. Measuring
			// only from its start otherwise keeps the model permanently busy.
			if s.contexts[runID] != nil {
				s.lastUnexpectedReview[runID] = time.Now().UTC()
			}
			s.dispatchReviewsLocked(time.Now().UTC())
			s.mu.Unlock()
		})
	}
}

func (s *Service) awaitFairReview(ctx context.Context, runID string, seconds int) (func(), error) {
	_, _, interval := s.agentConfig()
	if seconds < 0 {
		interval = 0
	}
	if requested := time.Duration(seconds) * time.Second; requested > interval {
		interval = requested
	}
	waiter := &reviewWaiter{runID: runID, interval: interval, ready: make(chan struct{})}
	s.mu.Lock()
	s.reviewWaiters = append(s.reviewWaiters, waiter)
	s.dispatchReviewsLocked(time.Now().UTC())
	s.mu.Unlock()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waiter.ready:
			if err := ctx.Err(); err != nil {
				waiter.release()
				return nil, err
			}
			return waiter.release, nil
		case <-ticker.C:
			s.mu.Lock()
			s.dispatchReviewsLocked(time.Now().UTC())
			s.mu.Unlock()
		case <-ctx.Done():
			s.mu.Lock()
			if waiter.granted {
				release := waiter.release
				s.mu.Unlock()
				release()
			} else {
				for i, queued := range s.reviewWaiters {
					if queued == waiter {
						s.reviewWaiters = append(s.reviewWaiters[:i], s.reviewWaiters[i+1:]...)
						break
					}
				}
				s.dispatchReviewsLocked(time.Now().UTC())
				s.mu.Unlock()
			}
			return nil, ctx.Err()
		}
	}
}
