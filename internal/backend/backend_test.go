package backend

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"j5.nz/cc/client"
)

func TestValidateServerHello(t *testing.T) {
	if err := ValidateServerHello(client.ServerHello{Addr: "127.0.0.1:1234"}, "/tmp/cache"); err != nil {
		t.Fatalf("valid hello rejected: %v", err)
	}
	if err := ValidateServerHello(client.ServerHello{Addr: "127.0.0.1:1234", Kind: "vmshd", TokenPath: "/tmp/token"}, "/tmp/cache"); err != nil {
		t.Fatalf("valid vmshd hello rejected: %v", err)
	}

	err := ValidateServerHello(client.ServerHello{Kind: "error", Detail: "boom"}, "/tmp/cache")
	if err == nil {
		t.Fatalf("error hello validation = %v", err)
	}

	err = ValidateServerHello(client.ServerHello{}, "/tmp/cache")
	if err == nil {
		t.Fatalf("missing address validation = %v", err)
	}

	err = ValidateServerHello(client.ServerHello{Addr: "127.0.0.1:1234", Kind: "vmshd"}, "/tmp/cache")
	if err == nil {
		t.Fatalf("missing vmshd token path validation = %v", err)
	}

	err = ValidateServerHello(client.ServerHello{Addr: "0.0.0.0:1234", Kind: "vmshd", TokenPath: "/tmp/token"}, "/tmp/cache")
	if err == nil {
		t.Fatal("non-loopback vmshd address was accepted")
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
	if state.Version != DaemonStateVersion || state.APIVersion != DaemonAPIVersion {
		t.Fatalf("state compatibility metadata = %+v", state)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat daemon state: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("daemon state mode = %o, want 600", got)
		}
		parent, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("stat daemon state directory: %v", err)
		}
		if got := parent.Mode().Perm(); got != 0o700 {
			t.Fatalf("daemon state directory mode = %o, want 700", got)
		}
	}

	blankPath := filepath.Join(t.TempDir(), "blank.json")
	if err := os.WriteFile(blankPath, []byte(`{"addr":"   "}`), 0o600); err != nil {
		t.Fatalf("write blank state: %v", err)
	}
	if _, err := ReadDaemonState(blankPath); err == nil {
		t.Fatalf("blank daemon state error = %v", err)
	}

	if runtime.GOOS != "windows" {
		permissivePath := filepath.Join(t.TempDir(), "permissive.json")
		if err := os.WriteFile(permissivePath, []byte(`{"addr":"localhost:9999"}`), 0o644); err != nil {
			t.Fatalf("write permissive state: %v", err)
		}
		if _, err := ReadDaemonState(permissivePath); err == nil {
			t.Fatal("permissive daemon state was accepted")
		}
	}

	badVersionPath := filepath.Join(t.TempDir(), "bad-version.json")
	if err := os.WriteFile(badVersionPath, []byte(`{"addr":"localhost:9999","version":2}`), 0o600); err != nil {
		t.Fatalf("write bad version state: %v", err)
	}
	if _, err := ReadDaemonState(badVersionPath); err == nil {
		t.Fatal("unsupported daemon state version was accepted")
	}

	badAPIPath := filepath.Join(t.TempDir(), "bad-api.json")
	if err := os.WriteFile(badAPIPath, []byte(`{"addr":"localhost:9999","api_version":"old"}`), 0o600); err != nil {
		t.Fatalf("write bad api state: %v", err)
	}
	if _, err := ReadDaemonState(badAPIPath); err == nil {
		t.Fatal("unsupported daemon API version was accepted")
	}
}

