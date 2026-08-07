package desktopapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDesktopWebAppOpensOnlyAfterReadiness(t *testing.T) {
	var ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/status" || !ready.Load() {
			http.Error(response, "not ready", http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	opened := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- waitForDesktopWebApp(t.Context(), server.Client(), server.URL+"/api/status", server.URL+"/lab", 5*time.Millisecond, func(value string) error {
			opened <- value
			return nil
		})
	}()

	select {
	case value := <-opened:
		t.Fatalf("browser opened before readiness: %s", value)
	case <-time.After(30 * time.Millisecond):
	}
	ready.Store(true)

	select {
	case value := <-opened:
		if value != server.URL+"/lab" {
			t.Fatalf("opened URL = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("browser did not open after readiness")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDesktopWebAppWaitStopsWithVM(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitForDesktopWebApp(ctx, &http.Client{}, "http://127.0.0.1:1/api/status", "http://127.0.0.1:1/lab", time.Millisecond, func(string) error {
		t.Fatal("browser opened for a stopped VM")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopWebAppRejectsNonLoopbackURL(t *testing.T) {
	if err := requireLoopbackHTTPURL("http://example.com/lab"); err == nil {
		t.Fatal("accepted non-loopback web app URL")
	}
}
