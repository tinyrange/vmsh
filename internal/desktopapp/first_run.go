package desktopapp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"j5.nz/cc/client"
)

const (
	appInstallPortable = "portable"
	appInstallSystem   = "system"
)

func managedSSHConfigBegin() string {
	return "# BEGIN " + strings.ToUpper(appConfig.DefaultVMName) + " MANAGED CONFIG"
}

func managedSSHConfigEnd() string {
	return "# END " + strings.ToUpper(appConfig.DefaultVMName) + " MANAGED CONFIG"
}

type appSettings struct {
	FirstRunComplete bool    `json:"first_run_complete"`
	SSHEnabled       bool    `json:"ssh_enabled"`
	InstallMode      string  `json:"install_mode,omitempty"`
	DownloadRate     float64 `json:"download_rate_bytes_per_second,omitempty"`
	SharedFolder     string  `json:"shared_folder,omitempty"`
}

type startupOptions struct {
	SSHEnabled    bool
	SystemInstall bool
	RefreshImage  bool
	DownloadRate  float64
	SharedFolder  string
	DisplayWidth  int
	DisplayHeight int
}

type startupPreflight struct {
	VirtualizationOK     bool
	VirtualizationDetail string
	DiskOK               bool
	FreeBytes            int64
	RequiredBytes        int64
	Image                client.ImagePullPlan
	ReleaseUpdate        *releaseUpdate
	ReleaseChecked       bool
	ReleaseCheckDetail   string
}

func (p startupPreflight) canStart() bool {
	return p.VirtualizationOK && p.DiskOK
}

func (p startupPreflight) hasUpdate() bool {
	return p.Image.Installed && !p.Image.Available
}

func (p startupPreflight) downloadETA(rate float64) time.Duration {
	if p.Image.BytesToDownload <= 0 {
		return 0
	}
	if rate <= 0 {
		rate = 12 << 20
	}
	return time.Duration(float64(p.Image.BytesToDownload) / rate * float64(time.Second))
}

func loadAppSettings() (appSettings, string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return appSettings{}, "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	dir := filepath.Join(configDir, appConfig.ConfigDirName)
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return appSettings{}, dir, nil
	}
	if err != nil {
		return appSettings{}, "", fmt.Errorf("read %s settings: %w", productName(), err)
	}
	var settings appSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return appSettings{}, "", fmt.Errorf("decode %s settings: %w", productName(), err)
	}
	return settings, dir, nil
}

func saveAppSettings(dir string, settings appSettings) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s settings directory: %w", productName(), err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s settings: %w", productName(), err)
	}
	data = append(data, '\n')
	return writeFileAtomically(filepath.Join(dir, "settings.json"), data, 0o600)
}

func (s appSettings) systemInstall() bool {
	switch s.InstallMode {
	case appInstallSystem:
		return true
	case appInstallPortable:
		return false
	default:
		// Settings written before install modes existed used the system cache.
		// Preserve that location for existing installations while making a
		// genuinely new installation portable by default.
		return s.FirstRunComplete
	}
}

var (
	appExecutable   = os.Executable
	appUserCacheDir = os.UserCacheDir
	appGOOS         = runtime.GOOS
)

func resolveAppCacheDir(explicit string, systemInstall bool) (string, error) {
	if dir := strings.TrimSpace(explicit); dir != "" {
		return filepath.Abs(dir)
	}
	if systemInstall {
		userCache, err := appUserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache directory: %w", err)
		}
		return filepath.Join(userCache, "ccx3"), nil
	}
	// Releases before portable mode stored desktop images in the regular ccx3 cache.
	// Reuse that complete installation before looking beside the executable;
	// otherwise an upgrade can appear to lose the image and download it again.
	regularCache, err := resolveAppCacheDir("", true)
	if err == nil && appCacheContainsImage(regularCache) {
		return regularCache, nil
	}
	if appGOOS == "darwin" {
		userCache, err := appUserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache directory: %w", err)
		}
		// App bundles may be read-only, installed in /Applications, or launched
		// from an App Translocation mount. An app-specific user cache keeps the
		// non-system mode writable without sharing the system-install cache.
		return filepath.Join(userCache, appConfig.ConfigDirName, "ccx3"), nil
	}
	executable, err := appExecutable()
	if err != nil {
		return "", fmt.Errorf("resolve %s executable: %w", productName(), err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable path: %w", productName(), err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve absolute %s executable path: %w", productName(), err)
	}
	parent := filepath.Dir(executable)
	appMarker := ".app" + string(filepath.Separator) + "Contents" + string(filepath.Separator) + "MacOS"
	if index := strings.LastIndex(executable, appMarker); index >= 0 {
		bundle := executable[:index+len(".app")]
		parent = filepath.Dir(bundle)
	}
	return filepath.Join(parent, appConfig.DataDirName, "cache"), nil
}