func TestEnsureStableVMSHDCopyCreatesExecutableCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, HostExecutableName("vmsh"))
	if err := os.WriteFile(src, []byte("daemon-binary-v1"), 0o755); err != nil {
		t.Fatalf("write source: %v", err)
	}
	cacheDir := filepath.Join(dir, "cache")
	stablePath, err := EnsureStableVMSHDCopy(src, cacheDir)
	if err != nil {
		t.Fatalf("ensure stable copy: %v", err)
	}
	if filepath.Base(stablePath) != HostExecutableName("vmshd") {
		t.Fatalf("stable path = %q", stablePath)
	}
	data, err := os.ReadFile(stablePath)
	if err != nil {
		t.Fatalf("read stable copy: %v", err)
	}
	if string(data) != "daemon-binary-v1" {
		t.Fatalf("stable copy contents = %q", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(stablePath)
		if err != nil {
			t.Fatalf("stat stable copy: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("stable copy mode = %o, want executable", info.Mode().Perm())
		}
	}
}

func TestDaemonChildReapHelper(t *testing.T) {
	mode := os.Getenv("VMSH_TEST_DAEMON_REAP_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	if err := json.NewEncoder(os.Stdout).Encode(client.ServerHello{Addr: "127.0.0.1:1"}); err != nil {
		t.Fatalf("write startup banner: %v", err)
	}
	switch mode {
	case "drain":
		if _, err := io.CopyN(os.Stdout, zeroReader{}, 2*1024*1024); err != nil {
			t.Fatalf("write daemon output: %v", err)
		}
	case "stop":
		time.Sleep(30 * time.Second)
	}
}

func TestStartDaemonCommandDrainsAndReapsSuccessfulChild(t *testing.T) {
	started, err := startDaemonCommand(CCVMLaunch{
		Path: os.Args[0],
		Args: []string{"-test.run=^TestDaemonChildReapHelper$", "--"},
		Env:  []string{"VMSH_TEST_DAEMON_REAP_HELPER=drain"},
	}, t.TempDir())
	if err != nil {
		t.Fatalf("start daemon helper: %v", err)
	}
	var hello client.ServerHello
	if err := json.NewDecoder(started.stdout).Decode(&hello); err != nil {
		started.stop()
		t.Fatalf("decode startup banner: %v", err)
	}
	started.release()
	select {
	case <-started.done:
	case <-time.After(5 * time.Second):
		started.stop()
		t.Fatal("successful daemon child was not drained and reaped")
	}
	started.stop()
	started.stop()
}

func TestStartDaemonCommandStopReapsChildIdempotently(t *testing.T) {
	started, err := startDaemonCommand(CCVMLaunch{
		Path: os.Args[0],
		Args: []string{"-test.run=^TestDaemonChildReapHelper$", "--"},
		Env:  []string{"VMSH_TEST_DAEMON_REAP_HELPER=stop"},
	}, t.TempDir())
	if err != nil {
		t.Fatalf("start daemon helper: %v", err)
	}
	var hello client.ServerHello
	if err := json.NewDecoder(started.stdout).Decode(&hello); err != nil {
		started.stop()
		t.Fatalf("decode startup banner: %v", err)
	}
	started.release()
	done := make(chan struct{})
	go func() {
		started.stop()
		started.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopped daemon child was not reaped")
	}
	select {
	case <-started.done:
	default:
		t.Fatal("Wait completion was not recorded")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
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
	state := normalizeDaemonState(DaemonState{Addr: ln.Addr().String(), LaunchKey: DaemonLaunchKey(launch)})
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
	mux.HandleFunc("/vmsh/status", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"vmshd","status":"ok"}`))
	}))
	mux.HandleFunc("/vmsh/protocol", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"vmshd","protocol":{"name":"vmshd.frontend","current":1,"minimum":1},"daemon":{"version":"test","platform":"test/test","executable":{"mode":"stable-daemon-copy"}},"compatibility":{"compatible":true,"action":"reuse","reason":"compatible"}}`))
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
	state := normalizeDaemonState(DaemonState{Addr: ln.Addr().String(), Kind: "vmshd", TokenPath: tokenPath, LaunchKey: DaemonLaunchKey(launch)})
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

func TestConnectCCVMWithOptionsRejectsVMSHDWithoutTokenPath(t *testing.T) {
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

	statePath := filepath.Join(t.TempDir(), "vmshd.json")
	launch := CCVMLaunch{Path: "/missing/ccvm"}
	state := normalizeDaemonState(DaemonState{Addr: ln.Addr().String(), Kind: "vmshd", LaunchKey: DaemonLaunchKey(launch)})
	if err := WriteDaemonState(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var reused bool
	_, err = ConnectCCVMWithOptions(launch, t.TempDir(), statePath, ConnectOptions{
		OnReuse: func(DaemonState) {
			reused = true
		},
	})
	if err == nil {
		t.Fatalf("connect vmshd without token path error = %v", err)
	}
	if reused {
		t.Fatal("vmshd without token path was reused")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file after vmshd token rejection stat err = %v, want not exist", err)
	}
}

func TestConnectCCVMWithOptionsPreservesIncompatibleVMSHDState(t *testing.T) {
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
	statePath := filepath.Join(dir, "vmshd.json")
	launch := CCVMLaunch{Path: "/missing/ccvm"}
	state := normalizeDaemonState(DaemonState{Addr: ln.Addr().String(), Kind: "vmshd", TokenPath: tokenPath, LaunchKey: DaemonLaunchKey(launch)})
	if err := WriteDaemonState(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var reused bool
	_, err = ConnectCCVMWithOptions(launch, t.TempDir(), statePath, ConnectOptions{
		OnReuse: func(DaemonState) {
			reused = true
		},
	})
	if reused {
		t.Fatal("vmshd without session route was reused")
	}
	if err == nil {
		t.Fatalf("connect vmshd without session route error = %v", err)
	}
	preserved, readErr := ReadDaemonState(statePath)
	if readErr != nil {
		t.Fatalf("read preserved state: %v", readErr)
	}
	if preserved != state {
		t.Fatalf("preserved state = %+v, want %+v", preserved, state)
	}
}

func TestConnectCCVMWithOptionsStartsPrivateDaemonForIncompatibleVMSHD(t *testing.T) {
	oldLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen old: %v", err)
	}
	const oldToken = "old-secret"
	oldMux := http.NewServeMux()
	oldMux.HandleFunc("/healthz", requireBearer(oldToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	oldMux.HandleFunc("/capabilities", requireBearer(oldToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"test","vm_supported":true}`))
	}))
	oldMux.HandleFunc("/watchdog/lease", requireBearer(oldToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	oldMux.HandleFunc("/vm/start", requireBearer(oldToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	oldSrv := &http.Server{Handler: oldMux}
	go func() {
		_ = oldSrv.Serve(oldLn)
	}()
	t.Cleanup(func() {
		_ = oldSrv.Close()
	})

	newLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen new: %v", err)
	}
	const newToken = "new-secret"
	newMux := http.NewServeMux()
	newMux.HandleFunc("/healthz", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	newMux.HandleFunc("/capabilities", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"test","vm_supported":true}`))
	}))
	newMux.HandleFunc("/watchdog/lease", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	newMux.HandleFunc("/vm/start", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	newMux.HandleFunc("/vmsh/status", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"vmshd","status":"ok"}`))
	}))
	newMux.HandleFunc("/vmsh/protocol", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"vmshd","protocol":{"name":"vmshd.frontend","current":1,"minimum":1},"daemon":{"version":"test","platform":"test/test","executable":{"mode":"stable-daemon-copy"}},"compatibility":{"compatible":true,"action":"reuse","reason":"compatible"}}`))
	}))
	newSrv := &http.Server{Handler: newMux}
	go func() {
		_ = newSrv.Serve(newLn)
	}()
	t.Cleanup(func() {
		_ = newSrv.Close()
	})

	dir := t.TempDir()
	oldTokenPath := filepath.Join(dir, "old.token")
	if err := os.WriteFile(oldTokenPath, []byte(oldToken+"\n"), 0o600); err != nil {
		t.Fatalf("write old token: %v", err)
	}
	newTokenPath := filepath.Join(dir, "new.token")
	if err := os.WriteFile(newTokenPath, []byte(newToken+"\n"), 0o600); err != nil {
		t.Fatalf("write new token: %v", err)
	}
	statePath := filepath.Join(dir, "vmshd.json")
	launch := CCVMLaunch{Path: "/stable/vmshd"}
	oldState := normalizeDaemonState(DaemonState{Addr: oldLn.Addr().String(), Kind: "vmshd", TokenPath: oldTokenPath, LaunchKey: DaemonLaunchKey(launch)})
	if err := WriteDaemonState(statePath, oldState); err != nil {
		t.Fatalf("write old state: %v", err)
	}
	restore := stubStartDaemonProcess(t, serverHelloBanner(t, client.ServerHello{Addr: newLn.Addr().String(), Kind: "vmshd", TokenPath: newTokenPath}))
	defer restore()

	var incompatible DaemonState
	var started DaemonState
	api, err := ConnectCCVMWithOptions(launch, dir, statePath, ConnectOptions{
		OnIncompatible: func(state DaemonState, err error) {
			incompatible = state
		},
		OnStart: func(state DaemonState) {
			started = state
		},
	})
	if err != nil {
		t.Fatalf("connect private daemon: %v", err)
	}
	if incompatible != oldState {
		t.Fatalf("incompatible state = %+v, want %+v", incompatible, oldState)
	}
	if started.Addr != newLn.Addr().String() || started.Kind != "vmshd" || started.TokenPath != newTokenPath {
		t.Fatalf("started state = %+v", started)
	}
	preserved, err := ReadDaemonState(statePath)
	if err != nil {
		t.Fatalf("read preserved state: %v", err)
	}
	if preserved != oldState {
		t.Fatalf("state file = %+v, want preserved %+v", preserved, oldState)
	}
	if err := api.HealthCheck(); err != nil {
		t.Fatalf("private daemon health check: %v", err)
	}
}

