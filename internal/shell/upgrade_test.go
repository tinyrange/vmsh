package shell

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/backend"
)

func TestDownloadVMSHReleaseVerifiesPublishedChecksum(t *testing.T) {
	const tag = "v9.8.7"
	binary := []byte("complete-vmsh-release-binary")
	assetName := vmshReleaseAssetName(tag, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(binary)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")

	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q,"size":%d},{"name":"checksums.txt","browser_download_url":%q,"size":%d}]}`,
				tag, assetName, server.URL+"/binary", len(binary), server.URL+"/checksums", len(checksums))
		case "/binary":
			_, _ = w.Write(binary)
		case "/checksums":
			_, _ = w.Write(checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	release, err := fetchVMSHRelease(context.Background(), server.Client(), server.URL+"/latest")
	if err != nil {
		t.Fatalf("fetch release: %v", err)
	}
	path, err := downloadVMSHRelease(context.Background(), server.Client(), release, t.TempDir(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("download release: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded release: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("downloaded release = %q, want %q", got, binary)
	}
}

func TestDownloadVMSHReleaseRejectsChecksumMismatch(t *testing.T) {
	const tag = "v9.8.7"
	binary := []byte("tampered")
	assetName := vmshReleaseAssetName(tag, runtime.GOOS, runtime.GOARCH)
	checksums := []byte(strings.Repeat("0", sha256.Size*2) + "  " + assetName + "\n")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			_, _ = w.Write(binary)
		case "/checksums":
			_, _ = w.Write(checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	release := vmshRelease{TagName: tag, Assets: []vmshReleaseAsset{
		{Name: assetName, BrowserDownloadURL: server.URL + "/binary", Size: int64(len(binary))},
		{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/checksums", Size: int64(len(checksums))},
	}}
	dir := t.TempDir()
	if _, err := downloadVMSHRelease(context.Background(), server.Client(), release, dir, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("checksum mismatch succeeded")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("staging directory after checksum failure: entries=%v err=%v", entries, err)
	}
}

func TestVMSHReleaseDownloadIntegration(t *testing.T) {
	if os.Getenv("VMSH_TEST_RELEASE_DOWNLOAD") == "" {
		t.Skip("set VMSH_TEST_RELEASE_DOWNLOAD=1 to download and validate the latest published vmsh release")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	release, err := fetchVMSHRelease(ctx, vmshUpgradeHTTPClient, vmshLatestReleaseURL)
	if err != nil {
		t.Fatalf("fetch latest release: %v", err)
	}
	path, err := downloadVMSHRelease(ctx, vmshUpgradeHTTPClient, release, t.TempDir(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("download latest release: %v", err)
	}
	if err := validateDownloadedVMSH(ctx, path, release.TagName, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("validate latest release: %v", err)
	}
	installDir := t.TempDir()
	executable := filepath.Join(installDir, backend.HostExecutableName("vmsh"))
	daemon := filepath.Join(installDir, "cache", "bin", backend.HostExecutableName("vmshd"))
	noticePath := upgradeNoticePath(filepath.Join(installDir, "cache"))
	notice := vmshUpgradeNotice{Version: release.TagName, Executable: executable, UpdatedAt: time.Now().UTC()}
	if err := installVMSHUpgrade(path, executable, daemon, noticePath, notice); err != nil {
		t.Fatalf("install latest release: %v", err)
	}
	if err := validateDownloadedVMSH(ctx, executable, release.TagName, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("validate installed shell: %v", err)
	}
	downloadedDigest := fileSHA256ForUpgradeTest(t, path)
	if daemonDigest := fileSHA256ForUpgradeTest(t, daemon); daemonDigest != downloadedDigest {
		t.Fatalf("installed daemon digest = %s, want %s", daemonDigest, downloadedDigest)
	}
	if got, err := readVMSHUpgradeNotice(noticePath); err != nil || got.Version != release.TagName {
		t.Fatalf("installed upgrade notice = %+v, err=%v", got, err)
	}
}

func TestInstallVMSHUpgradePublishesShellDaemonAndNotice(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, backend.HostExecutableName("vmsh"))
	daemon := filepath.Join(dir, "cache", "bin", backend.HostExecutableName("vmshd"))
	downloaded := filepath.Join(dir, "downloaded")
	writeUpgradeTestFile(t, executable, "old-shell")
	writeUpgradeTestFile(t, daemon, "old-daemon")
	writeUpgradeTestFile(t, downloaded, "new-release")
	noticePath := upgradeNoticePath(filepath.Join(dir, "cache"))
	notice := vmshUpgradeNotice{Version: "v9.8.7", Executable: executable, UpdatedAt: time.Now().UTC()}

	if err := installVMSHUpgrade(downloaded, executable, daemon, noticePath, notice); err != nil {
		t.Fatalf("install upgrade: %v", err)
	}
	for _, path := range []string{executable, daemon} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != "new-release" {
			t.Fatalf("%s = %q, want new release", path, got)
		}
	}
	gotNotice, err := readVMSHUpgradeNotice(noticePath)
	if err != nil {
		t.Fatalf("read upgrade notice: %v", err)
	}
	if gotNotice.Version != notice.Version || gotNotice.Executable != executable {
		t.Fatalf("upgrade notice = %+v, want %+v", gotNotice, notice)
	}
}

func TestInstallVMSHUpgradeRollsBackShellAndDaemonWhenNoticeFails(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, backend.HostExecutableName("vmsh"))
	daemon := filepath.Join(dir, "cache", "bin", backend.HostExecutableName("vmshd"))
	downloaded := filepath.Join(dir, "downloaded")
	noticePath := filepath.Join(dir, "cache", "notice-is-a-directory")
	writeUpgradeTestFile(t, executable, "old-shell")
	writeUpgradeTestFile(t, daemon, "old-daemon")
	writeUpgradeTestFile(t, downloaded, "new-release")
	if err := os.MkdirAll(noticePath, 0o755); err != nil {
		t.Fatalf("create invalid notice destination: %v", err)
	}

	err := installVMSHUpgrade(
		downloaded,
		executable,
		daemon,
		noticePath,
		vmshUpgradeNotice{Version: "v9.8.7", Executable: executable, UpdatedAt: time.Now().UTC()},
	)
	if err == nil {
		t.Fatal("upgrade with invalid notice destination succeeded")
	}
	for path, want := range map[string]string{executable: "old-shell", daemon: "old-daemon"} {
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read rolled-back %s: %v", path, readErr)
		}
		if string(got) != want {
			t.Fatalf("%s after rollback = %q, want %q", path, got, want)
		}
	}
}

func TestUpgradeBuiltinRestartsInstalledRelease(t *testing.T) {
	sh := newUnitShell(t, newRecordingShellAPI())
	executable := filepath.Join(t.TempDir(), backend.HostExecutableName("vmsh"))
	sh.vmshPath = executable
	sh.performUpgrade = func(io.Writer) (vmshUpgradeResult, error) {
		return vmshUpgradeResult{
			Version:    "v9.8.7",
			Executable: executable,
			Daemon:     filepath.Join(sh.rootCache, "bin", backend.HostExecutableName("vmshd")),
		}, nil
	}
	var stdout, stderr bytes.Buffer
	err := sh.eval("@upgrade", &stdout, &stderr)
	var req shellExecRequest
	if !errors.As(err, &req) {
		t.Fatalf("@upgrade error = %v, want shell restart", err)
	}
	if req.path != executable || len(req.argv) < 1 || req.argv[0] != executable {
		t.Fatalf("restart request = path %q argv %#v", req.path, req.argv)
	}
	if envHas(req.env, "VMSH_ACTIVE") {
		t.Fatal("restart request retained VMSH_ACTIVE")
	}
	if !strings.Contains(stdout.String(), "v9.8.7") {
		t.Fatalf("@upgrade output = %q", stdout.String())
	}
}

func TestUpgradeBuiltinDoesNotRestartLatestRelease(t *testing.T) {
	sh := newUnitShell(t, newRecordingShellAPI())
	sh.vmshPath = filepath.Join(t.TempDir(), backend.HostExecutableName("vmsh"))
	sh.performUpgrade = func(io.Writer) (vmshUpgradeResult, error) {
		return vmshUpgradeResult{Version: "v9.8.7", AlreadyLatest: true}, nil
	}
	var stdout, stderr bytes.Buffer
	if err := sh.eval("@upgrade", &stdout, &stderr); err != nil {
		t.Fatalf("@upgrade: %v", err)
	}
	if !strings.Contains(stdout.String(), "already the latest release") {
		t.Fatalf("@upgrade output = %q", stdout.String())
	}
}

func TestUpgradeNoticeIsShownOnceToOlderShell(t *testing.T) {
	sh := newUnitShell(t, newRecordingShellAPI())
	notice := vmshUpgradeNotice{
		Version:    "v999.0.0",
		Executable: filepath.Join(sh.rootCache, backend.HostExecutableName("vmsh")),
		UpdatedAt:  time.Now().UTC(),
	}
	if err := writeVMSHUpgradeNotice(upgradeNoticePath(sh.rootCache), notice); err != nil {
		t.Fatalf("write notice: %v", err)
	}
	var output bytes.Buffer
	sh.refreshVMSHUpgradeNotice()
	sh.printVMSHUpgradeNotice(&output)
	sh.printVMSHUpgradeNotice(&output)
	if strings.Count(output.String(), "@exec vmsh") != 1 {
		t.Fatalf("upgrade notice output = %q", output.String())
	}
}

func writeUpgradeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fileSHA256ForUpgradeTest(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
