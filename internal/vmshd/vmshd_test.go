package vmshd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/vmshdprotocol"
	"golang.org/x/net/websocket"
	"j5.nz/cc/client"
)

func TestTokenFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmshd.token")
	token, err := writeTokenFile(path)
	if err != nil {
		t.Fatalf("write token: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("token mode = %o, want 600", got)
		}
		parent, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("stat token directory: %v", err)
		}
		if got := parent.Mode().Perm(); got != 0o700 {
			t.Fatalf("token directory mode = %o, want 700", got)
		}
	}
}

func TestWriteSessionErrorPreservesWrappedDomainStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeSessionError(recorder, fmt.Errorf("update session: %w", sessionError{
		status: http.StatusConflict,
		err:    "session state conflict",
	}))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	var response client.ErrorResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error != "session state conflict" {
		t.Fatalf("error field = %q, want domain error", response.Error)
	}
}

func TestWriteSessionErrorKeepsUnknownErrorsInternal(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeSessionError(recorder, fmt.Errorf("database unavailable"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	var response client.ErrorResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error != "database unavailable" {
		t.Fatalf("error field = %q, want internal error", response.Error)
	}
}

func TestHostShellCommandUsesConfiguredExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host shell lookup is Unix-specific")
	}
	dir := t.TempDir()
	configured := filepath.Join(dir, "fish")
	if err := os.WriteFile(configured, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write configured shell fixture: %v", err)
	}
	t.Setenv("SHELL", configured)
	if got := hostShellCommand(); got != configured {
		t.Fatalf("host shell command = %q, want %q", got, configured)
	}
}

func TestHostShellCommandRejectsNonExecutableConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host shell lookup is Unix-specific")
	}
	dir := t.TempDir()
	configured := filepath.Join(dir, "configured")
	fallback := filepath.Join(dir, "sh")
	if err := os.WriteFile(configured, []byte("not executable\n"), 0o600); err != nil {
		t.Fatalf("write configured shell fixture: %v", err)
	}
	if err := os.WriteFile(fallback, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fallback shell fixture: %v", err)
	}
	t.Setenv("SHELL", configured)
	t.Setenv("PATH", dir)
	if got := hostShellCommand(); got != fallback {
		t.Fatalf("host shell command = %q, want fallback %q", got, fallback)
	}
}

func TestHostShellStartsInVerifiedSessionCWD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture")
	}
	bin := t.TempDir()
	shellPath := filepath.Join(bin, "zsh")
	script := "#!/bin/sh\nwhile IFS= read -r line; do\n  case \"$line\" in\n    pwd) pwd ;;\n    exit) exit 0 ;;\n  esac\ndone\n"
	if err := os.WriteFile(shellPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write shell fixture: %v", err)
	}
	t.Setenv("SHELL", shellPath)
	t.Setenv("PATH", bin)
	requestedCWD := t.TempDir()
	cwd, err := filepath.EvalSymlinks(requestedCWD)
	if err != nil {
		t.Fatalf("resolve cwd fixture: %v", err)
	}
	manager := newHostShellManager()
	shell, err := manager.Start("sess_cwd", requestedCWD, nil)
	if err != nil {
		t.Fatalf("start host shell: %v", err)
	}
	t.Cleanup(func() { manager.Close("sess_cwd") })
	if shell.cwd != cwd || shell.cmd.Dir != cwd {
		t.Fatalf("shell cwd=%q cmd.Dir=%q, want %q", shell.cwd, shell.cmd.Dir, cwd)
	}
	output, _, unsubscribe := shell.Subscribe()
	defer unsubscribe()
	if err := shell.Write([]byte("pwd\n")); err != nil {
		t.Fatalf("write pwd: %v", err)
	}
	var received bytes.Buffer
	deadline := time.After(5 * time.Second)
	for {
		select {
		case chunk := <-output:
			received.Write(chunk)
			for _, line := range strings.Split(strings.ReplaceAll(received.String(), "\r\n", "\n"), "\n") {
				if strings.TrimSpace(line) == cwd {
					return
				}
			}
		case <-deadline:
			t.Fatalf("pwd output lines = %q, want cwd %q", received.String(), cwd)
		}
	}
}

func TestHostShellRejectsMissingSessionCWD(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := newHostShellManager().Start("sess_missing_cwd", missing, nil)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("start error = %v, want missing cwd", err)
	}
}

func TestAuthenticateRequiresBearerToken(t *testing.T) {
	srv := NewServer("secret")
	handler := srv.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, tc := range []struct {
		name   string
		header string
		status int
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Basic secret", status: http.StatusUnauthorized},
		{name: "wrong token", header: "Bearer nope", status: http.StatusUnauthorized},
		{name: "valid", header: "Bearer secret", status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d", rr.Code, tc.status)
			}
		})
	}
}

func TestAuthenticateWrapsRuntimeAndVMSHRoutes(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv.RegisterHandlers(mux, fakeRuntimeView{})
	handler := srv.Authenticate(mux)

	for _, tc := range []struct {
		name   string
		target string
		auth   string
		status int
	}{
		{name: "runtime route missing token", target: "/healthz", status: http.StatusUnauthorized},
		{name: "vmsh route missing token", target: "/vmsh/status", status: http.StatusUnauthorized},
		{name: "runtime route accepted token", target: "/healthz", auth: "Bearer secret", status: http.StatusNoContent},
		{name: "vmsh route accepted token", target: "/vmsh/status", auth: "Bearer secret", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d", rr.Code, tc.status)
			}
		})
	}
}

func TestAuthenticateAllowsLegacyCompatibleMutation(t *testing.T) {
	srv := NewServer("secret")
	handler := srv.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/vmsh/frontends", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestAuthenticateRejectsUnsupportedFrontendMutation(t *testing.T) {
	srv := NewServer("secret")
	handler := srv.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/vmsh/frontends", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-VMSH-Frontend-Protocol", "2")
	req.Header.Set("X-VMSH-Frontend-Min-Protocol", "2")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUpgradeRequired)
	}
}

func TestAuthenticateRejectsUnsupportedTerminalWebSocketUpgrade(t *testing.T) {
	srv := NewServer("secret")
	var called bool
	handler := srv.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	req := httptest.NewRequest(http.MethodGet, "/vmsh/sessions/sess_1/attachments/attach_1/stream", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set(vmshdprotocol.HeaderProtocol, "2")
	req.Header.Set(vmshdprotocol.HeaderMinProtocol, "2")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if called {
		t.Fatal("incompatible WebSocket upgrade reached the terminal handler")
	}
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUpgradeRequired)
	}
	if got := recorder.Header().Get(vmshdprotocol.HeaderProtocol); got != fmt.Sprint(vmshdprotocol.Current) {
		t.Fatalf("supported current protocol = %q, want %d", got, vmshdprotocol.Current)
	}
	if got := recorder.Header().Get(vmshdprotocol.HeaderMinProtocol); got != fmt.Sprint(vmshdprotocol.Minimum) {
		t.Fatalf("supported minimum protocol = %q, want %d", got, vmshdprotocol.Minimum)
	}
}

