package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelRequestRetryPolicyAndBodyReplay(t *testing.T) {
	for _, status := range []int{429, 503, 401, 400} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				if string(body) != "same input" {
					t.Error("request body changed")
				}
				if calls == 1 {
					w.WriteHeader(status)
				} else {
					w.WriteHeader(200)
				}
			}))
			defer server.Close()
			req, _ := http.NewRequestWithContext(context.Background(), "POST", server.URL, strings.NewReader("same input"))
			res, retries, err := doModelRequest(server.Client(), req, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			want := 0
			if status == 429 || status == 503 {
				want = 1
			}
			if retries != want || calls != want+1 {
				t.Fatalf("calls=%d retries=%d", calls, retries)
			}
		})
	}
}

func TestModelRequestRetryBoundAndRetryAfter(t *testing.T) {
	for _, after := range []string{"", "10"} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Retry-After", after)
			w.WriteHeader(429)
		}))
		req, _ := http.NewRequest("POST", server.URL, strings.NewReader("body"))
		res, retries, err := doModelRequest(server.Client(), req, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		server.Close()
		want := 2
		if after == "10" {
			want = 1
		}
		if calls != want || retries != want-1 {
			t.Fatalf("unbounded/throttled retry: %d %d", calls, retries)
		}
	}
}

func TestModelRequestCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	backoffStarted := make(chan struct{})
	client := &http.Client{Transport: retryTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"1"}},
			Body:       retryTestBody{closed: backoffStarted},
			Request:    r,
		}, nil
	})}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://provider.example/v1/messages", strings.NewReader("body"))
	done := make(chan error, 1)
	go func() {
		res, _, err := doModelRequest(client, req, time.Millisecond)
		if res != nil {
			res.Body.Close()
		}
		done <- err
	}()
	select {
	case <-backoffStarted:
	case <-time.After(time.Second):
		t.Fatal("retry response was not closed before backoff")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("backoff ignored cancellation")
	}
	if calls.Load() != 1 {
		t.Fatal("request replayed after cancellation")
	}
}

type retryTestRoundTripper func(*http.Request) (*http.Response, error)

func (f retryTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type retryTestBody struct{ closed chan struct{} }

func (b retryTestBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b retryTestBody) Close() error {
	close(b.closed)
	return nil
}
