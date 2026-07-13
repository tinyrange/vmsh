package shell

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexInstallLockCannotBeStolen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.lock")
	first, err := acquireCodexFileLock(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	owner := readCodexLockOwner(t, path)
	if owner.PID != os.Getpid() || owner.Generation == "" {
		t.Fatalf("lock owner = %+v", owner)
	}

	if _, err := acquireCodexFileLock(path, 20*time.Millisecond); !errors.Is(err, errCodexLockTimeout) {
		t.Fatalf("contended acquire error = %v, want %v", err, errCodexLockTimeout)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("second owner release: %v", err)
	}

	second, err := acquireCodexFileLock(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if next := readCodexLockOwner(t, path); next.Generation == owner.Generation {
		t.Fatalf("new owner reused generation %q", next.Generation)
	}
}

func TestCodexInstallLockRecoversAfterOwnerExit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "install.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCodexInstallLockHelper$")
	cmd.Env = append(os.Environ(), "VMSH_CODEX_LOCK_HELPER_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lock helper: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(dir, "acquired")); err != nil {
		t.Fatalf("helper did not acquire lock: %v", err)
	}

	lock, err := acquireCodexFileLock(path, time.Second)
	if err != nil {
		t.Fatalf("acquire after owner exit: %v", err)
	}
	defer lock.Release()
}

func TestCodexInstallLockHelper(t *testing.T) {
	path := os.Getenv("VMSH_CODEX_LOCK_HELPER_PATH")
	if path == "" {
		return
	}
	lock, err := acquireCodexFileLock(path, time.Second)
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "acquired"), []byte(lock.owner.Generation), 0o600); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func readCodexLockOwner(t *testing.T, path string) codexLockOwner {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var owner codexLockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatalf("decode lock owner: %v", err)
	}
	return owner
}