func TestAuthenticateKeepsReadOnlyGetAvailableToUnsupportedFrontend(t *testing.T) {
	srv := NewServer("secret")
	handler := srv.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/vmsh/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(vmshdprotocol.HeaderProtocol, "2")
	req.Header.Set(vmshdprotocol.HeaderMinProtocol, "2")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestEnsureLoopbackAddrArg(t *testing.T) {
	got, err := ensureLoopbackAddrArg([]string{"-cache-dir", "/tmp/cache"})
	if err != nil {
		t.Fatalf("ensure default addr: %v", err)
	}
	if !hasString(got, "-addr") || !hasString(got, "127.0.0.1:0") {
		t.Fatalf("args = %q", got)
	}
	kept, err := ensureLoopbackAddrArg([]string{"--addr=localhost:1234"})
	if err != nil {
		t.Fatalf("keep explicit addr: %v", err)
	}
	if len(kept) != 1 || kept[0] != "--addr=localhost:1234" {
		t.Fatalf("explicit addr changed: %q", kept)
	}
	if _, err := ensureLoopbackAddrArg([]string{"--addr=0.0.0.0:1234"}); err == nil {
		t.Fatal("non-loopback addr was accepted")
	}
}

func TestProtocolEndpointReportsCompatibility(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, fakeRuntimeView{})
	handler := srv.Authenticate(mux)

	req := httptest.NewRequest(http.MethodGet, "/vmsh/protocol?frontend_protocol=1&frontend_min_protocol=1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var info struct {
		Kind     string `json:"kind"`
		Protocol struct {
			Name    string `json:"name"`
			Current int    `json:"current"`
			Minimum int    `json:"minimum"`
		} `json:"protocol"`
		Compatibility struct {
			Compatible bool   `json:"compatible"`
			Action     string `json:"action"`
			Reason     string `json:"reason"`
		} `json:"compatibility"`
		Daemon struct {
			Executable struct {
				Mode string `json:"mode"`
			} `json:"executable"`
		} `json:"daemon"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode protocol: %v", err)
	}
	if info.Kind != Kind || info.Protocol.Name != "vmshd.frontend" || info.Protocol.Current != 1 || info.Protocol.Minimum != 1 {
		t.Fatalf("protocol info = %+v", info)
	}
	if !info.Compatibility.Compatible || info.Compatibility.Action != "reuse" || info.Compatibility.Reason != "compatible" {
		t.Fatalf("compatibility = %+v", info.Compatibility)
	}
	if info.Daemon.Executable.Mode == "" {
		t.Fatal("executable mode was not reported")
	}
}

func TestProtocolEndpointRefusesUnsupportedFrontendRange(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, fakeRuntimeView{})
	handler := srv.Authenticate(mux)

	req := httptest.NewRequest(http.MethodGet, "/vmsh/protocol?frontend_protocol=2&frontend_min_protocol=2", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var info struct {
		Compatibility struct {
			Compatible bool   `json:"compatible"`
			Action     string `json:"action"`
			Reason     string `json:"reason"`
		} `json:"compatibility"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode protocol: %v", err)
	}
	if info.Compatibility.Compatible || info.Compatibility.Action != "start-private-daemon" || info.Compatibility.Reason != "frontend-too-new" {
		t.Fatalf("compatibility = %+v", info.Compatibility)
	}
}

func TestStatusRoute(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	runtime := fakeRuntimeView{statuses: []client.InstanceState{{ID: "vm1", Status: "running"}}}
	srv.RegisterHandlers(mux, runtime)
	session := mustCreateRegistrySession(t, srv.registry, "main")
	updated, err := srv.registry.Update(session.ID, UpdateSessionRequest{
		HostCWD:         "/work",
		SelectedContext: &SessionContext{Mode: "host", Name: "host", Short: "host", Source: "host"},
		VMRefs:          []VMRef{{ID: "dev", BackendID: "dev-isolated", Context: "vm:dev", Image: "debian", Isolated: true}},
		HostShells:      []ShellHandle{{ID: "host", Kind: "host", Name: "host", CWD: "/work", State: "open"}},
		Jobs:            []JobSummary{{ID: 1, Command: "sleep 1", Status: "running", StartedAt: time.Now()}},
		Copies:          []CopySummary{{ID: 1, Source: "@host:a", Dest: "@host:b", Status: "running", StartedAt: time.Now()}},
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/vmsh/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rr.Code, rr.Body.String())
	}
	var status Status
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Kind != Kind || status.Status != "ok" || len(status.Sessions) != 1 {
		t.Fatalf("status = %+v", status)
	}
	if status.Sessions[0].ID != session.ID || status.Sessions[0].Name != "main" || status.Sessions[0].State != "detached" {
		t.Fatalf("status sessions = %+v, want session %+v", status.Sessions, session)
	}
	if status.Sessions[0].HostCWD != updated.HostCWD || status.Sessions[0].SelectedContext == nil || status.Sessions[0].SelectedContext.Mode != "host" {
		t.Fatalf("status session metadata = %+v, want %+v", status.Sessions[0], updated)
	}
	if len(status.Sessions[0].Jobs) != 1 || status.Sessions[0].Jobs[0].Command != "sleep 1" {
		t.Fatalf("status session jobs = %+v", status.Sessions[0].Jobs)
	}
	if len(status.Sessions[0].VMRefs) != 1 || status.Sessions[0].VMRefs[0].BackendID != "dev-isolated" || !status.Sessions[0].VMRefs[0].Isolated {
		t.Fatalf("status session vm refs = %+v", status.Sessions[0].VMRefs)
	}
	if len(status.Sessions[0].Copies) != 1 || status.Sessions[0].Copies[0].Source != "@host:a" || status.Sessions[0].Copies[0].Status != "running" {
		t.Fatalf("status session copies = %+v", status.Sessions[0].Copies)
	}
	if len(status.Sessions[0].HostShells) != 1 || status.Sessions[0].HostShells[0].CWD != "/work" {
		t.Fatalf("status host shells = %+v", status.Sessions[0].HostShells)
	}
	if len(status.VMs) != 1 || status.VMs[0].ID != "vm1" || status.VMs[0].Status != "running" {
		t.Fatalf("status VMs = %+v", status.VMs)
	}
}

