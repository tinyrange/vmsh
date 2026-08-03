package desktopapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"j5.nz/cc/client"
)

func TestGuestSSHUsesTheAccountsPrimaryGroup(t *testing.T) {
	previous := appConfig
	t.Cleanup(func() { appConfig = previous })
	appConfig.ProductName = "NeurodeskAppX"
	appConfig.SSHUser = "jovyan"
	appConfig.SSHHome = "/home/jovyan"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request client.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(request.Command) < 6 || request.Command[4] != "jovyan" || request.Command[5] != "/home/jovyan" {
			t.Errorf("SSH setup command = %q", request.Command)
		}
		if !strings.Contains(request.Command[2], `group=$(id -gn "$user")`) {
			t.Errorf("SSH setup does not resolve the primary group:\n%s", request.Command[2])
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}))
	defer server.Close()

	if err := configureGuestSSH(t.Context(), client.NewClient(server.URL, nil), "ndappx", []byte("ssh-ed25519 test")); err != nil {
		t.Fatal(err)
	}
}

func TestGuestSSHReportsStructuredCommandFailure(t *testing.T) {
	previous := appConfig
	t.Cleanup(func() { appConfig = previous })
	appConfig.ProductName = "NeurodeskAppX"
	appConfig.SSHUser = "jovyan"
	appConfig.SSHHome = "/home/jovyan"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		encoder := json.NewEncoder(w)
		_ = encoder.Encode(client.ExecEvent{Kind: "stderr", Output: "install: invalid group 'jovyan'\n"})
		_ = encoder.Encode(client.ExecEvent{Kind: "exit", ExitCode: 1})
	}))
	defer server.Close()

	err := configureGuestSSH(t.Context(), client.NewClient(server.URL, nil), "ndappx", []byte("ssh-ed25519 test"))
	if err == nil || !strings.Contains(err.Error(), "guest command exited 1: install: invalid group 'jovyan'") {
		t.Fatalf("SSH setup error = %v", err)
	}
	if strings.Contains(err.Error(), "__CCX3_") {
		t.Fatalf("SSH setup exposed internal protocol markers: %v", err)
	}
}
