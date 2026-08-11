package desktopapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"j5.nz/cc/client"
)

func TestDesktopWebAppGuestConfigurationOutlivesSystemdReadiness(t *testing.T) {
	var request client.RunRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, incoming *http.Request) {
		if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(client.ExecResponse{})
	}))
	defer server.Close()

	trigger, err := newDesktopWebAppTrigger("http://127.0.0.1:8888/lab")
	if err != nil {
		t.Fatal(err)
	}
	defer trigger.Close()
	if err := trigger.configureGuest(t.Context(), client.NewClient(server.URL, nil), "ndappx"); err != nil {
		t.Fatal(err)
	}
	if request.TimeoutSeconds <= 30 {
		t.Fatalf("guest configuration timeout = %.1fs; systemd readiness alone may take 30s", request.TimeoutSeconds)
	}
}

func TestPrepareDesktopWebAppMakesForwardAndCallbackReachable(t *testing.T) {
	network := &client.NetworkConfig{Enabled: true, AllowInternet: true}
	trigger, err := prepareDesktopWebApp(network, DesktopWebAppConfig{GuestPort: 8888, URLPath: "/lab"})
	if err != nil {
		t.Fatal(err)
	}
	defer trigger.Close()
	if len(network.PortForwards) != 1 {
		t.Fatalf("port forwards = %+v, want one Jupyter forward", network.PortForwards)
	}
	forward := network.PortForwards[0]
	if forward.Protocol != "tcp" || forward.HostAddr != "127.0.0.1" || forward.HostPort <= 0 || forward.GuestPort != 8888 {
		t.Fatalf("Jupyter port forward = %+v", forward)
	}
	if len(network.AllowedServiceProxyPorts) != 1 || network.AllowedServiceProxyPorts[0] != trigger.notifyPort {
		t.Fatalf("callback ports = %v, want %d", network.AllowedServiceProxyPorts, trigger.notifyPort)
	}
}

func TestDesktopWebAppOpensOnlyAfterGuestNotification(t *testing.T) {
	trigger, err := newDesktopWebAppTrigger("http://127.0.0.1:8888/lab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(trigger.Close)

	opened := make(chan string, 1)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		done <- trigger.monitor(ctx, func(value string) error {
			opened <- value
			return nil
		})
	}()

	callbackURL := "http://127.0.0.1:" + strconv.Itoa(trigger.notifyPort) + trigger.notifyPath
	response, err := http.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET callback status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}
	select {
	case value := <-opened:
		t.Fatalf("browser opened without guest POST: %s", value)
	default:
	}

	request, err := http.NewRequest(http.MethodPost, callbackURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("POST callback status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	select {
	case value := <-opened:
		if value != "http://127.0.0.1:8888/lab" {
			t.Fatalf("opened URL = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("browser did not open after guest notification")
	}

	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	select {
	case value := <-opened:
		if value != "http://127.0.0.1:8888/lab" {
			t.Fatalf("reopened URL = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("browser did not reopen after a second guest notification")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDesktopWebAppNotificationUsesGuestServiceProxy(t *testing.T) {
	trigger, err := newDesktopWebAppTrigger("http://127.0.0.1:8888/lab")
	if err != nil {
		t.Fatal(err)
	}
	defer trigger.Close()
	want := "http://service.internal:" + strconv.Itoa(trigger.notifyPort) + trigger.notifyPath
	if got := trigger.guestNotificationURL(); got != want {
		t.Fatalf("guest notification URL = %q, want %q", got, want)
	}
}

func TestDesktopWebAppWaitStopsWithVM(t *testing.T) {
	trigger, err := newDesktopWebAppTrigger("http://127.0.0.1:8888/lab")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := trigger.monitor(ctx, func(string) error {
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