func TestSessionRoutesCreateListReadAndDelete(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions", bytes.NewBufferString(`{"name":"main"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created Session
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	if created.ID == "" || created.Name != "main" || created.State != "detached" {
		t.Fatalf("created session = %+v", created)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, "/vmsh/sessions/"+created.ID, bytes.NewBufferString(`{"host_cwd":"/work","selected_context":{"mode":"vm","name":"dev","short":"vm:dev","source":"docker:debian","vm":"dev","image":"debian","cwd":"/repo","user":"root","isolated":true},"vm_refs":[{"id":"dev","backend_id":"dev-isolated","context":"vm:dev","image":"debian","isolated":true}],"host_shells":[{"id":"host","kind":"host","name":"host","cwd":"/work","state":"open"}],"guest_shells":[{"id":"dev","kind":"guest","name":"dev","context":"vm:dev","cwd":"/repo","vm":"dev","user":"root","state":"open"}],"ssh_shells":[{"id":"ssh","kind":"ssh","name":"app","context":"ssh:app","cwd":"/srv","ssh_host":"app","user":"me","state":"open"}],"jobs":[{"id":1,"context":"vm:dev","command":"make","status":"running","control":"vm:dev","logs":"@jobs logs 1","started_at":"2026-06-25T00:00:00Z"}],"copies":[{"id":1,"source":"@host:src","dest":"@host:dst","status":"done","started_at":"2026-06-25T00:00:00Z","finished_at":"2026-06-25T00:00:01Z"}]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", rr.Code, rr.Body.String())
	}
	var updated Session
	if err := json.NewDecoder(rr.Body).Decode(&updated); err != nil {
		t.Fatalf("decode updated session: %v", err)
	}
	if updated.ID != created.ID || updated.HostCWD != "/work" || updated.SelectedContext == nil || updated.SelectedContext.Mode != "vm" || updated.SelectedContext.VMID != "dev" || updated.SelectedContext.CWD != "/repo" || !updated.SelectedContext.Isolated {
		t.Fatalf("updated session = %+v", updated)
	}
	if len(updated.Jobs) != 1 || updated.Jobs[0].Command != "make" || updated.Jobs[0].Status != "running" {
		t.Fatalf("updated jobs = %+v", updated.Jobs)
	}
	if len(updated.VMRefs) != 1 || updated.VMRefs[0].ID != "dev" || updated.VMRefs[0].BackendID != "dev-isolated" || !updated.VMRefs[0].Isolated {
		t.Fatalf("updated vm refs = %+v", updated.VMRefs)
	}
	if len(updated.Copies) != 1 || updated.Copies[0].Status != "done" || updated.Copies[0].Dest != "@host:dst" {
		t.Fatalf("updated copies = %+v", updated.Copies)
	}
	if len(updated.HostShells) != 1 || len(updated.GuestShells) != 1 || len(updated.SSHShells) != 1 {
		t.Fatalf("updated shell handles host=%+v guest=%+v ssh=%+v", updated.HostShells, updated.GuestShells, updated.SSHShells)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/sessions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rr.Code, rr.Body.String())
	}
	var sessions []SessionSummary
	if err := json.NewDecoder(rr.Body).Decode(&sessions); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != created.ID || sessions[0].Name != "main" {
		t.Fatalf("sessions = %+v, want created %+v", sessions, created)
	}
	if sessions[0].HostCWD != updated.HostCWD || sessions[0].SelectedContext == nil || sessions[0].SelectedContext.Short != "vm:dev" {
		t.Fatalf("session summary metadata = %+v, want %+v", sessions[0], updated)
	}
	if len(sessions[0].Jobs) != 1 || sessions[0].Jobs[0].Logs != "@jobs logs 1" {
		t.Fatalf("session summary jobs = %+v", sessions[0].Jobs)
	}
	if len(sessions[0].VMRefs) != 1 || sessions[0].VMRefs[0].Context != "vm:dev" {
		t.Fatalf("session summary vm refs = %+v", sessions[0].VMRefs)
	}
	if len(sessions[0].Copies) != 1 || sessions[0].Copies[0].Source != "@host:src" {
		t.Fatalf("session summary copies = %+v", sessions[0].Copies)
	}
	if len(sessions[0].GuestShells) != 1 || sessions[0].GuestShells[0].VMID != "dev" {
		t.Fatalf("session summary guest shells = %+v", sessions[0].GuestShells)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/jobs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("jobs status = %d body=%s", rr.Code, rr.Body.String())
	}
	var jobs []JobSummary
	if err := json.NewDecoder(rr.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].SessionID != created.ID || jobs[0].Command != "make" {
		t.Fatalf("jobs = %+v", jobs)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/sessions/"+created.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("read status = %d body=%s", rr.Code, rr.Body.String())
	}
	var read Session
	if err := json.NewDecoder(rr.Body).Decode(&read); err != nil {
		t.Fatalf("decode read session: %v", err)
	}
	if read.ID != created.ID || read.Name != created.Name {
		t.Fatalf("read session = %+v, want %+v", read, created)
	}
	if read.HostCWD != updated.HostCWD || read.SelectedContext == nil || read.SelectedContext.Name != "dev" {
		t.Fatalf("read session metadata = %+v, want %+v", read, updated)
	}
	if len(read.Jobs) != 1 || read.Jobs[0].Context != "vm:dev" {
		t.Fatalf("read session jobs = %+v", read.Jobs)
	}
	if len(read.SSHShells) != 1 || read.SSHShells[0].SSHHost != "app" {
		t.Fatalf("read session ssh shells = %+v", read.SSHShells)
	}

	daemonJob, err := srv.registry.StartJob(created.ID, StartHostJobRequest{Command: []string{"sleep", "30"}, Context: "host"})
	if err != nil {
		t.Fatalf("start daemon job: %v", err)
	}
	srv.registry.SetDaemonHostShell(created.ID, ShellHandle{ID: "host", Kind: "host", Name: "host", CWD: "/daemon", State: "open"})

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/vmsh/sessions/"+created.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	var deleted Session
	if err := json.NewDecoder(rr.Body).Decode(&deleted); err != nil {
		t.Fatalf("decode deleted session: %v", err)
	}
	if deleted.ID != created.ID || deleted.State != "closing" {
		t.Fatalf("deleted session = %+v", deleted)
	}
	if len(deleted.Jobs) != 2 {
		t.Fatalf("deleted jobs = %+v", deleted.Jobs)
	}
	var deletedDaemonJob JobSummary
	for _, job := range deleted.Jobs {
		if job.ID == daemonJob.ID && job.Control == "vmshd" {
			deletedDaemonJob = job
		}
	}
	if deletedDaemonJob.ID != daemonJob.ID || deletedDaemonJob.Status != "canceling" {
		t.Fatalf("deleted daemon job = %+v", deletedDaemonJob)
	}
	if len(deleted.HostShells) != 1 || deleted.HostShells[0].CWD != "/daemon" {
		t.Fatalf("deleted host shells = %+v", deleted.HostShells)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/jobs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("jobs after delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	jobs = nil
	if err := json.NewDecoder(rr.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode jobs after delete: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs after delete = %+v", jobs)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/sessions/"+created.ID, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("read deleted status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFrontendCloseCleansEphemeralSessionsAndKeepsPersistedSessions(t *testing.T) {
	srv := NewServer("secret")
	frontend := srv.registry.RegisterFrontend(RegisterFrontendRequest{Name: "vmsh"})
	ephemeral, err := srv.registry.Create(CreateSessionRequest{Name: "ephemeral", FrontendID: frontend.ID, Scope: "frontend"})
	if err != nil {
		t.Fatalf("create ephemeral session: %v", err)
	}
	persisted, err := srv.registry.Create(CreateSessionRequest{Name: "persisted", FrontendID: frontend.ID, Scope: "frontend"})
	if err != nil {
		t.Fatalf("create persisted session: %v", err)
	}
	persisted, err = srv.registry.Persist(persisted.ID, PersistSessionRequest{Scope: "system"})
	if err != nil {
		t.Fatalf("persist session: %v", err)
	}
	if _, _, err := srv.registry.Attach(ephemeral.ID, AttachSessionRequest{FrontendID: frontend.ID, Mode: "interactive"}); err != nil {
		t.Fatalf("attach ephemeral session: %v", err)
	}
	if _, _, err := srv.registry.Attach(persisted.ID, AttachSessionRequest{FrontendID: frontend.ID, Mode: "observer"}); err != nil {
		t.Fatalf("attach persisted session: %v", err)
	}
	if _, err := srv.registry.Update(ephemeral.ID, UpdateSessionRequest{
		VMRefs: []VMRef{{ID: "dev", BackendID: "dev-isolated", Isolated: true}},
	}); err != nil {
		t.Fatalf("update ephemeral session: %v", err)
	}
	job, err := srv.registry.StartJob(ephemeral.ID, StartHostJobRequest{Command: []string{"sleep", "1"}})
	if err != nil {
		t.Fatalf("start daemon job: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	srv.jobs.Track(job.ID, cancel)

	shutdowns := make(chan string, 1)
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, fakeRuntimeView{
		shutdown: func(_ context.Context, id string) error {
			shutdowns <- id
			return nil
		},
	})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/vmsh/frontends/"+frontend.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("close frontend status = %d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := srv.registry.Get(ephemeral.ID); ok {
		t.Fatalf("ephemeral session still exists after frontend close")
	}
	kept, ok := srv.registry.Get(persisted.ID)
	if !ok {
		t.Fatalf("persisted session was removed")
	}
	if kept.Scope != "system" || !kept.DetachOnClose || len(kept.Attachments) != 0 {
		t.Fatalf("persisted session = %+v", kept)
	}
	select {
	case id := <-shutdowns:
		if id != "dev-isolated" {
			t.Fatalf("shutdown id = %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runtime shutdown")
	}
	if srv.jobs.CancelOne(job.ID) {
		t.Fatalf("daemon job was not canceled during frontend cleanup")
	}
}

func TestSessionDeleteRetainsFailedVMOwnershipForRetry(t *testing.T) {
	srv := NewServer("secret")
	session := mustCreateRegistrySession(t, srv.registry, "main")
	if _, err := srv.registry.Update(session.ID, UpdateSessionRequest{VMRefs: []VMRef{
		{ID: "one", BackendID: "vm-one"},
		{ID: "two", BackendID: "vm-two"},
	}}); err != nil {
		t.Fatalf("add VM refs: %v", err)
	}

	var shutdowns []string
	failTwo := true
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, fakeRuntimeView{shutdown: func(_ context.Context, id string) error {
		shutdowns = append(shutdowns, id)
		if id == "vm-two" && failTwo {
			return errors.New("backend unavailable")
		}
		return nil
	}})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/vmsh/sessions/"+session.ID, nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("first delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	var cleanupErr CleanupErrorResponse
	if err := json.NewDecoder(rr.Body).Decode(&cleanupErr); err != nil {
		t.Fatalf("decode cleanup failure: %v", err)
	}
	if len(cleanupErr.Sessions) != 1 || cleanupErr.Sessions[0].State != "cleanup_failed" || cleanupErr.Sessions[0].Cleanup == nil {
		t.Fatalf("cleanup response = %+v", cleanupErr)
	}
	pending := cleanupErr.Sessions[0]
	if pending.Cleanup.Attempts != 1 || len(pending.Cleanup.Failures) != 1 || pending.Cleanup.Failures[0].VMID != "vm-two" || len(pending.VMRefs) != 1 || pending.VMRefs[0].BackendID != "vm-two" {
		t.Fatalf("pending cleanup = %+v", pending)
	}
	stored, ok := srv.registry.Get(session.ID)
	if !ok || stored.State != "cleanup_failed" || len(stored.VMRefs) != 1 || stored.VMRefs[0].BackendID != "vm-two" {
		t.Fatalf("stored cleanup tombstone = %+v, exists=%v", stored, ok)
	}

	failTwo = false
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/vmsh/sessions/"+session.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("retry delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := srv.registry.Get(session.ID); ok {
		t.Fatal("session ownership remains after confirmed shutdown")
	}
	wantShutdowns := []string{"vm-one", "vm-two", "vm-two"}
	if !reflect.DeepEqual(shutdowns, wantShutdowns) {
		t.Fatalf("shutdowns = %v, want %v", shutdowns, wantShutdowns)
	}
}

func TestFrontendCloseRetainsSessionWhenVMShutdownFails(t *testing.T) {
	srv := NewServer("secret")
	frontend := srv.registry.RegisterFrontend(RegisterFrontendRequest{Name: "vmsh"})
	session, err := srv.registry.Create(CreateSessionRequest{Name: "ephemeral", FrontendID: frontend.ID, Scope: "frontend"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := srv.registry.Update(session.ID, UpdateSessionRequest{VMRefs: []VMRef{{ID: "dev", BackendID: "dev-isolated"}}}); err != nil {
		t.Fatalf("add VM ref: %v", err)
	}
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, fakeRuntimeView{shutdown: func(context.Context, string) error {
		return errors.New("backend unavailable")
	}})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/vmsh/frontends/"+frontend.ID, nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("frontend close status = %d body=%s", rr.Code, rr.Body.String())
	}
	pending, ok := srv.registry.Get(session.ID)
	if !ok || pending.State != "cleanup_failed" || pending.Cleanup == nil || len(pending.VMRefs) != 1 || pending.VMRefs[0].BackendID != "dev-isolated" {
		t.Fatalf("pending frontend cleanup = %+v, exists=%v", pending, ok)
	}
}

func TestFrontendScopedSessionsRequireLiveOwner(t *testing.T) {
	registry := newSessionRegistry()
	_, err := registry.Create(CreateSessionRequest{Name: "ownerless", Scope: "frontend"})
	createErr, ok := err.(sessionError)
	if !ok || createErr.status != http.StatusBadRequest {
		t.Fatalf("ownerless create error = %#v, want bad request", err)
	}

	session, err := registry.Create(CreateSessionRequest{Name: "system", Scope: "system"})
	if err != nil {
		t.Fatalf("create system session: %v", err)
	}
	_, err = registry.Persist(session.ID, PersistSessionRequest{Scope: "frontend"})
	persistErr, ok := err.(sessionError)
	if !ok || persistErr.status != http.StatusBadRequest {
		t.Fatalf("ownerless persist error = %#v, want bad request", err)
	}
	unchanged, ok := registry.Get(session.ID)
	if !ok || unchanged.Scope != "system" || unchanged.FrontendID != "" {
		t.Fatalf("session changed after rejected conversion = %+v", unchanged)
	}

	frontend := registry.RegisterFrontend(RegisterFrontendRequest{Name: "vmsh"})
	owned, err := registry.Create(CreateSessionRequest{Name: "owned", FrontendID: frontend.ID, Scope: "system"})
	if err != nil {
		t.Fatalf("create owned system session: %v", err)
	}
	owned, err = registry.Persist(owned.ID, PersistSessionRequest{Scope: "frontend"})
	if err != nil {
		t.Fatalf("convert session with live owner: %v", err)
	}
	if owned.Scope != "frontend" || owned.FrontendID != frontend.ID || owned.DetachOnClose {
		t.Fatalf("converted session ownership = %+v", owned)
	}
}

func TestFrontendDetachOnCloseSurvivorBecomesSystemOwned(t *testing.T) {
	registry := newSessionRegistry()
	frontend := registry.RegisterFrontend(RegisterFrontendRequest{Name: "vmsh"})
	detach := true
	session, err := registry.Create(CreateSessionRequest{
		Name:          "survivor",
		FrontendID:    frontend.ID,
		Scope:         "frontend",
		DetachOnClose: &detach,
	})
	if err != nil {
		t.Fatalf("create frontend session: %v", err)
	}
	_, cleanups, err := registry.CloseFrontend(frontend.ID)
	if err != nil {
		t.Fatalf("close frontend: %v", err)
	}
	if len(cleanups) != 0 {
		t.Fatalf("detach-on-close session scheduled for cleanup: %+v", cleanups)
	}
	kept, ok := registry.Get(session.ID)
	if !ok {
		t.Fatal("detach-on-close session was removed")
	}
	if kept.Scope != "system" || kept.FrontendID != "" || !kept.DetachOnClose {
		t.Fatalf("surviving session ownership = %+v", kept)
	}
}

func TestSessionAttachDetachRoutes(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := mustCreateRegistrySession(t, srv.registry, "main")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/attach", bytes.NewBufferString(`{"terminal":{"cols":120,"rows":40}}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("attach status = %d body=%s", rr.Code, rr.Body.String())
	}
	var attached AttachSessionResponse
	if err := json.NewDecoder(rr.Body).Decode(&attached); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	if attached.Attachment.ID == "" || attached.Attachment.Mode != "interactive" {
		t.Fatalf("attachment = %+v", attached.Attachment)
	}
	if attached.Attachment.Terminal == nil || attached.Attachment.Terminal.Cols != 120 || attached.Attachment.Terminal.Rows != 40 {
		t.Fatalf("attachment terminal = %+v", attached.Attachment.Terminal)
	}
	if attached.Session.State != "attached" || len(attached.Session.Attachments) != 1 {
		t.Fatalf("attached session = %+v", attached.Session)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/attach", bytes.NewBufferString(`{"mode":"interactive"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("second interactive attach status = %d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/attach", bytes.NewBufferString(`{"mode":"observer"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("observer attach status = %d body=%s", rr.Code, rr.Body.String())
	}
	var observer AttachSessionResponse
	if err := json.NewDecoder(rr.Body).Decode(&observer); err != nil {
		t.Fatalf("decode observer response: %v", err)
	}
	if observer.Attachment.Mode != "observer" || len(observer.Session.Attachments) != 2 {
		t.Fatalf("observer response = %+v", observer)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rr.Code, rr.Body.String())
	}
	var status Status
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(status.Sessions) != 1 || len(status.Sessions[0].AttachedClients) != 2 {
		t.Fatalf("status sessions = %+v", status.Sessions)
	}

	rr = httptest.NewRecorder()
	resizeTarget := "/vmsh/sessions/" + session.ID + "/attachments/" + attached.Attachment.ID + "/terminal"
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, resizeTarget, bytes.NewBufferString(`{"cols":100,"rows":32}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("resize status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resized AttachSessionResponse
	if err := json.NewDecoder(rr.Body).Decode(&resized); err != nil {
		t.Fatalf("decode resize response: %v", err)
	}
	if resized.Attachment.Terminal == nil || resized.Attachment.Terminal.Cols != 100 || resized.Attachment.Terminal.Rows != 32 {
		t.Fatalf("resized attachment = %+v", resized.Attachment)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, resizeTarget, bytes.NewBufferString(`{"cols":65537,"rows":32}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized resize status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	_, unchanged, ok := srv.registry.GetAttachment(session.ID, attached.Attachment.ID)
	if !ok || unchanged.Terminal == nil || *unchanged.Terminal != (Terminal{Cols: 100, Rows: 32}) {
		t.Fatalf("attachment changed after oversized resize: %+v", unchanged)
	}

	rr = httptest.NewRecorder()
	body := `{"attachment_id":"` + attached.Attachment.ID + `"}`
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/detach", bytes.NewBufferString(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("detach status = %d body=%s", rr.Code, rr.Body.String())
	}
	var detached Session
	if err := json.NewDecoder(rr.Body).Decode(&detached); err != nil {
		t.Fatalf("decode detached session: %v", err)
	}
	if detached.State != "attached" || len(detached.Attachments) != 1 || detached.Attachments[0].ID != observer.Attachment.ID {
		t.Fatalf("detached session = %+v", detached)
	}

	rr = httptest.NewRecorder()
	body = `{"attachment_id":"` + observer.Attachment.ID + `"}`
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/detach", bytes.NewBufferString(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("detach observer status = %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.NewDecoder(rr.Body).Decode(&detached); err != nil {
		t.Fatalf("decode detached observer session: %v", err)
	}
	if detached.State != "detached" || len(detached.Attachments) != 0 {
		t.Fatalf("fully detached session = %+v", detached)
	}
}

