package store

import (
	"path/filepath"
	"testing"
	"time"

	"liveguard/internal/domain"
)

func TestRecoverInterruptedTaskAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := &domain.Task{ID: "task-recovery", URL: "https://www.douyin.com/", Objective: "检查页面", Status: domain.StatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := first.Create(task); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != domain.StatusQueued {
		t.Fatalf("expected queued, got %s", recovered.Status)
	}
	if len(recovered.Events) == 0 || recovered.Events[len(recovered.Events)-1].Type != "recovery" {
		t.Fatal("expected a persisted recovery event")
	}
}

func TestSnapshotsAreReturnedAsCopies(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Create(&domain.Task{ID: "task-copy", Status: domain.StatusQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	copy, _ := s.Get("task-copy")
	copy.Status = domain.StatusFailed
	original, _ := s.Get("task-copy")
	if original.Status != domain.StatusQueued {
		t.Fatal("caller mutated repository state without Update")
	}
}

func TestCreateManyPersistsBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tasks := []*domain.Task{
		{ID: "task-batch-1", CampaignID: "campaign-1", Status: domain.StatusQueued, CreatedAt: now, UpdatedAt: now},
		{ID: "task-batch-2", CampaignID: "campaign-1", Status: domain.StatusQueued, CreatedAt: now, UpdatedAt: now},
	}
	if err := s.CreateMany(tasks); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.List()) != 2 {
		t.Fatalf("expected two persisted tasks, got %d", len(reopened.List()))
	}
}
