package worker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"liveguard/internal/domain"
	"liveguard/internal/store"
)

func TestAppendExpectationsAndSummarizeBusinessVerdict(t *testing.T) {
	task := &domain.Task{ExpectedTexts: []string{"活动入口已开启"}, ExpectedLiveStatus: "live"}
	plan := appendExpectations([]domain.CheckSpec{{Key: "reachable", Kind: "page_reachable"}}, task)
	if len(plan) != 3 || plan[1].Kind != "contains_all" || plan[2].Kind != "expected_live_status" {
		t.Fatalf("unexpected plan: %#v", plan)
	}

	verdict, summary := summarizeChecks([]domain.CheckResult{{Status: domain.CheckFailed}, {Status: domain.CheckUnverified}}, false)
	if verdict != "failed" || summary != "发现 1 项不符合用户预期" {
		t.Fatalf("unexpected verdict %q summary %q", verdict, summary)
	}
	verdict, _ = summarizeChecks(nil, true)
	if verdict != "needs_human" {
		t.Fatalf("expected human verdict, got %q", verdict)
	}
}

func TestRetryClassificationAndBackoff(t *testing.T) {
	if !isRetryable(errors.New("browser inspection: Chrome exited")) {
		t.Fatal("browser failures should be retried")
	}
	if !isRetryable(context.DeadlineExceeded) {
		t.Fatal("deadline should be retried")
	}
	if isRetryable(context.Canceled) || isRetryable(errors.New("invalid objective")) {
		t.Fatal("cancellation and validation failures should not be retried")
	}
	if retryDelay(1) != 2*time.Second || retryDelay(3) != 8*time.Second {
		t.Fatalf("unexpected retry delays: %s %s", retryDelay(1), retryDelay(3))
	}
}

func TestFailQueuesTransientErrorUntilAttemptBudgetIsExhausted(t *testing.T) {
	repository, err := store.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := &domain.Task{ID: "task-retry", Status: domain.StatusRunning, Attempt: 1, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}
	if err := repository.Create(task); err != nil {
		t.Fatal(err)
	}
	w := &Worker{store: repository}
	w.fail(task.ID, errors.New("browser inspection: temporary Chrome failure"))
	queued, _ := repository.Get(task.ID)
	if queued.Status != domain.StatusQueued || queued.NextAttemptAt == nil || queued.Error == "" {
		t.Fatalf("expected delayed retry, got %#v", queued)
	}
	if queued.Events[len(queued.Events)-1].Type != "retry" {
		t.Fatal("expected retry event")
	}
	_, err = repository.Update(task.ID, func(current *domain.Task) error {
		current.Status = domain.StatusRunning
		current.Attempt = 3
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.fail(task.ID, errors.New("browser inspection: temporary Chrome failure"))
	exhausted, _ := repository.Get(task.ID)
	if exhausted.Status != domain.StatusFailed || exhausted.Events[len(exhausted.Events)-1].Type != "error" {
		t.Fatalf("expected terminal failure after budget, got %#v", exhausted)
	}
}
