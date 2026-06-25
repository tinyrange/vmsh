package backend

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"j5.nz/cc/client"
)

func TestValidateServerHello(t *testing.T) {
	if err := ValidateServerHello(client.ServerHello{Addr: "127.0.0.1:1234"}, "/tmp/cache"); err != nil {
		t.Fatalf("valid hello rejected: %v", err)
	}

	err := ValidateServerHello(client.ServerHello{Kind: "error", Detail: "boom"}, "/tmp/cache")
	if err == nil {
		t.Fatalf("error hello validation = %v", err)
	}

	err = ValidateServerHello(client.ServerHello{}, "/tmp/cache")
	if err == nil {
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
	if _, err := ReadDaemonState(blankPath); err == nil {
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
	if bundled.Path != exe || len(bundled.Env) != 1 || bundled.Env[0] != InternalVMSHDEnv+"=1" {
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
	launch := CCVMLaunch{Path: "/missing/ccvm"}
	state := DaemonState{Addr: ln.Addr().String(), LaunchKey: DaemonLaunchKey(launch)}
	if err := WriteDaemonState(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var reused DaemonState
	var started bool
	api, err := ConnectCCVMWithOptions(launch, t.TempDir(), statePath, ConnectOptions{
		OnReuse: func(state DaemonState) {
			reused = state
		},
		OnStart: func(DaemonState) {
			started = true
		},
	})
	if err != nil {
		t.Fatalf("connect existing daemon: %v", err)
	}
	if reused.Addr != state.Addr {
		t.Fatalf("reused state = %+v, want %+v", reused, state)
	}
	if started {
		t.Fatal("new daemon callback was called for reused daemon")
	}
	if err := api.HealthCheck(); err != nil {
		t.Fatalf("reused client health check: %v", err)
	}
}

func TestConnectCCVMWithOptionsReusesAuthenticatedDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	requireAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/healthz", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux.HandleFunc("/capabilities", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"test","vm_supported":true}`))
	}))
	mux.HandleFunc("/watchdog/lease", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	mux.HandleFunc("/vm/start", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
	})

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "vmshd.token")
	if err := os.WriteFile(tokenPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	statePath := filepath.Join(dir, "ccvm.json")
	launch := CCVMLaunch{Path: "/missing/ccvm"}
	state := DaemonState{Addr: ln.Addr().String(), Kind: "vmshd", TokenPath: tokenPath, LaunchKey: DaemonLaunchKey(launch)}
	if err := WriteDaemonState(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var reused DaemonState
	api, err := ConnectCCVMWithOptions(launch, t.TempDir(), statePath, ConnectOptions{
		OnReuse: func(state DaemonState) {
			reused = state
		},
	})
	if err != nil {
		t.Fatalf("connect authenticated daemon: %v", err)
	}
	if reused != state {
		t.Fatalf("reused state = %+v, want %+v", reused, state)
	}
	if err := api.HealthCheck(); err != nil {
		t.Fatalf("authenticated health check: %v", err)
	}
}

func TestConnectCCVMWithOptionsReportsNewDaemonStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake daemon is Unix-only")
	}
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

	cacheDir := t.TempDir()
	keepalive := filepath.Join(cacheDir, "keepalive")
	if err := os.WriteFile(keepalive, []byte("1"), 0o600); err != nil {
		t.Fatalf("write keepalive: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(keepalive)
	})

	bin := filepath.Join(t.TempDir(), "fake-ccvm")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '{\"addr\":\"%s\"}'\nwhile [ -f \"$2/keepalive\" ]; do sleep 0.1; done\n", ln.Addr().String())
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ccvm: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "ccvm.json")
	launch := CCVMLaunch{Path: bin}
	var reused bool
	var started DaemonState
	api, err := ConnectCCVMWithOptions(launch, cacheDir, statePath, ConnectOptions{
		OnReuse: func(DaemonState) {
			reused = true
		},
		OnStart: func(state DaemonState) {
			started = state
		},
	})
	if err != nil {
		t.Fatalf("connect new daemon: %v", err)
	}
	if reused {
		t.Fatal("reuse callback was called for new daemon")
	}
	want := DaemonState{Addr: ln.Addr().String(), LaunchKey: DaemonLaunchKey(launch)}
	if started != want {
		t.Fatalf("started state = %+v, want %+v", started, want)
	}
	if err := api.HealthCheck(); err != nil {
		t.Fatalf("new client health check: %v", err)
	}
	written, err := ReadDaemonState(statePath)
	if err != nil {
		t.Fatalf("read written state: %v", err)
	}
	if written != want {
		t.Fatalf("written state = %+v, want %+v", written, want)
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
	if err == nil {
		t.Fatalf("connect legacy daemon error = %v", err)
	}
	if reused {
		t.Fatal("legacy daemon was reused")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file after legacy rejection stat err = %v, want not exist", err)
	}
}

func TestConnectCCVMWithOptionsRejectsMismatchedLaunchState(t *testing.T) {
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
	if err := WriteDaemonState(statePath, DaemonState{Addr: ln.Addr().String(), LaunchKey: DaemonLaunchKey(CCVMLaunch{Path: "/old/ccvm"})}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var reused bool
	_, err = ConnectCCVMWithOptions(CCVMLaunch{Path: "/new/ccvm"}, t.TempDir(), statePath, ConnectOptions{
		OnReuse: func(DaemonState) {
			reused = true
		},
	})
	if err == nil {
		t.Fatalf("connect mismatched daemon error = %v", err)
	}
	if reused {
		t.Fatal("mismatched daemon was reused")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file after mismatched rejection stat err = %v, want not exist", err)
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
	if err == nil {
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
