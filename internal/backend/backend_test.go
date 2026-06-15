package backend

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"j5.nz/cc/client"
)

func TestValidateServerHello(t *testing.T) {
	if err := ValidateServerHello(client.ServerHello{Addr: "127.0.0.1:1234"}, "/tmp/cache"); err != nil {
		t.Fatalf("valid hello rejected: %v", err)
	}

	err := ValidateServerHello(client.ServerHello{Kind: "error", Detail: "boom"}, "/tmp/cache")
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "/tmp/cache") {
		t.Fatalf("error hello validation = %v", err)
	}

	err = ValidateServerHello(client.ServerHello{}, "/tmp/cache")
	if err == nil || !strings.Contains(err.Error(), "without an address") {
		t.Fatalf("missing address validation = %v", err)
	}
}

func TestDaemonStateRoundTripAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ccvm.json")
	if err := WriteDaemonState(path, DaemonState{Addr: "localhost:9999"}); err != nil {
		t.Fatalf("write daemon state: %v", err)
	}
	state, err := ReadDaemonState(path)
	if err != nil {
		t.Fatalf("read daemon state: %v", err)
	}
	if state.Addr != "localhost:9999" {
		t.Fatalf("state addr = %q", state.Addr)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat daemon state: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("daemon state mode = %o, want 600", got)
		}
	}

	blankPath := filepath.Join(t.TempDir(), "blank.json")
	if err := os.WriteFile(blankPath, []byte(`{"addr":"   "}`), 0o600); err != nil {
		t.Fatalf("write blank state: %v", err)
	}
	if _, err := ReadDaemonState(blankPath); err == nil || !strings.Contains(err.Error(), "has no addr") {
		t.Fatalf("blank daemon state error = %v", err)
	}
}

func TestCCVMLaunchHelpers(t *testing.T) {
	launch := CCVMLaunch{Path: "/bin/ccvm", Args: []string{"-addr", "localhost:0"}}
	if got := CCVMLaunchName(launch); got != "/bin/ccvm -addr localhost:0" {
		t.Fatalf("launch name = %q", got)
	}

	exe := filepath.Join("tmp", HostExecutableName("vmsh"))
	candidates := CCVMPathCandidates(exe)
	if len(candidates) != 2 {
		t.Fatalf("candidates = %q", candidates)
	}
	if filepath.Base(candidates[0]) != HostExecutableName("ccvm") {
		t.Fatalf("first candidate = %q", candidates[0])
	}
	if candidates[1] != CompanionExecutablePath(exe, "vm") {
		t.Fatalf("companion candidate = %q", candidates[1])
	}
}