func TestConnectCCVMWithOptionsStartsPrivateDaemonForUnauthenticatedVMSHDState(t *testing.T) {
	const oldToken = "old-secret"
	const newToken = "new-secret"

	oldLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen old daemon: %v", err)
	}
	oldMux := http.NewServeMux()
	oldMux.HandleFunc("/healthz", requireBearer(oldToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	oldSrv := &http.Server{Handler: oldMux}
	go func() {
		_ = oldSrv.Serve(oldLn)
	}()
	t.Cleanup(func() {
		_ = oldSrv.Close()
	})

	newLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen new daemon: %v", err)
	}
	newMux := http.NewServeMux()
	newMux.HandleFunc("/healthz", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	newMux.HandleFunc("/capabilities", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"test","vm_supported":true}`))
	}))
	newMux.HandleFunc("/watchdog/lease", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	newMux.HandleFunc("/vm/start", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	newMux.HandleFunc("/vmsh/status", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"vmshd","status":"ok"}`))
	}))
	newMux.HandleFunc("/vmsh/protocol", requireBearer(newToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"vmshd","protocol":{"name":"vmshd.frontend","current":1,"minimum":1},"daemon":{"version":"test","platform":"test/test","executable":{"mode":"stable-daemon-copy"}},"compatibility":{"compatible":true,"action":"reuse","reason":"compatible"}}`))
	}))
	newSrv := &http.Server{Handler: newMux}
	go func() {
		_ = newSrv.Serve(newLn)
	}()
	t.Cleanup(func() {
		_ = newSrv.Close()
	})

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "vmshd.token")
	if err := os.WriteFile(tokenPath, []byte("wrong-token\n"), 0o600); err != nil {
		t.Fatalf("write wrong token: %v", err)
	}
	statePath := filepath.Join(dir, "vmshd.json")
	launch := CCVMLaunch{Path: "/stable/vmshd"}
	oldState := normalizeDaemonState(DaemonState{Addr: oldLn.Addr().String(), Kind: "vmshd", TokenPath: tokenPath, LaunchKey: DaemonLaunchKey(launch)})
	if err := WriteDaemonState(statePath, oldState); err != nil {
		t.Fatalf("write old state: %v", err)
	}

	oldStartDaemonProcess := startDaemonProcess
	var startedCache string
	startDaemonProcess = func(_ CCVMLaunch, cacheDir string) (*startedDaemonProcess, error) {
		startedCache = cacheDir
		newTokenPath := filepath.Join(cacheDir, "vmshd.token")
		if err := os.WriteFile(newTokenPath, []byte(newToken+"\n"), 0o600); err != nil {
			return nil, err
		}
		banner := serverHelloBanner(t, client.ServerHello{Addr: newLn.Addr().String(), Kind: "vmshd", TokenPath: newTokenPath})
		return &startedDaemonProcess{
			stdout: io.NopCloser(strings.NewReader(banner)),
			stop:   func() {},
		}, nil
	}
	t.Cleanup(func() {
		startDaemonProcess = oldStartDaemonProcess
	})

	var incompatible DaemonState
	api, err := ConnectCCVMWithOptions(launch, dir, statePath, ConnectOptions{
		OnIncompatible: func(state DaemonState, err error) {
			incompatible = state
		},
	})
	if err != nil {
		t.Fatalf("connect private daemon: %v", err)
	}
	if incompatible != oldState {
		t.Fatalf("incompatible state = %+v, want %+v", incompatible, oldState)
	}
	if !strings.HasPrefix(startedCache, filepath.Join(dir, "private")+string(os.PathSeparator)) {
		t.Fatalf("started cache = %q, want private cache under %q", startedCache, dir)
	}
	preserved, err := ReadDaemonState(statePath)
	if err != nil {
		t.Fatalf("read preserved state: %v", err)
	}
	if preserved != oldState {
		t.Fatalf("state file = %+v, want preserved %+v", preserved, oldState)
	}
	if got, err := os.ReadFile(tokenPath); err != nil || string(got) != "wrong-token\n" {
		t.Fatalf("shared token = %q err=%v, want unchanged wrong token", got, err)
	}
	if err := api.HealthCheck(); err != nil {
		t.Fatalf("private daemon health check: %v", err)
	}
}

func TestConnectCCVMWithOptionsReportsNewDaemonStart(t *testing.T) {
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

	restore := stubStartDaemonProcess(t, `{"addr":"`+ln.Addr().String()+`"}`+"\n")
	defer restore()

	statePath := filepath.Join(t.TempDir(), "ccvm.json")
	launch := CCVMLaunch{Path: "/fake/ccvm"}
	var reused bool
	var started DaemonState
	api, err := ConnectCCVMWithOptions(launch, t.TempDir(), statePath, ConnectOptions{
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
	want := normalizeDaemonState(DaemonState{Addr: ln.Addr().String(), LaunchKey: DaemonLaunchKey(launch)})
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

func TestConnectCCVMWithOptionsRejectsStartedVMSHDWithoutSessionRoute(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	const token = "secret"
	requireAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux := http.NewServeMux()
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

	cacheDir := t.TempDir()
	tokenPath := filepath.Join(cacheDir, "vmshd.token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	restore := stubStartDaemonProcess(t, `{"addr":"`+ln.Addr().String()+`","kind":"vmshd","token_path":"`+tokenPath+`"}`+"\n")
	defer restore()

	statePath := filepath.Join(t.TempDir(), "vmshd.json")
	var started bool
	_, err = ConnectCCVMWithOptions(CCVMLaunch{Path: "/fake/vmshd"}, cacheDir, statePath, ConnectOptions{
		OnStart: func(DaemonState) {
			started = true
		},
	})
	if err == nil {
		t.Fatalf("connect started vmshd without session route error = %v", err)
	}
	if started {
		t.Fatal("start callback was called for route-incomplete vmshd")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file after started vmshd route rejection stat err = %v, want not exist", err)
	}
}

func stubStartDaemonProcess(t *testing.T, banner string) func() {
	t.Helper()
	old := startDaemonProcess
	startDaemonProcess = func(CCVMLaunch, string) (*startedDaemonProcess, error) {
		return &startedDaemonProcess{
			stdout:  io.NopCloser(strings.NewReader(banner)),
			release: func() {},
			stop:    func() {},
		}, nil
	}
	return func() {
		startDaemonProcess = old
	}
}

func serverHelloBanner(t *testing.T, hello client.ServerHello) string {
	t.Helper()
	data, err := json.Marshal(hello)
	if err != nil {
		t.Fatalf("marshal server hello: %v", err)
	}
	return string(data) + "\n"
}

func requireBearer(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
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