func appCacheContainsImage(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "images", appConfig.CacheImageDir))
	return err == nil && info.IsDir()
}

func runAppPreflight(ctx context.Context, api *client.Client, source, cacheDir string) (startupPreflight, error) {
	type releaseCheckResult struct {
		update *releaseUpdate
		err    error
	}
	releaseContext, cancelRelease := context.WithTimeout(ctx, 2*time.Second)
	defer cancelRelease()
	releaseDone := make(chan releaseCheckResult, 1)
	go func() {
		update, err := checkLatestRelease(
			releaseContext,
			releaseHTTPClient,
			latestReleaseURL,
			currentAppVersion(),
			runtime.GOOS,
			runtime.GOARCH,
		)
		releaseDone <- releaseCheckResult{update: update, err: err}
	}()

	supported, err := api.VMSupportedContext(ctx)
	if err != nil {
		return startupPreflight{}, fmt.Errorf("check virtualization support: %w", err)
	}
	result := startupPreflight{
		VirtualizationOK:     supported.Supported,
		VirtualizationDetail: strings.TrimSpace(supported.Error),
	}
	if result.VirtualizationDetail == "" {
		if result.VirtualizationOK {
			result.VirtualizationDetail = "Hardware virtualization is available"
		} else {
			result.VirtualizationDetail = "Hardware virtualization is unavailable"
		}
	}

	if isRegistryImageReference(source) {
		localName := pulledImageName(source, runtimeArchitecture())
		result.Image, err = api.PlanImagePullContext(ctx, localName, client.PullImageRequest{
			Source:       source,
			Architecture: runtimeArchitecture(),
		})
		if err != nil {
			return startupPreflight{}, fmt.Errorf("resolve %s image manifest: %w", productName(), err)
		}
	} else {
		result.Image = client.ImagePullPlan{Name: source, Source: source, Installed: true, Available: true}
	}

	if strings.TrimSpace(cacheDir) == "" {
		userCache, cacheErr := os.UserCacheDir()
		if cacheErr != nil {
			return startupPreflight{}, fmt.Errorf("resolve cache directory: %w", cacheErr)
		}
		cacheDir = filepath.Join(userCache, "ccx3")
	}
	free, err := hostFreeBytes(cacheDir)
	if err != nil {
		return startupPreflight{}, fmt.Errorf("check free disk space: %w", err)
	}
	result.FreeBytes = free
	result.RequiredBytes = estimatedDiskRequirement(result.Image.BytesToDownload)
	result.DiskOK = result.FreeBytes >= result.RequiredBytes
	select {
	case release := <-releaseDone:
		// A release check must never prevent an offline user from starting.
		if release.err == nil {
			result.ReleaseChecked = true
			result.ReleaseUpdate = release.update
		} else {
			result.ReleaseCheckDetail = "Check unavailable"
		}
	case <-releaseContext.Done():
		result.ReleaseCheckDetail = "Check timed out"
	}
	return result, nil
}

func estimatedDiskRequirement(downloadBytes int64) int64 {
	const workspaceHeadroom = int64(2 << 30)
	if downloadBytes <= 0 {
		return workspaceHeadroom
	}
	// OCI layers remain compressed in the shared cache and are also expanded
	// into seekable layer tar files. Reserve both representations plus a small
	// writable-home allowance.
	return downloadBytes*2 + workspaceHeadroom
}

func runtimeArchitecture() string {
	return runtimeGOARCH
}

func appKernelModules(architecture string) []string {
	if architecture == "arm64" {
		return []string{"CONFIG_BINFMT_MISC"}
	}
	return nil
}

var runtimeGOARCH = runtimeArch()

func runtimeArch() string {
	return runtime.GOARCH
}

func reserveSSHPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve %s SSH port: %w", productName(), err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if port <= 0 {
		return 0, fmt.Errorf("reserve %s SSH port: invalid port %d", productName(), port)
	}
	return port, nil
}

func ensureSSHIdentity(configDir string) (string, []byte, error) {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create %s settings directory: %w", productName(), err)
	}
	privatePath := filepath.Join(configDir, appConfig.DefaultVMName+"_ed25519")
	privateData, err := os.ReadFile(privatePath)
	if err == nil {
		signer, parseErr := ssh.ParsePrivateKey(privateData)
		if parseErr != nil {
			return "", nil, fmt.Errorf("parse %s SSH identity: %w", productName(), parseErr)
		}
		return privatePath, ssh.MarshalAuthorizedKey(signer.PublicKey()), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("read %s SSH identity: %w", productName(), err)
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate %s SSH identity: %w", productName(), err)
	}
	privateBlock, err := ssh.MarshalPrivateKey(privateKey, productName())
	if err != nil {
		return "", nil, fmt.Errorf("encode %s SSH identity: %w", productName(), err)
	}
	if err := writeFileAtomically(privatePath, pem.EncodeToMemory(privateBlock), 0o600); err != nil {
		return "", nil, err
	}
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		return "", nil, fmt.Errorf("encode %s SSH public key: %w", productName(), err)
	}
	return privatePath, ssh.MarshalAuthorizedKey(publicKey), nil
}