func TestTerminalWinsizeValidatesBeforeConversion(t *testing.T) {
	winsize, err := terminalWinsize(&Terminal{})
	if err != nil {
		t.Fatalf("default winsize: %v", err)
	}
	if winsize.Cols != 80 || winsize.Rows != 24 {
		t.Fatalf("default winsize = %+v", winsize)
	}

	winsize, err = terminalWinsize(&Terminal{Cols: maxTerminalDimension, Rows: maxTerminalDimension})
	if err != nil {
		t.Fatalf("maximum winsize: %v", err)
	}
	if int(winsize.Cols) != maxTerminalDimension || int(winsize.Rows) != maxTerminalDimension {
		t.Fatalf("maximum winsize wrapped: %+v", winsize)
	}

	for _, term := range []Terminal{
		{Cols: -1, Rows: 24},
		{Cols: 80, Rows: -1},
		{Cols: maxTerminalDimension + 1, Rows: 24},
		{Cols: 80, Rows: maxTerminalDimension + 1},
	} {
		if _, err := terminalWinsize(&term); err == nil {
			t.Fatalf("terminalWinsize(%+v) returned no error", term)
		}
	}
}

func TestSessionAttachRejectsBadRequests(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := mustCreateRegistrySession(t, srv.registry, "main")

	for _, tc := range []struct {
		name   string
		target string
		body   string
		status int
	}{
		{name: "missing session", target: "/vmsh/sessions/sess_missing/attach", body: `{}`, status: http.StatusNotFound},
		{name: "bad mode", target: "/vmsh/sessions/" + session.ID + "/attach", body: `{"mode":"writer"}`, status: http.StatusBadRequest},
		{name: "bad attach json", target: "/vmsh/sessions/" + session.ID + "/attach", body: `{`, status: http.StatusBadRequest},
		{name: "trailing attach json", target: "/vmsh/sessions/" + session.ID + "/attach", body: `{"mode":"interactive"}{}`, status: http.StatusBadRequest},
		{name: "unknown attach field", target: "/vmsh/sessions/" + session.ID + "/attach", body: `{"mode":"interactive","unexpected":true}`, status: http.StatusBadRequest},
		{name: "missing attachment id", target: "/vmsh/sessions/" + session.ID + "/detach", body: `{}`, status: http.StatusBadRequest},
		{name: "missing attachment", target: "/vmsh/sessions/" + session.ID + "/detach", body: `{"attachment_id":"attach_missing"}`, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.target, bytes.NewBufferString(tc.body)))
			if rr.Code != tc.status {
				t.Fatalf("status = %d want %d body=%s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}
}

func TestSessionTerminalUpdateRejectsBadRequests(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := mustCreateRegistrySession(t, srv.registry, "main")
	_, attachment, err := srv.registry.Attach(session.ID, AttachSessionRequest{})
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}

	for _, tc := range []struct {
		name   string
		target string
		body   string
		status int
	}{
		{name: "bad json", target: "/vmsh/sessions/" + session.ID + "/attachments/" + attachment.ID + "/terminal", body: `{`, status: http.StatusBadRequest},
		{name: "missing session", target: "/vmsh/sessions/sess_missing/attachments/" + attachment.ID + "/terminal", body: `{}`, status: http.StatusNotFound},
		{name: "missing attachment", target: "/vmsh/sessions/" + session.ID + "/attachments/attach_missing/terminal", body: `{}`, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.target, bytes.NewBufferString(tc.body)))
			if rr.Code != tc.status {
				t.Fatalf("status = %d want %d body=%s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}
}

func TestDaemonOwnedHostJobRunsDetachedAndUpdatesSessionState(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := mustCreateRegistrySession(t, srv.registry, "main")
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	body := fmt.Sprintf(`{"command":[%q,"-test.run=TestDaemonHostJobHelper","--"],"env":["VMSHD_TEST_HOST_JOB=1","VMSHD_TEST_VALUE=ok"],"context":"host"}`, os.Args[0])
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/vmsh/sessions/"+session.ID+"/jobs", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new job request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start job status = %d", resp.StatusCode)
	}
	var started JobSummary
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("decode started job: %v", err)
	}
	if started.ID == 0 || started.SessionID != session.ID || started.Status != "running" || started.Context != "host" {
		t.Fatalf("started job = %+v", started)
	}

	jobs := getJobsFromServer(t, httpSrv.URL, "secret")
	if len(jobs) != 1 || jobs[0].ID != started.ID || jobs[0].Status != "running" {
		t.Fatalf("running jobs = %+v", jobs)
	}
	read, ok := srv.registry.Get(session.ID)
	if !ok || read.State != "detached" || len(read.Jobs) != 1 || read.Jobs[0].ID != started.ID {
		t.Fatalf("detached session jobs = %+v ok=%v", read, ok)
	}

	requireEventually(t, func() bool {
		jobs := getJobsFromServer(t, httpSrv.URL, "secret")
		return len(jobs) == 1 && jobs[0].Status == "exited" && jobs[0].FinishedAt.After(jobs[0].StartedAt) && jobs[0].Logs == "daemon-job:ok\n"
	})
}

