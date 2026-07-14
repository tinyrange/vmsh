package shell

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishCodexReleaseRestoresPreviousOnFailure(t *testing.T) {
	root := t.TempDir()
	releaseDir := filepath.Join(root, "1.2.3-aarch64-apple-darwin")
	stage := filepath.Join(root, ".staging")
	writeCodexReleaseMarker(t, releaseDir, "old")
	writeCodexReleaseMarker(t, stage, "new")

	publishErr := errors.New("injected publish failure")
	rename := func(oldPath, newPath string) error {
		if oldPath == stage && newPath == releaseDir {
			return publishErr
		}
		return os.Rename(oldPath, newPath)
	}
	if err := publishCodexRelease(stage, releaseDir, rename, os.RemoveAll); !errors.Is(err, publishErr) {
		t.Fatalf("publishCodexRelease error = %v, want injected failure", err)
	}

	assertCodexReleaseMarker(t, releaseDir, "old")
	assertNoCodexRollbackDirs(t, root)
}

func TestPublishCodexReleaseReplacesPreviousAfterStageIsReady(t *testing.T) {
	root := t.TempDir()
	releaseDir := filepath.Join(root, "1.2.3-aarch64-apple-darwin")
	stage := filepath.Join(root, ".staging")
	writeCodexReleaseMarker(t, releaseDir, "old")
	writeCodexReleaseMarker(t, stage, "new")

	if err := publishCodexRelease(stage, releaseDir, os.Rename, os.RemoveAll); err != nil {
		t.Fatalf("publishCodexRelease: %v", err)
	}

	assertCodexReleaseMarker(t, releaseDir, "new")
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatalf("stage still exists after publication: %v", err)
	}
	assertNoCodexRollbackDirs(t, root)
}

func TestUpdateVMShCodexLinkSwitchesCurrentRelease(t *testing.T) {
	root := t.TempDir()
	target := "aarch64-apple-darwin"
	for _, release := range []string{"1.2.2-" + target, "1.2.3-" + target} {
		if err := os.MkdirAll(filepath.Join(root, "releases", release), 0o755); err != nil {
			t.Fatalf("create release: %v", err)
		}
	}

	if err := updateVMShCodexLink(root, target, "1.2.2-"+target); err != nil {
		t.Fatalf("activate initial release: %v", err)
	}
	if err := updateVMShCodexLink(root, target, "1.2.3-"+target); err != nil {
		t.Fatalf("activate replacement release: %v", err)
	}

	current := filepath.Join(root, "vmsh", target, "current")
	destination, err := os.Readlink(current)
	if err != nil {
		t.Fatalf("read current release link: %v", err)
	}
	want := filepath.Join("..", "..", "releases", "1.2.3-"+target)
	if destination != want {
		t.Fatalf("current release = %q, want %q", destination, want)
	}
}

func writeCodexReleaseMarker(t *testing.T, dir, marker string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create release fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write release marker: %v", err)
	}
}

func assertCodexReleaseMarker(t *testing.T, dir, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "marker"))
	if err != nil {
		t.Fatalf("read release marker: %v", err)
	}
	if string(got) != want {
		t.Fatalf("release marker = %q, want %q", got, want)
	}
}

func assertNoCodexRollbackDirs(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read release root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rollback.") {
			t.Fatalf("rollback directory remains: %s", entry.Name())
		}
	}
}