func configureSSHHost(homeDir, identityPath string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid %s SSH port %d", productName(), port)
	}
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("create SSH configuration directory: %w", err)
	}
	configPath := filepath.Join(sshDir, "config")
	existing, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read SSH configuration: %w", err)
	}
	cleaned := removeManagedSSHBlock(string(existing))
	block := fmt.Sprintf(
		"%s\nHost %s\n    HostName 127.0.0.1\n    Port %d\n    User %s\n    IdentityFile %s\n    IdentitiesOnly yes\n    StrictHostKeyChecking accept-new\n    UserKnownHostsFile %s\n%s\n",
		managedSSHConfigBegin(),
		appConfig.SSHHost,
		port,
		appConfig.SSHUser,
		strconv.Quote(identityPath),
		strconv.Quote(filepath.Join(sshDir, appConfig.DefaultVMName+"_known_hosts")),
		managedSSHConfigEnd(),
	)
	if cleaned != "" && !strings.HasSuffix(cleaned, "\n") {
		cleaned += "\n"
	}
	if cleaned != "" {
		cleaned += "\n"
	}
	return writeFileAtomically(configPath, []byte(cleaned+block), 0o600)
}

func removeSSHHost(homeDir string) error {
	configPath := filepath.Join(homeDir, ".ssh", "config")
	existing, err := os.ReadFile(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read SSH configuration: %w", err)
	}
	cleaned := removeManagedSSHBlock(string(existing))
	if cleaned != "" && !strings.HasSuffix(cleaned, "\n") {
		cleaned += "\n"
	}
	return writeFileAtomically(configPath, []byte(cleaned), 0o600)
}

func removeManagedSSHBlock(value string) string {
	begin := managedSSHConfigBegin()
	endMarker := managedSSHConfigEnd()
	start := strings.Index(value, begin)
	if start < 0 {
		return strings.TrimRight(value, "\n")
	}
	endOffset := strings.Index(value[start:], endMarker)
	if endOffset < 0 {
		return strings.TrimRight(value[:start], "\n")
	}
	end := start + endOffset + len(endMarker)
	for end < len(value) && (value[end] == '\r' || value[end] == '\n') {
		end++
	}
	joined := value[:start] + value[end:]
	return strings.TrimSpace(joined)
}

func writeFileAtomically(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set permissions on %s: %w", tempPath, err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write %s: %w", tempPath, err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tempPath, err)
	}
	if err := replaceFile(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func configureGuestSSH(ctx context.Context, api *client.Client, name string, publicKey []byte) error {
	const script = `set -eu
user=$1
home=$2
group=$(id -gn "$user")
install -d -o "$user" -g "$group" -m 0700 "$home/.ssh"
key=$(cat)
touch "$home/.ssh/authorized_keys"
if ! grep -qxF "$key" "$home/.ssh/authorized_keys"; then
    printf '%s\n' "$key" >> "$home/.ssh/authorized_keys"
fi
chown "$user:$group" "$home/.ssh/authorized_keys"
chmod 0600 "$home/.ssh/authorized_keys"
`
	exitCode := -1
	var output strings.Builder
	err := api.RunStreamInContext(ctx, name, client.RunRequest{
		Command:        []string{"/bin/sh", "-c", script, "ssh-setup", appConfig.SSHUser, appConfig.SSHHome},
		User:           "root",
		Stdin:          bytes.TrimSpace(publicKey),
		TimeoutSeconds: 30,
	}, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "stderr", "output":
			if event.Output != "" {
				output.WriteString(event.Output)
			} else if len(event.Data) != 0 {
				output.Write(event.Data)
			}
		case "error":
			if event.Error != "" {
				return fmt.Errorf("%s", event.Error)
			}
			return fmt.Errorf("guest SSH setup failed")
		case "exit":
			exitCode = event.ExitCode
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("configure %s SSH server: %w", productName(), err)
	}
	if exitCode != 0 {
		detail := strings.TrimSpace(output.String())
		if detail == "" {
			detail = "no error detail was reported"
		}
		return fmt.Errorf("configure %s SSH server: guest command exited %d: %s", productName(), exitCode, detail)
	}
	return nil
}
