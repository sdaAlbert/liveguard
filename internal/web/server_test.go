package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"liveguard/internal/browser"
	"liveguard/internal/domain"
	"liveguard/internal/monitor"
	"liveguard/internal/store"
)

func TestValidateExpectedTexts(t *testing.T) {
	texts, err := validateExpectedTexts([]string{" 活动入口 ", "活动入口", "直播中"})
	if err != nil || len(texts) != 2 || texts[0] != "活动入口" {
		t.Fatalf("unexpected validation result %#v: %v", texts, err)
	}
	if _, err := validateExpectedTexts([]string{"1", "2", "3", "4", "5", "6"}); err == nil {
		t.Fatal("expected limit error")
	}
}

func TestMonitorDemoAPI(t *testing.T) {
	taskStore, err := store.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	server := New(taskStore, t.TempDir())
	monitorService := monitor.NewService(monitor.NewMemoryRepository(), nil)
	defer monitorService.StopAll()
	monitorService.SetReplayGap(time.Millisecond)
	server.SetMonitorService(monitorService)

	response := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"target_url":"https://live.douyin.com/123456","goal":"抽奖开始时提醒我"}`)
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/monitors", body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected accepted, got %d: %s", response.Code, response.Body.String())
	}
	var run monitor.Run
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.TargetURL == "" || run.Goal == "" || len(run.Rooms) != 1 {
		t.Fatalf("monitor should preserve the personal watch request: %#v", run)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/monitors/"+run.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("expected monitor details, got %d: %s", response.Code, response.Body.String())
	}
}

func TestCampaignCreationPersistsAllRoomsAndSummarizesLatestRetry(t *testing.T) {
	taskStore, err := store.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	server := New(taskStore, t.TempDir())
	body := bytes.NewBufferString(`{"name":"晚间巡检","urls":["http://127.0.0.1:8080/demo/live?room=1","http://127.0.0.1:8080/demo/live?room=2"],"expected_texts":["海边电台"],"expected_live_status":"live"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/campaigns", body)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("expected created, got %d: %s", response.Code, response.Body.String())
	}
	var created campaignSummary
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Total != 2 || len(created.Tasks) != 2 || len(taskStore.List()) != 2 {
		t.Fatalf("unexpected campaign: %#v", created)
	}
	for _, task := range created.Tasks {
		if task.MaxAttempts != 3 || task.Status != domain.StatusQueued {
			t.Fatalf("unexpected task defaults: %#v", task)
		}
	}

	old := created.Tasks[0]
	retry := *old
	retry.ID = "task-newest-retry"
	retry.ParentTaskID = old.ID
	retry.CreatedAt = old.CreatedAt.Add(time.Second)
	retry.UpdatedAt = retry.CreatedAt
	retry.Status = domain.StatusCompleted
	retry.Report = &domain.Report{Verdict: "passed"}
	if err := taskStore.Create(&retry); err != nil {
		t.Fatal(err)
	}
	summary := summarizeCampaigns(taskStore.List(), true)[0]
	if summary.Total != 2 || summary.Passed != 1 || len(summary.Tasks) != 2 {
		t.Fatalf("retry must replace its room in campaign summary: %#v", summary)
	}
}

func TestTaskCreationRejectsBusyLoginProfileBeforeQueueing(t *testing.T) {
	taskStore, err := store.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	server := New(taskStore, t.TempDir())
	server.SetBrowserSession(func(string) error { return nil }, func() browser.SessionState {
		return browser.SessionState{Configured: true, Open: true}
	})
	body := bytes.NewBufferString(`{"url":"https://live.douyin.com/123","objective":"检查直播间状态","expected_live_status":"live","use_authenticated_session":true}`)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks", body)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("expected conflict, got %d: %s", response.Code, response.Body.String())
	}
	if len(taskStore.List()) != 0 {
		t.Fatal("busy login profile must not create a doomed task")
	}
}