func TestDaemonOwnedSSHJobRunsAsHostProcess(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := mustCreateRegistrySession(t, srv.registry, "main")
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	body := fmt.Sprintf(`{"kind":"ssh","command":[%q,"-test.run=TestDaemonHostJobHelper","--"],"env":["VMSHD_TEST_HOST_JOB=1","VMSHD_TEST_VALUE=ssh"],"context":"ssh:server"}`, os.Args[0])
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/vmsh/sessions/"+session.ID+"/jobs", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new job request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start job status = %d", resp.StatusCode)
	}
	var started JobSummary
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("decode started job: %v", err)
	}
	if started.ID == 0 || started.SessionID != session.ID || started.Status != "running" || started.Context != "ssh:server" || started.Control != "vmshd" {
		t.Fatalf("started job = %+v", started)
	}

	requireEventually(t, func() bool {
		jobs := getJobsFromServer(t, httpSrv.URL, "secret")
		return len(jobs) == 1 && jobs[0].ID == started.ID && jobs[0].Status == "exited" && jobs[0].Logs == "daemon-job:ssh\n"
	})
}

func TestDaemonOwnedHostJobCanBeCanceled(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := mustCreateRegistrySession(t, srv.registry, "main")
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	body := fmt.Sprintf(`{"command":[%q,"-test.run=TestDaemonHostJobHelper","--"],"env":["VMSHD_TEST_HOST_JOB=1","VMSHD_TEST_SLEEP=10s"],"context":"host"}`, os.Args[0])
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/vmsh/sessions/"+session.ID+"/jobs", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new job request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start job status = %d", resp.StatusCode)
	}
	var started JobSummary
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("decode started job: %v", err)
	}

	req, err = http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/vmsh/sessions/%s/jobs/%d", httpSrv.URL, session.ID, started.ID), nil)
	if err != nil {
		t.Fatalf("new cancel request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cancel job: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel job status = %d", resp.StatusCode)
	}
	var canceling JobSummary
	if err := json.NewDecoder(resp.Body).Decode(&canceling); err != nil {
		t.Fatalf("decode canceled job: %v", err)
	}
	if canceling.ID != started.ID || canceling.Status != "canceling" || canceling.Control != "vmshd" {
		t.Fatalf("canceling job = %+v", canceling)
	}

	requireEventually(t, func() bool {
		jobs := getJobsFromServer(t, httpSrv.URL, "secret")
		return len(jobs) == 1 && jobs[0].ID == started.ID && jobs[0].Status == "canceled" && !jobs[0].FinishedAt.IsZero()
	})
}

