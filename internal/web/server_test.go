package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"liveguard/internal/browser"
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
