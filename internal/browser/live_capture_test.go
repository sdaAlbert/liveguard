package browser

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

func TestValidateLiveURLOnlyAllowsConfiguredHosts(t *testing.T) {
	allowed := map[string]bool{"douyin.com": true, "127.0.0.1": true}
	for _, target := range []string{"https://live.douyin.com/123", "http://127.0.0.1:8080/demo/live"} {
		if err := validateLiveURL(target, allowed); err != nil {
			t.Fatalf("expected allowed URL %q: %v", target, err)
		}
	}
	for _, target := range []string{"file:///C:/secret", "https://example.com/live", "javascript:alert(1)"} {
		if err := validateLiveURL(target, allowed); err == nil {
			t.Fatalf("expected URL to be rejected: %q", target)
		}
	}
}

func TestDetectDevToolsConnectionAbort(t *testing.T) {
	if !isDevToolsDisconnected(errors.New("write tcp 127.0.0.1:8001->127.0.0.1:2601: wsasend: An established connection was aborted by the software in your host machine")) {
		t.Fatal("Windows local Chrome DevTools connection abort must be recognized as a stale page")
	}
	if isDevToolsDisconnected(errors.New("Chrome returned an empty screenshot")) {
		t.Fatal("an invalid screenshot response must not be classified as a socket disconnect")
	}
}

func TestFirstMonitorReusesLoginTabAndNavigatesToRequestedURL(t *testing.T) {
	type cdpRequest struct {
		ID     int            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	requests := make(chan cdpRequest, 1)
	server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		var request cdpRequest
		if err := websocket.JSON.Receive(conn, &request); err != nil {
			return
		}
		requests <- request
		_ = websocket.JSON.Send(conn, map[string]any{"id": request.ID, "result": map[string]any{}})
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	config, err := websocket.NewConfig(wsURL, "http://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	loginPage := &livePage{targetID: "login-tab", conn: connection}
	capturer := &LiveCapturer{pages: map[string]*livePage{"__liveguard_login__": loginPage}}
	targetURL := "https://live.douyin.com/room-123"

	page, created, err := capturer.page(context.Background(), "monitor-1", targetURL)
	if err != nil {
		t.Fatalf("reuse login tab: %v", err)
	}
	if page != loginPage || !created {
		t.Fatalf("page() = (%p, %v), want login tab %p and newly assigned monitor page", page, created, loginPage)
	}
	if capturer.pages["monitor-1"] != loginPage || capturer.pages["__liveguard_login__"] != nil {
		t.Fatal("login tab was not transferred to the monitor")
	}

	request := <-requests
	if request.Method != "Page.navigate" || request.Params["url"] != targetURL {
		t.Fatalf("CDP request = %#v, want Page.navigate to %q", request, targetURL)
	}
}
