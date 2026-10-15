package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"

	"liveguard/internal/browser"
	"liveguard/internal/monitor"
)

func TestMonitorDemoAPI(t *testing.T) {
	server := New(t.TempDir())
	monitorService := monitor.NewService(monitor.NewMemoryRepository(), nil)
	defer monitorService.StopAll()
	monitorService.SetReplayGap(time.Millisecond)
	server.SetMonitorService(monitorService)

	response := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"target_url":"https://live.example.com/123456","goal":"抽奖开始时提醒我"}`)
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
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/monitors/"+run.ID+"/goal", bytes.NewBufferString(`{"goal":"聊天区提到决胜局时提醒我"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("expected goal update, got %d: %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Goal != "聊天区提到决胜局时提醒我" {
		t.Fatalf("goal edit should be visible in returned monitor: %#v", run)
	}
}

func TestBrowserSessionOpensTheUserProvidedPlatformURL(t *testing.T) {
	server := New(t.TempDir())
	var opened string
	server.SetBrowserSession(func(target string) error {
		opened = target
		return nil
	}, func() browser.SessionState { return browser.SessionState{Configured: true, Open: true} })

	target := "https://live.platform-b.example/room-2"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/browser/session", bytes.NewBufferString(`{"url":"`+target+`"}`)))
	if response.Code != http.StatusAccepted || opened != target {
		t.Fatalf("browser session should open the supplied platform URL: status=%d opened=%q body=%s", response.Code, opened, response.Body.String())
	}

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/browser/session", bytes.NewBufferString(`{"url":""}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("browser session must require a user-provided URL instead of choosing a platform default: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMonitorOCRMultipartFlowsIntoAgent(t *testing.T) {
	server := New(t.TempDir())
	monitorService := monitor.NewService(monitor.NewMemoryRepository(), nil)
	monitorService.SetTextRecognizer(monitor.StaticTextRecognizer{Text: "新品正式发布，今晚八点开启预售", Confidence: .93})
	defer monitorService.StopAll()
	server.SetMonitorService(monitorService)

	start := httptest.NewRecorder()
	server.Handler().ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/monitors", bytes.NewBufferString(`{"target_url":"https://live.example.com/ocr","goal":"新品正式发布时提醒我"}`)))
	if start.Code != http.StatusAccepted {
		t.Fatalf("expected monitor creation, got %d: %s", start.Code, start.Body.String())
	}
	var run monitor.Run
	if err := json.Unmarshal(start.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("room_id", run.Rooms[0].ID)
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", `form-data; name="image"; filename="frame.png"`)
	partHeader.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/FXsAAAAASUVORK5CYII=")
	_, _ = part.Write(png)
	_ = writer.Close()
	requestBody := append([]byte(nil), body.Bytes()...)
	request := httptest.NewRequest(http.MethodPost, "/api/monitors/"+run.ID+"/ocr", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "ocr-upload-1")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected OCR accepted, got %d: %s", response.Code, response.Body.String())
	}
	var updated monitor.Run
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Events) != 1 || len(updated.Alerts) != 1 {
		t.Fatalf("OCR upload should enter Agent loop and alert: %#v", updated)
	}
	retryBody := bytes.NewReader(requestBody)
	retry := httptest.NewRequest(http.MethodPost, "/api/monitors/"+run.ID+"/ocr", retryBody)
	retry.Header.Set("Content-Type", writer.FormDataContentType())
	retry.Header.Set("Idempotency-Key", "ocr-upload-1")
	retried := httptest.NewRecorder()
	server.Handler().ServeHTTP(retried, retry)
	if retried.Code != http.StatusAccepted {
		t.Fatalf("same upload retry should be accepted: %d %s", retried.Code, retried.Body.String())
	}
	if err := json.Unmarshal(retried.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Events) != 1 || len(updated.Alerts) != 1 {
		t.Fatalf("HTTP retry duplicated the event or alert: %#v", updated)
	}
}

func TestMonitorAudioMultipartFlowsIntoAgent(t *testing.T) {
	server := New(t.TempDir())
	monitorService := monitor.NewService(monitor.NewMemoryRepository(), nil)
	monitorService.SetTranscriber(monitor.StaticTranscriber{Text: "主播说，抽奖倒计时 00:30，福袋开启", Confidence: .8})
	defer monitorService.StopAll()
	server.SetMonitorService(monitorService)

	start := httptest.NewRecorder()
	server.Handler().ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/monitors", bytes.NewBufferString(`{"target_url":"https://live.example.com/asr","goal":"抽奖开始时提醒我","source_mode":"ocr_upload"}`)))
	if start.Code != http.StatusAccepted {
		t.Fatalf("expected monitor creation, got %d: %s", start.Code, start.Body.String())
	}
	var run monitor.Run
	if err := json.Unmarshal(start.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("room_id", run.Rooms[0].ID)
	observedAt := time.Now().Add(-12 * time.Second).UTC().Truncate(time.Millisecond)
	_ = writer.WriteField("observed_at", observedAt.Format(time.RFC3339Nano))
	part, err := writer.CreateFormFile("audio", "clip.webm")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("test audio bytes"))
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/monitors/"+run.ID+"/audio", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected ASR audio accepted, got %d: %s", response.Code, response.Body.String())
	}
	var updated monitor.Run
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Events) != 1 || updated.Events[0].Source != "asr" || updated.Events[0].Text != "主播说，抽奖倒计时 00:30，福袋开启" || !updated.Events[0].ObservedAt.Equal(observedAt) || len(updated.Alerts) != 1 || !updated.Alerts[0].CreatedAt.After(observedAt) {
		t.Fatalf("ASR transcript should enter the same Agent loop: %#v", updated)
	}
}
