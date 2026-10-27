package monitor

import (
	"context"
	"errors"
	"time"
)

type modelCallTraceContextKey struct{}

type modelCallTrace struct {
	ReviewID       string
	AttemptID      string
	Turn           int
	ParentDeadline time.Time
}

func withModelCallTrace(ctx context.Context, reviewID, attemptID string, turn int) context.Context {
	trace := modelCallTraceFromContext(ctx)
	trace.ReviewID, trace.AttemptID, trace.Turn = reviewID, attemptID, turn
	if trace.ParentDeadline.IsZero() {
		trace.ParentDeadline, _ = ctx.Deadline()
	}
	return context.WithValue(ctx, modelCallTraceContextKey{}, trace)
}

func modelCallTraceFromContext(ctx context.Context) modelCallTrace {
	trace, _ := ctx.Value(modelCallTraceContextKey{}).(modelCallTrace)
	return trace
}

func modelTimeoutSource(ctx context.Context, trace modelCallTrace, clientTimeout time.Duration, err error) string {
	if !errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	if ctx.Err() == nil {
		if clientTimeout > 0 {
			return "http_client_timeout"
		}
		return "transport_timeout"
	}
	deadline, ok := ctx.Deadline()
	if ok && !trace.ParentDeadline.IsZero() && deadline.Sub(trace.ParentDeadline) < time.Second && deadline.Sub(trace.ParentDeadline) > -time.Second {
		return "signal_deadline"
	}
	return "agent_review_deadline"
}