func TestDaemonOwnedVMJobRunsThroughRuntime(t *testing.T) {
	runSeen := make(chan client.RunRequest, 1)
	runtime := fakeRuntimeView{
		runStream: func(_ context.Context, id string, req client.RunRequest, _ <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
			if id != "dev" {
				t.Fatalf("runtime id = %q", id)
			}
			runSeen <- req
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "vm-job:ok\n"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		},
	}
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, runtime)
	session := mustCreateRegistrySession(t, srv.registry, "main")
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	body := `{"kind":"vm","vm":"dev","context":"vm:dev","run":{"image":"ubuntu","command":["sh","-lc","printf ok"],"workdir":"/work","user":"1000:1000"}}`
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/vmsh/sessions/"+session.ID+"/jobs", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new job request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start job status = %d", resp.StatusCode)
	}
	var started JobSummary
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("decode started job: %v", err)
	}
	if started.ID == 0 || started.Context != "vm:dev" || started.Control != "vmshd" || started.Status != "running" {
		t.Fatalf("started job = %+v", started)
	}
	select {
	case got := <-runSeen:
		if got.Image != "ubuntu" || got.WorkDir != "/work" || got.User != "1000:1000" {
			t.Fatalf("runtime request = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runtime run")
	}
	requireEventually(t, func() bool {
		jobs := getJobsFromServer(t, httpSrv.URL, "secret")
		return len(jobs) == 1 && jobs[0].ID == started.ID && jobs[0].Status == "exited" && jobs[0].Logs == "vm-job:ok\n"
	})
}

func TestCompletedJobRetentionNeverPrunesActiveJobs(t *testing.T) {
	registry := newSessionRegistry()
	registry.maxCompletedJobs = 2
	session := mustCreateRegistrySession(t, registry, "main")
	active, err := registry.StartJob(session.ID, StartHostJobRequest{Command: []string{"active"}})
	if err != nil {
		t.Fatalf("start active job: %v", err)
	}
	var completed []JobSummary
	for i := 0; i < 3; i++ {
		job, err := registry.StartJob(session.ID, StartHostJobRequest{Command: []string{fmt.Sprintf("job-%d", i)}})
		if err != nil {
			t.Fatalf("start completed job: %v", err)
		}
		finished, ok := registry.FinishJob(session.ID, job.ID, JobSummary{Status: "done", Logs: strings.Repeat("x", maxJobLogBytes), FinishedAt: time.Unix(int64(i+1), 0)})
		if !ok {
			t.Fatalf("finish job %d", job.ID)
		}
		completed = append(completed, finished)
	}
	jobs := registry.Jobs()
	wantIDs := []int{active.ID, completed[1].ID, completed[2].ID}
	gotIDs := make([]int, 0, len(jobs))
	for _, job := range jobs {
		gotIDs = append(gotIDs, job.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("retained job IDs = %v, want %v", gotIDs, wantIDs)
	}
	if jobs[0].Status != "running" {
		t.Fatalf("active job = %+v", jobs[0])
	}
}

func TestDaemonHostJobHelper(t *testing.T) {
	if os.Getenv("VMSHD_TEST_HOST_JOB") != "1" {
		return
	}
	sleep := 100 * time.Millisecond
	if value := os.Getenv("VMSHD_TEST_SLEEP"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			t.Fatalf("parse VMSHD_TEST_SLEEP: %v", err)
		}
		sleep = parsed
	}
	time.Sleep(sleep)
	fmt.Fprintf(os.Stdout, "daemon-job:%s\n", os.Getenv("VMSHD_TEST_VALUE"))
	os.Exit(0)
}

func TestTerminalAttachmentStreamTracksActiveStreamAndResize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host PTY shell test requires a Unix shell")
	}
	t.Setenv("SHELL", "/bin/sh")
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := mustCreateRegistrySession(t, srv.registry, "main")
	_, attachment, err := srv.registry.Attach(session.ID, AttachSessionRequest{Terminal: &Terminal{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	target := strings.Replace(httpSrv.URL, "http://", "ws://", 1) + "/vmsh/sessions/" + session.ID + "/attachments/" + attachment.ID + "/stream"
	cfg, err := websocket.NewConfig(target, httpSrv.URL)
	if err != nil {
		t.Fatalf("websocket config: %v", err)
	}
	cfg.Header.Set("Authorization", "Bearer secret")
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("dial stream: %v", err)
	}
	defer ws.Close()

	var attached TerminalStreamMessage
	if err := websocket.JSON.Receive(ws, &attached); err != nil {
		t.Fatalf("receive attached message: %v", err)
	}
	if attached.Kind != "attached" || attached.Stream == nil || attached.Stream.Kind != "terminal" || attached.Stream.SessionID != session.ID || attached.Stream.AttachmentID != attachment.ID {
		t.Fatalf("attached message = %+v", attached)
	}

	status := getStatusFromServer(t, httpSrv.URL, "secret")
	if len(status.Streams) != 1 || status.Streams[0].Kind != "terminal" || status.Streams[0].AttachmentID != attachment.ID {
		t.Fatalf("status streams = %+v", status.Streams)
	}
	requireEventually(t, func() bool {
		status := getStatusFromServer(t, httpSrv.URL, "secret")
		return len(status.Sessions) == 1 && len(status.Sessions[0].HostShells) == 1 && status.Sessions[0].HostShells[0].State == "open"
	})

	if err := websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "resize", Terminal: &Terminal{Cols: 100, Rows: 40}}); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	requireEventually(t, func() bool {
		read, ok := srv.registry.Get(session.ID)
		return ok && len(read.Attachments) == 1 && read.Attachments[0].Terminal != nil && read.Attachments[0].Terminal.Cols == 100 && read.Attachments[0].Terminal.Rows == 40
	})

	if err := websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "stdin", Data: []byte("printf '__vmshd_pty_ok__\\n'\nexit\n")}); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	if got := receiveTerminalDataUntil(t, ws, "__vmshd_pty_ok__"); !strings.Contains(got, "__vmshd_pty_ok__") {
		t.Fatalf("terminal output = %q", got)
	}

	if err := websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "close"}); err != nil {
		t.Fatalf("send close: %v", err)
	}
	_ = ws.Close()
	requireEventually(t, func() bool {
		return len(getStatusFromServer(t, httpSrv.URL, "secret").Streams) == 0
	})
}

