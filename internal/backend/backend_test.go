package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResolveCCVMPathHonorsExplicitPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-ccvm")
	got, err := ResolveCCVMPath(path, false)
	if err != nil {
		t.Fatalf("ResolveCCVMPath(explicit) error = %v", err)
	}
	if got.Path != path || len(got.Args) != 0 {
		t.Fatalf("ResolveCCVMPath(explicit) = %#v; want path %q with no args", got, path)
	}
}

func TestCCVMPathCandidatesUseHostExecutableNames(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, HostExecutableName("vmsh"))
	got := CCVMPathCandidates(exePath)
	want := []string{
		filepath.Join(dir, HostExecutableName("ccvm")),
		CompanionExecutablePath(exePath, "vm"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CCVMPathCandidates() = %#v, want %#v", got, want)
	}
	if runtime.GOOS == "windows" {
		for _, candidate := range got {
			if !strings.EqualFold(filepath.Ext(candidate), ".exe") {
				t.Fatalf("candidate %q extension = %q, want .exe", candidate, filepath.Ext(candidate))
			}
		}
	}
}

func TestInternalCCVMSidecarModeConstants(t *testing.T) {
	if InternalCCVMSidecarModeEnv != "CCX3_CCVM_SIDECAR_MODE" {
		t.Fatalf("sidecar mode env = %q, want CCX3_CCVM_SIDECAR_MODE", InternalCCVMSidecarModeEnv)
	}
	if InternalCCVMSidecarMode != "vmsh-internal" {
		t.Fatalf("sidecar mode = %q, want vmsh-internal", InternalCCVMSidecarMode)
	}
}

func TestWriteReadDaemonState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccvm.json")
	if err := WriteDaemonState(path, DaemonState{Addr: "127.0.0.1:1234"}); err != nil {
		t.Fatalf("WriteDaemonState() error = %v", err)
	}
	state, err := ReadDaemonState(path)
	if err != nil {
		t.Fatalf("ReadDaemonState() error = %v", err)
	}
	if state.Addr != "127.0.0.1:1234" {
		t.Fatalf("daemon addr = %q", state.Addr)
	}
	if _, err := ReadDaemonState(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("ReadDaemonState(missing) error = %v, want not exist", err)
	}
}

func TestDaemonWatchdogTimeoutDefaultAndEnvironment(t *testing.T) {
	t.Setenv("VMSH_DAEMON_WATCHDOG_TIMEOUT", "")
	t.Setenv("CCX3_DAEMON_WATCHDOG_TIMEOUT", "")
	if got := daemonWatchdogTimeout(); got != 3*time.Second {
		t.Fatalf("daemonWatchdogTimeout(default) = %s, want 3s", got)
	}

	t.Setenv("VMSH_DAEMON_WATCHDOG_TIMEOUT", "1.25")
	if got := daemonWatchdogTimeout(); got != 1250*time.Millisecond {
		t.Fatalf("daemonWatchdogTimeout(VMSH) = %s, want 1.25s", got)
	}

	t.Setenv("VMSH_DAEMON_WATCHDOG_TIMEOUT", "")
	t.Setenv("CCX3_DAEMON_WATCHDOG_TIMEOUT", "2")
	if got := daemonWatchdogTimeout(); got != 2*time.Second {
		t.Fatalf("daemonWatchdogTimeout(CCX3) = %s, want 2s", got)
	}

	t.Setenv("VMSH_DAEMON_WATCHDOG_TIMEOUT", "bad")
	t.Setenv("CCX3_DAEMON_WATCHDOG_TIMEOUT", "2")
	if got := daemonWatchdogTimeout(); got != 3*time.Second {
		t.Fatalf("daemonWatchdogTimeout(invalid) = %s, want fallback 3s", got)
	}
}
