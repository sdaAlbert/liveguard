package web

import (
	"bufio"
	"context"
	"liveguard/internal/domain"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEDisconnectReleasesSubscriberAndReconnectReceivesUpdates(t *testing.T) {
	s := New(t.TempDir())
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/events", nil)
		response, err := httpServer.Client().Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		reader := bufio.NewReader(response.Body)
		line, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(line, "ready") {
			response.Body.Close()
			cancel()
			t.Fatalf("no reconnect handshake: %q %v", line, err)
		}
		s.Publish(domain.Event{ID: "event", Type: "monitor_update", At: time.Now().UTC()})
		found := false
		for i := 0; i < 6; i++ {
			line, err = reader.ReadString('\n')
			if err != nil {
				break
			}
			if strings.Contains(line, "event: update") {
				found = true
				break
			}
		}
		response.Body.Close()
		cancel()
		if !found {
			t.Fatalf("no update: %v", err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			s.mu.Lock()
			count := len(s.subscribers)
			s.mu.Unlock()
			if count == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("disconnected subscriber leaked")
			}
			time.Sleep(time.Millisecond)
		}
	}
}