func TestTerminalStreamRegistryAllowsOneWriterPerAttachment(t *testing.T) {
	registry := newStreamRegistry()
	first, closeFirst, err := registry.Open("terminal", "session", "attachment", "", nil)
	if err != nil || first.ID == "" {
		t.Fatalf("open first stream: stream=%+v err=%v", first, err)
	}
	if _, _, err := registry.Open("terminal", "session", "attachment", "", nil); err == nil {
		t.Fatal("second writer stream was accepted")
	}
	closeFirst()
	if _, closeSecond, err := registry.Open("terminal", "session", "attachment", "", nil); err != nil {
		t.Fatalf("reopen after close: %v", err)
	} else {
		closeSecond()
	}
}

func TestHostShellSubscriberReceivesEveryOutputChunkUnderBackpressure(t *testing.T) {
	shell := &hostShell{subscribers: map[int]*hostShellSubscriber{}}
	output, done, unsubscribe := shell.Subscribe()
	defer unsubscribe()

	const chunks = 128
	published := make(chan struct{})
	go func() {
		defer close(published)
		for i := 0; i < chunks; i++ {
			shell.publish([]byte{byte(i)})
		}
	}()
	for i := 0; i < chunks; i++ {
		select {
		case data := <-output:
			if len(data) != 1 || data[0] != byte(i) {
				t.Fatalf("chunk %d = %v", i, data)
			}
		case <-done:
			t.Fatalf("subscriber closed after %d chunks", i)
		case <-time.After(time.Second):
			t.Fatalf("timed out after %d chunks", i)
		}
	}
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("publisher remained blocked after subscriber caught up")
	}
}

func TestTerminalStreamsAreRevokedByAttachmentLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host PTY shell test requires a Unix shell")
	}
	for _, lifecycle := range []string{"detach", "delete-session", "close-frontend"} {
		t.Run(lifecycle, func(t *testing.T) {
			srv := NewServer("secret")
			mux := http.NewServeMux()
			srv.RegisterHandlers(mux, nil)
			frontend := srv.registry.RegisterFrontend(RegisterFrontendRequest{Name: "test"})
			session, err := srv.registry.Create(CreateSessionRequest{Name: "main", FrontendID: frontend.ID, Scope: "frontend"})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			_, attachment, err := srv.registry.Attach(session.ID, AttachSessionRequest{FrontendID: frontend.ID, Mode: "interactive"})
			if err != nil {
				t.Fatalf("attach session: %v", err)
			}
			httpSrv := httptest.NewServer(srv.Authenticate(mux))
			defer httpSrv.Close()
			t.Cleanup(func() { srv.shells.Close(session.ID) })

			target := strings.Replace(httpSrv.URL, "http://", "ws://", 1) + "/vmsh/sessions/" + session.ID + "/attachments/" + attachment.ID + "/stream"
			cfg, err := websocket.NewConfig(target, httpSrv.URL)
			if err != nil {
				t.Fatalf("websocket config: %v", err)
			}
			cfg.Header.Set("Authorization", "Bearer secret")
			ws, err := websocket.DialConfig(cfg)
			if err != nil {
				t.Fatalf("dial stream: %v", err)
			}
			defer ws.Close()
			var attached TerminalStreamMessage
			if err := websocket.JSON.Receive(ws, &attached); err != nil {
				t.Fatalf("receive attached message: %v", err)
			}

			method := http.MethodPost
			path := "/vmsh/sessions/" + session.ID + "/detach"
			body := bytes.NewBufferString(fmt.Sprintf(`{"attachment_id":%q}`, attachment.ID))
			switch lifecycle {
			case "delete-session":
				method = http.MethodDelete
				path = "/vmsh/sessions/" + session.ID
				body = bytes.NewBuffer(nil)
			case "close-frontend":
				method = http.MethodDelete
				path = "/vmsh/frontends/" + frontend.ID
				body = bytes.NewBuffer(nil)
			}
			req, err := http.NewRequest(method, httpSrv.URL+path, body)
			if err != nil {
				t.Fatalf("new lifecycle request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("lifecycle request: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("lifecycle status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if err := ws.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatalf("set stream deadline: %v", err)
			}
			var msg TerminalStreamMessage
			if err := websocket.JSON.Receive(ws, &msg); err == nil {
				t.Fatalf("revoked stream remained readable: %+v", msg)
			}
			requireEventually(t, func() bool { return len(srv.streams.List()) == 0 })
		})
	}
}

func TestStreamRegistryRevocationIsIdempotentAndOutsideLock(t *testing.T) {
	registry := newStreamRegistry()
	revoked := make(chan struct{}, 1)
	_, closeStream, err := registry.Open("terminal", "sess_1", "attach_1", "fe_1", func() {
		registry.List()
		revoked <- struct{}{}
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	registry.RevokeAttachment("sess_1", "attach_1")
	registry.RevokeAttachment("sess_1", "attach_1")
	closeStream()
	closeStream()
	select {
	case <-revoked:
	default:
		t.Fatal("stream was not revoked")
	}
	select {
	case <-revoked:
		t.Fatal("stream revoke callback ran more than once")
	default:
	}
	if streams := registry.List(); len(streams) != 0 {
		t.Fatalf("streams after revocation = %+v", streams)
	}
}

func TestObserverTerminalStreamIsReadOnlyAndReceivesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host PTY shell test requires a Unix shell")
	}
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := mustCreateRegistrySession(t, srv.registry, "main")
	_, observer, err := srv.registry.Attach(session.ID, AttachSessionRequest{Mode: "observer", Terminal: &Terminal{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatalf("attach observer: %v", err)
	}
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	dial := func(attachmentID string) *websocket.Conn {
		t.Helper()
		target := strings.Replace(httpSrv.URL, "http://", "ws://", 1) + "/vmsh/sessions/" + session.ID + "/attachments/" + attachmentID + "/stream"
		cfg, err := websocket.NewConfig(target, httpSrv.URL)
		if err != nil {
			t.Fatalf("websocket config: %v", err)
		}
		cfg.Header.Set("Authorization", "Bearer secret")
		ws, err := websocket.DialConfig(cfg)
		if err != nil {
			t.Fatalf("dial stream: %v", err)
		}
		var attached TerminalStreamMessage
		if err := websocket.JSON.Receive(ws, &attached); err != nil {
			t.Fatalf("receive attached message: %v", err)
		}
		if attached.Kind != "attached" || attached.Stream == nil || attached.Stream.AttachmentID != attachmentID {
			t.Fatalf("attached message = %+v", attached)
		}
		return ws
	}
	observerWS := dial(observer.ID)
	defer observerWS.Close()

	if err := websocket.JSON.Send(observerWS, TerminalStreamMessage{Kind: "resize", Terminal: &Terminal{Cols: 1, Rows: 1}}); err != nil {
		t.Fatalf("send observer resize: %v", err)
	}
	receiveTerminalMessageKind(t, observerWS, "error")
	if err := websocket.JSON.Send(observerWS, TerminalStreamMessage{Kind: "stdin", Data: []byte("printf '__observer_injected__\\n'\n")}); err != nil {
		t.Fatalf("send observer stdin: %v", err)
	}
	receiveTerminalMessageKind(t, observerWS, "error")

	_, storedObserver, ok := srv.registry.GetAttachment(session.ID, observer.ID)
	if !ok || storedObserver.Terminal == nil || storedObserver.Terminal.Cols != 80 || storedObserver.Terminal.Rows != 24 {
		t.Fatalf("observer terminal changed = %+v", storedObserver.Terminal)
	}
	var shell *hostShell
	requireEventually(t, func() bool {
		srv.shells.mu.Lock()
		shell = srv.shells.shells[session.ID]
		srv.shells.mu.Unlock()
		if shell == nil {
			return false
		}
		shell.mu.Lock()
		defer shell.mu.Unlock()
		return len(shell.subscribers) > 0
	})
	shell.publish([]byte("__observer_can_read__\n"))
	receiveTerminalDataUntil(t, observerWS, "__observer_can_read__")
}

func receiveTerminalMessageKind(t *testing.T, ws *websocket.Conn, want string) TerminalStreamMessage {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set terminal read deadline: %v", err)
	}
	defer ws.SetReadDeadline(time.Time{})
	for {
		var msg TerminalStreamMessage
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			t.Fatalf("receive terminal message: %v", err)
		}
		if msg.Kind == want {
			return msg
		}
	}
}

func receiveTerminalDataUntil(t *testing.T, ws *websocket.Conn, marker string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	if err := ws.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set terminal read deadline: %v", err)
	}
	t.Cleanup(func() {
		_ = ws.SetReadDeadline(time.Time{})
	})
	var out strings.Builder
	for time.Now().Before(deadline) {
		var msg TerminalStreamMessage
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			t.Fatalf("receive terminal data: %v", err)
		}
		if msg.Kind != "data" {
			continue
		}
		out.Write(msg.Data)
		if strings.Contains(out.String(), marker) {
			return out.String()
		}
	}
	t.Fatalf("terminal data marker %q not received; output=%q", marker, out.String())
	return out.String()
}

