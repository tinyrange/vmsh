package shell

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tinyrange/vmsh/internal/backend"
	"github.com/tinyrange/vmsh/internal/version"
)

const (
	vmshLatestReleaseURL     = "https://api.github.com/repos/tinyrange/vmsh/releases/latest"
	vmshUpgradeMetadataBytes = 1024 * 1024
	vmshUpgradeChecksumBytes = 1024 * 1024
)

var vmshUpgradeHTTPClient = &http.Client{Timeout: 30 * time.Minute}

type vmshUpgradeResult struct {
	Version       string
	Executable    string
	Daemon        string
	AlreadyLatest bool
}

type vmshRelease struct {
	TagName string             `json:"tag_name"`
	Assets  []vmshReleaseAsset `json:"assets"`
}

type vmshReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type vmshUpgradeNotice struct {
	Version    string    `json:"version"`
	Executable string    `json:"executable"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (s *shellState) upgradeVMSH(stdout, stderr io.Writer) error {
	if strings.TrimSpace(s.vmshPath) == "" {
		return fmt.Errorf("cannot determine the running vmsh executable")
	}
	run := s.performUpgrade
	if run == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		run = func(progress io.Writer) (vmshUpgradeResult, error) {
			return performVMSHUpgrade(ctx, vmshUpgradeHTTPClient, vmshLatestReleaseURL, s.vmshPath, s.rootCache, progress)
		}
	}
	result, err := run(stderr)
	if err != nil {
		return err
	}
	if result.AlreadyLatest {
		_, err := fmt.Fprintf(stdout, "vmsh %s is already the latest release.\n", result.Version)
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Installed vmsh %s at %s\nInstalled vmshd at %s\nRestarting with the new vmsh release.\n", result.Version, result.Executable, result.Daemon); err != nil {
		return err
	}
	return s.restartVMSHRequest(result.Executable)
}

func (s *shellState) restartVMSHRequest(executable string) shellExecRequest {
	env := hostCommandEnv(s.env, nil)
	env = mergedEnv(env, []string{"VMSH_DISABLE=1"})
	env = withoutEnv(env, "VMSH_ACTIVE")
	args := []string{executable}
	if strings.TrimSpace(s.rootCache) != "" {
		args = append(args, "-cache-dir", s.rootCache)
	}
	return shellExecRequest{path: executable, argv: args, env: env}
}

func performVMSHUpgrade(ctx context.Context, httpClient *http.Client, releaseURL, executable, cacheDir string, progress io.Writer) (vmshUpgradeResult, error) {
	if httpClient == nil {
		httpClient = vmshUpgradeHTTPClient
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		return vmshUpgradeResult{}, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	if progress != nil {
		_, _ = fmt.Fprintln(progress, "Checking the latest vmsh release...")
	}
	release, err := fetchVMSHRelease(ctx, httpClient, releaseURL)
	if err != nil {
		return vmshUpgradeResult{}, err
	}
	current := strings.TrimSpace(version.Current().Version)
	if current == release.TagName {
		return vmshUpgradeResult{Version: release.TagName, Executable: executable, AlreadyLatest: true}, nil
	}
	if strings.TrimSpace(cacheDir) == "" {
		return vmshUpgradeResult{}, fmt.Errorf("vmsh cache directory is required")
	}
	downloadDir, err := os.MkdirTemp(cacheDir, ".upgrade-download-")
	if err != nil {
		return vmshUpgradeResult{}, fmt.Errorf("create upgrade staging directory: %w", err)
	}
	defer os.RemoveAll(downloadDir)
	if progress != nil {
		_, _ = fmt.Fprintf(progress, "Downloading vmsh %s for %s/%s...\n", release.TagName, runtime.GOOS, runtime.GOARCH)
	}
	downloaded, err := downloadVMSHRelease(ctx, httpClient, release, downloadDir, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return vmshUpgradeResult{}, err
	}
	if err := validateDownloadedVMSH(ctx, downloaded, release.TagName, runtime.GOOS, runtime.GOARCH); err != nil {
		return vmshUpgradeResult{}, err
	}
	daemonPath := filepath.Join(cacheDir, "bin", backend.HostExecutableName("vmshd"))
	notice := vmshUpgradeNotice{Version: release.TagName, Executable: executable, UpdatedAt: time.Now().UTC()}
	if err := installVMSHUpgrade(downloaded, executable, daemonPath, upgradeNoticePath(cacheDir), notice); err != nil {
		return vmshUpgradeResult{}, err
	}
	return vmshUpgradeResult{Version: release.TagName, Executable: executable, Daemon: daemonPath}, nil
}

func fetchVMSHRelease(ctx context.Context, httpClient *http.Client, releaseURL string) (vmshRelease, error) {
	var release vmshRelease
	if err := requireHTTPSURL(releaseURL); err != nil {
		return release, fmt.Errorf("fetch latest vmsh release: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return release, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vmsh")
	resp, err := httpClient.Do(req)
	if err != nil {
		return release, fmt.Errorf("fetch latest vmsh release: %w", err)
	}
	defer resp.Body.Close()
	if err := requireHTTPSURL(resp.Request.URL.String()); err != nil {
		return release, fmt.Errorf("fetch latest vmsh release: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return release, fmt.Errorf("fetch latest vmsh release: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, vmshUpgradeMetadataBytes+1))
	if err := decoder.Decode(&release); err != nil {
		return release, fmt.Errorf("decode latest vmsh release: %w", err)
	}
	if strings.TrimSpace(release.TagName) == "" {
		return release, fmt.Errorf("latest vmsh release is missing a tag")
	}
	if !validVMSHReleaseTag(release.TagName) {
		return release, fmt.Errorf("latest vmsh release has invalid tag %q", release.TagName)
	}
	return release, nil
}

func validVMSHReleaseTag(tag string) bool {
	if len(tag) < 2 || tag[0] != 'v' {
		return false
	}
	for _, char := range tag[1:] {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func downloadVMSHRelease(ctx context.Context, httpClient *http.Client, release vmshRelease, dir, goos, goarch string) (string, error) {
	assetName := vmshReleaseAssetName(release.TagName, goos, goarch)
	binary, ok := findVMSHReleaseAsset(release.Assets, assetName)
	if !ok {
		return "", fmt.Errorf("vmsh release %s does not include %s", release.TagName, assetName)
	}
	checksums, ok := findVMSHReleaseAsset(release.Assets, "checksums.txt")
	if !ok {
		return "", fmt.Errorf("vmsh release %s does not include checksums.txt", release.TagName)
	}
	checksumData, err := downloadVMSHAssetBytes(ctx, httpClient, checksums, vmshUpgradeChecksumBytes)
	if err != nil {
		return "", err
	}
	expected, err := checksumForVMSHAsset(checksumData, assetName)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, assetName)
	actual, err := downloadVMSHAssetFile(ctx, httpClient, binary, path)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(actual, expected) {
		_ = os.Remove(path)
		return "", fmt.Errorf("SHA-256 mismatch for %s", assetName)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func vmshReleaseAssetName(tag, goos, goarch string) string {
	suffix := ""
	if goos == "windows" {
		suffix = ".exe"
	}
	return "vmsh_" + tag + "_" + goos + "_" + goarch + suffix
}

func findVMSHReleaseAsset(assets []vmshReleaseAsset, name string) (vmshReleaseAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name && strings.TrimSpace(asset.BrowserDownloadURL) != "" {
			return asset, true
		}
	}
	return vmshReleaseAsset{}, false
}

func downloadVMSHAssetBytes(ctx context.Context, httpClient *http.Client, asset vmshReleaseAsset, limit int64) ([]byte, error) {
	resp, err := requestVMSHAsset(ctx, httpClient, asset)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download %s exceeds %d bytes", asset.Name, limit)
	}
	if asset.Size > 0 && int64(len(data)) != asset.Size {
		return nil, fmt.Errorf("download %s size %d does not match release metadata %d", asset.Name, len(data), asset.Size)
	}
	return data, nil
}

func downloadVMSHAssetFile(ctx context.Context, httpClient *http.Client, asset vmshReleaseAsset, path string) (string, error) {
	resp, err := requestVMSHAsset(ctx, httpClient, asset)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(out, hash), resp.Body)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("download %s: %w", asset.Name, copyErr)
	}
	if syncErr != nil {
		_ = os.Remove(path)
		return "", syncErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", closeErr
	}
	if asset.Size > 0 && written != asset.Size {
		_ = os.Remove(path)
		return "", fmt.Errorf("download %s size %d does not match release metadata %d", asset.Name, written, asset.Size)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func requestVMSHAsset(ctx context.Context, httpClient *http.Client, asset vmshReleaseAsset) (*http.Response, error) {
	if err := requireHTTPSURL(asset.BrowserDownloadURL); err != nil {
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "vmsh")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("download %s: %s: %s", asset.Name, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := requireHTTPSURL(resp.Request.URL.String()); err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	if asset.Size > 0 && resp.ContentLength > 0 && resp.ContentLength != asset.Size {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s content length %d does not match release metadata %d", asset.Name, resp.ContentLength, asset.Size)
	}
	return resp, nil
}

func requireHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return fmt.Errorf("release asset URL must use HTTPS")
	}
	return nil
}

func checksumForVMSHAsset(data []byte, assetName string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || strings.TrimPrefix(fields[1], "*") != assetName {
			continue
		}
		digest := strings.ToLower(fields[0])
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("checksums.txt has an invalid SHA-256 for %s", assetName)
		}
		return digest, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksums.txt does not include %s", assetName)
}

func validateDownloadedVMSH(ctx context.Context, path, tag, goos, goarch string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("validate downloaded vmsh: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fields := parseVMSHVersionOutput(string(out))
	if fields["version"] != tag {
		return fmt.Errorf("downloaded vmsh reports version %q, want %q", fields["version"], tag)
	}
	if fields["platform"] != goos+"/"+goarch {
		return fmt.Errorf("downloaded vmsh reports platform %q, want %q", fields["platform"], goos+"/"+goarch)
	}
	if fields["ccvm"] != "bundled" {
		return fmt.Errorf("downloaded vmsh does not contain the bundled daemon")
	}
	return nil
}

func parseVMSHVersionOutput(out string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 3 && parts[0] == "vmsh" && parts[1] == "version" {
			fields["version"] = parts[2]
			continue
		}
		if len(parts) >= 2 {
			fields[parts[0]] = strings.Join(parts[1:], " ")
		}
	}
	return fields
}

func installVMSHUpgrade(downloaded, executable, daemonPath, noticePath string, notice vmshUpgradeNotice) error {
	if err := os.MkdirAll(filepath.Dir(noticePath), 0o700); err != nil {
		return fmt.Errorf("create upgrade state directory: %w", err)
	}
	backupDir, err := os.MkdirTemp(filepath.Dir(noticePath), ".upgrade-rollback-")
	if err != nil {
		return fmt.Errorf("create upgrade rollback directory: %w", err)
	}
	defer os.RemoveAll(backupDir)
	executableBackup, executableExisted, err := backupExecutable(executable, backupDir, "vmsh")
	if err != nil {
		return fmt.Errorf("back up vmsh: %w", err)
	}
	daemonBackup, daemonExisted, err := backupExecutable(daemonPath, backupDir, "vmshd")
	if err != nil {
		return fmt.Errorf("back up vmshd: %w", err)
	}
	executableChanged := false
	daemonChanged := false
	rollback := func(cause error) error {
		var rollbackErr error
		if daemonChanged {
			rollbackErr = errors.Join(rollbackErr, restoreExecutable(daemonBackup, daemonPath, daemonExisted))
		}
		if executableChanged {
			rollbackErr = errors.Join(rollbackErr, restoreExecutable(executableBackup, executable, executableExisted))
		}
		if rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback incomplete: %w", rollbackErr))
		}
		return cause
	}
	if err := backend.InstallExecutable(downloaded, executable); err != nil {
		return fmt.Errorf("install vmsh at %s: %w", executable, err)
	}
	executableChanged = true
	if err := backend.InstallExecutable(downloaded, daemonPath); err != nil {
		return rollback(fmt.Errorf("install vmshd at %s: %w", daemonPath, err))
	}
	daemonChanged = true
	if err := writeVMSHUpgradeNotice(noticePath, notice); err != nil {
		return rollback(fmt.Errorf("publish upgrade notice: %w", err))
	}
	return nil
}

func backupExecutable(path, dir, name string) (string, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file", path)
	}
	backup := filepath.Join(dir, name)
	if err := backend.InstallExecutable(path, backup); err != nil {
		return "", false, err
	}
	return backup, true, nil
}

func restoreExecutable(backup, path string, existed bool) error {
	if existed {
		return backend.InstallExecutable(backup, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func upgradeNoticePath(cacheDir string) string {
	return filepath.Join(cacheDir, "vmsh-upgrade.json")
}

func writeVMSHUpgradeNotice(path string, notice vmshUpgradeNotice) error {
	if strings.TrimSpace(notice.Version) == "" || strings.TrimSpace(notice.Executable) == "" {
		return fmt.Errorf("upgrade notice requires a version and executable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vmsh-upgrade-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := json.NewEncoder(tmp).Encode(notice); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	if err := replaceHostRootFile(root, filepath.Base(tmpPath), filepath.Base(path)); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func readVMSHUpgradeNotice(path string) (vmshUpgradeNotice, error) {
	var notice vmshUpgradeNotice
	f, err := os.Open(path)
	if err != nil {
		return notice, err
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, 64*1024)).Decode(&notice); err != nil {
		return notice, err
	}
	return notice, nil
}

func (s *shellState) watchVMSHUpgradeNotices() func() {
	s.refreshVMSHUpgradeNotice()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshVMSHUpgradeNotice()
			}
		}
	}()
	return cancel
}

func (s *shellState) refreshVMSHUpgradeNotice() {
	if strings.TrimSpace(s.rootCache) == "" {
		return
	}
	notice, err := readVMSHUpgradeNotice(upgradeNoticePath(s.rootCache))
	if err != nil {
		return
	}
	s.upgradeNoticeMu.Lock()
	s.upgradeNotice = &notice
	s.upgradeNoticeMu.Unlock()
}

func (s *shellState) printVMSHUpgradeNotice(w io.Writer) {
	s.upgradeNoticeMu.Lock()
	if s.upgradeNotice == nil {
		s.upgradeNoticeMu.Unlock()
		return
	}
	notice := *s.upgradeNotice
	id := notice.Version + "\x00" + notice.Executable + "\x00" + notice.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if id == s.upgradeNoticeSeen {
		s.upgradeNoticeMu.Unlock()
		return
	}
	s.upgradeNoticeSeen = id
	s.upgradeNoticeMu.Unlock()
	current := version.Current().Version
	if current == notice.Version {
		return
	}
	_, _ = fmt.Fprintf(w, "vmsh was upgraded to %s. Run @exec vmsh to use it in this shell.\n", notice.Version)
}
