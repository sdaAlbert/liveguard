package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestShutdownIsLocalPostOnlyAndInvokesHandler(t *testing.T) {
	called := false
	s := New(t.TempDir())
	s.SetShutdownHandler(func() { called = true })
	handler := s.Handler()

	remoteRequest := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	remoteRequest.RemoteAddr = "203.0.113.4:4321"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remoteRequest)
	if remoteResponse.Code != http.StatusForbidden {
		t.Fatalf("remote shutdown status = %d, want %d", remoteResponse.Code, http.StatusForbidden)
	}
	if called {
		t.Fatal("remote request invoked shutdown handler")
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/api/shutdown", nil)
	getRequest.RemoteAddr = "127.0.0.1:4321"
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET shutdown status = %d, want %d", getResponse.Code, http.StatusMethodNotAllowed)
	}
	if called {
		t.Fatal("GET request invoked shutdown handler")
	}

	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.RemoteAddr = "127.0.0.1:4321"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("POST shutdown status = %d, want %d", response.Code, http.StatusAccepted)
	}
	if !called {
		t.Fatal("local POST did not invoke shutdown handler")
	}
}
