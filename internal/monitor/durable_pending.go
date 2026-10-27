package monitor

import (
	"context"
	"time"
)

// DurablePendingQueue is implemented by repositories that store admission and
// queued work atomically. Processing is at-least-once across process crashes;
// provider requests themselves cannot be resumed mid-flight.
type DurablePendingQueue interface {
	UpdateAndEnqueuePending(context.Context, string, func(*Run) (pendingSignal, bool, error)) (*Run, error)
	ClaimPending(context.Context, string, uint64, time.Time) (pendingSignal, bool, error)
	FinishPending(context.Context, string, string, bool) error
	RecoverPending(context.Context, string, uint64, time.Time) error
	PendingCounts(context.Context, string) (int, bool, error)
}