func TestEventStreamPublishesSessionEvents(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/vmsh/events", nil)
	if err != nil {
		t.Fatalf("new event request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	first := readEvent(t, scanner)
	if first.Kind != "connected" || first.ID == "" || first.At.IsZero() {
		t.Fatalf("connected event = %+v", first)
	}

	createReq, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/vmsh/sessions", bytes.NewBufferString(`{"name":"main"}`))
	if err != nil {
		t.Fatalf("new create request: %v", err)
	}
	createReq.Header.Set("Authorization", "Bearer secret")
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d", createResp.StatusCode)
	}

	event := readEvent(t, scanner)
	if event.Kind != "session_created" || event.ID == "" || event.Session == nil || event.Session.Name != "main" || event.Session.State != "detached" {
		t.Fatalf("session created event = %+v", event)
	}

	body := fmt.Sprintf(`{"command":[%q,"-test.run=TestDaemonHostJobHelper","--"],"env":["VMSHD_TEST_HOST_JOB=1"],"context":"host"}`, os.Args[0])
	jobReq, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/vmsh/sessions/"+event.Session.ID+"/jobs", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new job request: %v", err)
	}
	jobReq.Header.Set("Authorization", "Bearer secret")
	jobReq.Header.Set("Content-Type", "application/json")
	jobResp, err := http.DefaultClient.Do(jobReq)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	defer jobResp.Body.Close()
	if jobResp.StatusCode != http.StatusOK {
		t.Fatalf("job status = %d", jobResp.StatusCode)
	}

	event = readEvent(t, scanner)
	if event.Kind != "job_started" || event.Session == nil || event.Session.ID == "" || event.Job == nil || event.Job.SessionID != event.Session.ID || event.Job.Status != "running" || event.Job.Control != "vmshd" {
		t.Fatalf("job started event = %+v", event)
	}
}

func TestStatusReportsActiveEventStreams(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/vmsh/events", nil)
	if err != nil {
		t.Fatalf("new event request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	connected := readEvent(t, scanner)

	var status Status
	requireEventually(t, func() bool {
		status = getStatusFromServer(t, httpSrv.URL, "secret")
		return len(status.Streams) == 1 && status.Streams[0].LastEventID == connected.ID
	})
	if len(status.Streams) != 1 {
		t.Fatalf("streams = %+v", status.Streams)
	}
	stream := status.Streams[0]
	if stream.ID == "" || stream.Kind != "events" || stream.State != "open" || stream.ConnectedAt.IsZero() || stream.LastEventID != connected.ID {
		t.Fatalf("stream summary = %+v, connected=%+v", stream, connected)
	}

	cancel()
	_ = resp.Body.Close()
	requireEventually(t, func() bool {
		return len(getStatusFromServer(t, httpSrv.URL, "secret").Streams) == 0
	})
}

func TestEventStreamReportsGapForStaleCursor(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	open := func(after string) (*http.Response, *bufio.Scanner) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/vmsh/events?after="+after, nil)
		if err != nil {
			t.Fatalf("new event request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open event stream: %v", err)
		}
		return resp, bufio.NewScanner(resp.Body)
	}

	resp, scanner := open("")
	initial := readEvent(t, scanner)
	resp.Body.Close()
	srv.events.Publish(Event{Kind: "session_updated"})

	resp, scanner = open(initial.ID)
	defer resp.Body.Close()
	connected := readEvent(t, scanner)
	gap := readEvent(t, scanner)
	if gap.Kind != "gap" || gap.After != initial.ID || gap.ID != connected.ID || gap.Refresh != "/vmsh/status" {
		t.Fatalf("gap event = %+v, connected=%+v", gap, connected)
	}
}

func TestEventStreamHeartbeatKeepsIdleStreamActive(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /vmsh/events", func(w http.ResponseWriter, r *http.Request) {
		srv.streamEventsWithTiming(w, r, 10*time.Millisecond, time.Second)
	})
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/vmsh/events", nil)
	if err != nil {
		t.Fatalf("new event request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	connected := readEvent(t, scanner)
	heartbeat := readEvent(t, scanner)
	if heartbeat.Kind != "heartbeat" || heartbeat.ID != connected.ID || heartbeat.At.IsZero() {
		t.Fatalf("heartbeat event = %+v, connected=%+v", heartbeat, connected)
	}
}

func TestEventHubDisconnectsSubscriberInsteadOfDropping(t *testing.T) {
	hub := newEventHub()
	_, disconnected, _, _, unsubscribe := hub.Subscribe("")
	defer unsubscribe()
	for i := 0; i < 17; i++ {
		hub.Publish(Event{Kind: "session_updated"})
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("full event subscriber was not disconnected")
	}
	if streams := hub.Streams(); len(streams) != 0 {
		t.Fatalf("active streams = %+v", streams)
	}
}

func TestEventStreamRequiresAuth(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	handler := srv.Authenticate(mux)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/events", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("event stream status = %d", rr.Code)
	}
}

func getStatusFromServer(t *testing.T, baseURL, token string) Status {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/vmsh/status", nil)
	if err != nil {
		t.Fatalf("new status request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", resp.StatusCode)
	}
	var status Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return status
}

func getJobsFromServer(t *testing.T, baseURL, token string) []JobSummary {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/vmsh/jobs", nil)
	if err != nil {
		t.Fatalf("new jobs request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get jobs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jobs status = %d", resp.StatusCode)
	}
	var jobs []JobSummary
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	return jobs
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func requireEventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func readEvent(t *testing.T, scanner *bufio.Scanner) Event {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("scan event: %v", scanner.Err())
	}
	var event Event
	if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
		t.Fatalf("decode event %q: %v", scanner.Text(), err)
	}
	return event
}

func TestCreateSessionRejectsBadJSON(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions", bytes.NewBufferString("{")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func mustCreateRegistrySession(t *testing.T, registry *sessionRegistry, name string) Session {
	t.Helper()
	session, err := registry.Create(CreateSessionRequest{Name: name})
	if err != nil {
		t.Fatalf("create registry session: %v", err)
	}
	return session
}

type fakeRuntimeView struct {
	statuses  []client.InstanceState
	runStream func(context.Context, string, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
	shutdown  func(context.Context, string) error
	allowPort func(context.Context, string, int) error
}

func (f fakeRuntimeView) InstanceStatuses() []client.InstanceState {
	return f.statuses
}

func (f fakeRuntimeView) RunStreamIn(ctx context.Context, id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	if f.runStream != nil {
		return f.runStream(ctx, id, req, inputs, onEvent)
	}
	return fmt.Errorf("run stream is not configured")
}

func (f fakeRuntimeView) ShutdownInstance(ctx context.Context, id string) error {
	if f.shutdown != nil {
		return f.shutdown(ctx, id)
	}
	return nil
}

func (f fakeRuntimeView) AllowServiceProxyPort(ctx context.Context, id string, port int) error {
	if f.allowPort != nil {
		return f.allowPort(ctx, id, port)
	}
	return nil
}