func TestResolveCCVMPathExplicitBundledAndPathFallback(t *testing.T) {
	explicit, err := ResolveCCVMPath("/custom/ccvm", false)
	if err != nil {
		t.Fatalf("resolve explicit ccvm: %v", err)
	}
	if explicit.Path != "/custom/ccvm" || len(explicit.Env) != 0 {
		t.Fatalf("explicit launch = %+v", explicit)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	for _, candidate := range CCVMPathCandidates(exe) {
		if _, err := os.Stat(candidate); err == nil {
			t.Skipf("ccvm companion exists next to test binary, skipping fallback-order test: %s", candidate)
		}
	}

	t.Setenv("PATH", t.TempDir())
	bundled, err := ResolveCCVMPath("", true)
	if err != nil {
		t.Fatalf("resolve bundled ccvm: %v", err)
	}
	if bundled.Path != exe || len(bundled.Env) != 1 || bundled.Env[0] != InternalCCVMEnv+"=1" {
		t.Fatalf("bundled launch = %+v, executable %q", bundled, exe)
	}

	pathDir := t.TempDir()
	ccvmPath := filepath.Join(pathDir, HostExecutableName("ccvm"))
	if err := os.WriteFile(ccvmPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write path ccvm: %v", err)
	}
	t.Setenv("PATH", pathDir)
	fromPath, err := ResolveCCVMPath("", false)
	if err != nil {
		t.Fatalf("resolve ccvm from PATH: %v", err)
	}
	if fromPath.Path != ccvmPath {
		t.Fatalf("PATH launch = %+v, want %s", fromPath, ccvmPath)
	}
}

func TestConnectCCVMWithOptionsReportsDaemonReuse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"test","vm_supported":true}`))
	})
	mux.HandleFunc("/watchdog/lease", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/vm/start", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
	})

	statePath := filepath.Join(t.TempDir(), "ccvm.json")
	state := DaemonState{Addr: ln.Addr().String()}
	if err := WriteDaemonState(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var reused DaemonState
	api, err := ConnectCCVMWithOptions(CCVMLaunch{Path: "/missing/ccvm"}, t.TempDir(), statePath, ConnectOptions{
		OnReuse: func(state DaemonState) {
			reused = state
		},
	})
	if err != nil {
		t.Fatalf("connect existing daemon: %v", err)
	}
	if reused.Addr != state.Addr {
		t.Fatalf("reused state = %+v, want %+v", reused, state)
	}
	if err := api.HealthCheck(); err != nil {
		t.Fatalf("reused client health check: %v", err)
	}
}

func TestConnectCCVMWithOptionsRejectsLegacyDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
	})

	statePath := filepath.Join(t.TempDir(), "ccvm.json")
	if err := WriteDaemonState(statePath, DaemonState{Addr: ln.Addr().String()}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var reused bool
	_, err = ConnectCCVMWithOptions(CCVMLaunch{Path: "/missing/ccvm"}, t.TempDir(), statePath, ConnectOptions{
		OnReuse: func(DaemonState) {
			reused = true
		},
	})
	if err == nil || !strings.Contains(err.Error(), "start ccvm daemon") {
		t.Fatalf("connect legacy daemon error = %v", err)
	}
	if reused {
		t.Fatal("legacy daemon was reused")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file after legacy rejection stat err = %v, want not exist", err)
	}
}

func TestConnectCCVMWithOptionsRejectsCapabilitiesOnlyDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"test","vm_supported":true}`))
	})
	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
	})

	statePath := filepath.Join(t.TempDir(), "ccvm.json")
	if err := WriteDaemonState(statePath, DaemonState{Addr: ln.Addr().String()}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var reused bool
	_, err = ConnectCCVMWithOptions(CCVMLaunch{Path: "/missing/ccvm"}, t.TempDir(), statePath, ConnectOptions{
		OnReuse: func(DaemonState) {
			reused = true
		},
	})
	if err == nil || !strings.Contains(err.Error(), "start ccvm daemon") {
		t.Fatalf("connect capabilities-only daemon error = %v", err)
	}
	if reused {
		t.Fatal("capabilities-only daemon was reused")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file after capabilities-only rejection stat err = %v, want not exist", err)
	}
}

func TestStartDaemonLeaseUsesShortTimeout(t *testing.T) {
	t.Setenv("VMSH_DAEMON_WATCHDOG_TIMEOUT", "")
	t.Setenv("CCX3_DAEMON_WATCHDOG_TIMEOUT", "")

	api := &recordingWatchdogAPI{}
	stop, err := StartDaemonLease(api)
	if err != nil {
		t.Fatalf("start lease: %v", err)
	}
	stop()

	if len(api.created) != 1 {
		t.Fatalf("created leases = %d, want 1", len(api.created))
	}
	if got := api.created[0].TimeoutSeconds; got != 3 {
		t.Fatalf("watchdog timeout = %v, want 3", got)
	}
	if len(api.released) != 1 || api.released[0] != "lease" {
		t.Fatalf("released leases = %q", api.released)
	}
}

type recordingWatchdogAPI struct {
	created  []client.WatchdogLeaseRequest
	fed      []string
	released []string
}

func (a *recordingWatchdogAPI) CreateWatchdogLease(req client.WatchdogLeaseRequest) (client.WatchdogLeaseResponse, error) {
	a.created = append(a.created, req)
	return client.WatchdogLeaseResponse{LeaseID: "lease", TimeoutSeconds: req.TimeoutSeconds}, nil
}

func (a *recordingWatchdogAPI) FeedWatchdogLease(id string) error {
	a.fed = append(a.fed, id)
	return nil
}

func (a *recordingWatchdogAPI) ReleaseWatchdogLease(id string) error {
	a.released = append(a.released, id)
	return nil
}
