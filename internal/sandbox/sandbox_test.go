package sandbox

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type fakeCommandRunner struct{}

func (fakeCommandRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "start" {
		return "sandbox ready\nresult=PASS\n", nil
	}
	if len(args) > 0 && args[0] == "inspect" {
		return `{"ExitCode":0,"OOMKilled":false}`, nil
	}
	return "", nil
}

func TestCreateArgsContainIsolationControls(t *testing.T) {
	executor := NewExecutor("golang:1.26")
	args := strings.Join(executor.CreateArgs("safe-name", "echo ok"), " ")
	for _, expected := range []string{"--network none", "--read-only", "--memory 128m", "--memory-swap 128m", "--cpus 0.5", "--pids-limit 32", "--cap-drop ALL", "no-new-privileges", "--user 65534:65534", "golang:1.26"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("missing isolation control %q in %s", expected, args)
		}
	}
}

func TestRunLabelsAppearBeforeContainerImage(t *testing.T) {
	executor := NewExecutor("golang:1.26")
	args := executor.createArgsForRun(&Run{ID: "sbx_order", Attempt: 2}, "safe-name", "echo ok")
	imageIndex, labelIndex := -1, -1
	for index, arg := range args {
		if arg == "golang:1.26" {
			imageIndex = index
		}
		if arg == "--label" && labelIndex == -1 {
			labelIndex = index
		}
	}
	if labelIndex < 0 || imageIndex < 0 || labelIndex > imageIndex {
		t.Fatalf("Docker labels must be options before image: %v", args)
	}
}

func TestConcurrentSubmissionsHaveUniqueIDs(t *testing.T) {
	executor := NewExecutor("golang:1.26")
	executor.Docker = fakeCommandRunner{}
	lab := NewLab(executor)
	const count = 32
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := lab.Submit(ScenarioNormal, "concurrency-test")
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			ids <- run.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("got %d unique ids, want %d", len(seen), count)
	}
}

func TestLabRejectsArbitraryScenario(t *testing.T) {
	lab := NewLab(NewExecutor("golang:1.26"))
	if _, err := lab.Submit("rm_host_files", ""); err == nil {
		t.Fatal("expected arbitrary scenario to be rejected")
	}
}

func TestOutputIsCapped(t *testing.T) {
	output, truncated := truncate(strings.Repeat("x", maxOutputBytes+50))
	if !truncated || len(output) > maxOutputBytes+100 {
		t.Fatalf("output cap failed: truncated=%v length=%d", truncated, len(output))
	}
}
