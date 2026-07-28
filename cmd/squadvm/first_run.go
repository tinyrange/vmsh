package main

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
	squadVMSSHConfigBegin = "# BEGIN SQUADVM MANAGED CONFIG"
	squadVMSSHConfigEnd   = "# END SQUADVM MANAGED CONFIG"
)

type squadVMSettings struct {
	FirstRunComplete bool    `json:"first_run_complete"`
	SSHEnabled       bool    `json:"ssh_enabled"`
	DownloadRate     float64 `json:"download_rate_bytes_per_second,omitempty"`
}

type startupOptions struct {
	SSHEnabled   bool
	RefreshImage bool
	DownloadRate float64
}

type startupPreflight struct {
	VirtualizationOK     bool
	VirtualizationDetail string
	DiskOK               bool
	FreeBytes            int64
	RequiredBytes        int64
	Image                client.ImagePullPlan
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

func loadSquadVMSettings() (squadVMSettings, string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return squadVMSettings{}, "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	dir := filepath.Join(configDir, "SquadVM")
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return squadVMSettings{}, dir, nil
	}
	if err != nil {
		return squadVMSettings{}, "", fmt.Errorf("read SquadVM settings: %w", err)
	}
	var settings squadVMSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return squadVMSettings{}, "", fmt.Errorf("decode SquadVM settings: %w", err)
	}
	return settings, dir, nil
}

func saveSquadVMSettings(dir string, settings squadVMSettings) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create SquadVM settings directory: %w", err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode SquadVM settings: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomically(filepath.Join(dir, "settings.json"), data, 0o600)
}

func runSquadVMPreflight(ctx context.Context, api *client.Client, source, cacheDir string) (startupPreflight, error) {
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
		localName := squadvmPulledImageName(source, runtimeArchitecture())
		result.Image, err = api.PlanImagePullContext(ctx, localName, client.PullImageRequest{
			Source:       source,
			Architecture: runtimeArchitecture(),
		})
		if err != nil {
			return startupPreflight{}, fmt.Errorf("resolve SquadVM image manifest: %w", err)
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
	result.RequiredBytes = estimatedSquadVMDiskRequirement(result.Image.BytesToDownload)
	result.DiskOK = result.FreeBytes >= result.RequiredBytes
	return result, nil
}

func estimatedSquadVMDiskRequirement(downloadBytes int64) int64 {
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

var runtimeGOARCH = runtimeArch()

func runtimeArch() string {
	return runtime.GOARCH
}

func reserveSquadVMSSHPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve SquadVM SSH port: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if port <= 0 {
		return 0, fmt.Errorf("reserve SquadVM SSH port: invalid port %d", port)
	}
	return port, nil
}

func ensureSquadVMSSHIdentity(configDir string) (string, []byte, error) {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create SquadVM settings directory: %w", err)
	}
	privatePath := filepath.Join(configDir, "squadvm_ed25519")
	privateData, err := os.ReadFile(privatePath)
	if err == nil {
		signer, parseErr := ssh.ParsePrivateKey(privateData)
		if parseErr != nil {
			return "", nil, fmt.Errorf("parse SquadVM SSH identity: %w", parseErr)
		}
		return privatePath, ssh.MarshalAuthorizedKey(signer.PublicKey()), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("read SquadVM SSH identity: %w", err)
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate SquadVM SSH identity: %w", err)
	}
	privateBlock, err := ssh.MarshalPrivateKey(privateKey, "SquadVM")
	if err != nil {
		return "", nil, fmt.Errorf("encode SquadVM SSH identity: %w", err)
	}
	if err := writeFileAtomically(privatePath, pem.EncodeToMemory(privateBlock), 0o600); err != nil {
		return "", nil, err
	}
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		return "", nil, fmt.Errorf("encode SquadVM SSH public key: %w", err)
	}
	return privatePath, ssh.MarshalAuthorizedKey(publicKey), nil
}

func configureSquadVMSSHHost(homeDir, identityPath string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid SquadVM SSH port %d", port)
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
		"%s\nHost squadvm\n    HostName 127.0.0.1\n    Port %d\n    User squad\n    IdentityFile %s\n    IdentitiesOnly yes\n    StrictHostKeyChecking accept-new\n    UserKnownHostsFile %s\n%s\n",
		squadVMSSHConfigBegin,
		port,
		strconv.Quote(identityPath),
		strconv.Quote(filepath.Join(sshDir, "squadvm_known_hosts")),
		squadVMSSHConfigEnd,
	)
	if cleaned != "" && !strings.HasSuffix(cleaned, "\n") {
		cleaned += "\n"
	}
	if cleaned != "" {
		cleaned += "\n"
	}
	return writeFileAtomically(configPath, []byte(cleaned+block), 0o600)
}

func removeSquadVMSSHHost(homeDir string) error {
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
	start := strings.Index(value, squadVMSSHConfigBegin)
	if start < 0 {
		return strings.TrimRight(value, "\n")
	}
	endOffset := strings.Index(value[start:], squadVMSSHConfigEnd)
	if endOffset < 0 {
		return strings.TrimRight(value[:start], "\n")
	}
	end := start + endOffset + len(squadVMSSHConfigEnd)
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

func configureSquadVMGuestSSH(ctx context.Context, api *client.Client, name string, publicKey []byte) error {
	const script = `set -eu
install -d -o squad -g squad -m 0700 /home/squad/.ssh
key=$(cat)
touch /home/squad/.ssh/authorized_keys
if ! grep -qxF "$key" /home/squad/.ssh/authorized_keys; then
    printf '%s\n' "$key" >> /home/squad/.ssh/authorized_keys
fi
chown squad:squad /home/squad/.ssh/authorized_keys
chmod 0600 /home/squad/.ssh/authorized_keys
`
	response, err := api.RunInContext(ctx, name, client.RunRequest{
		Command:        []string{"/bin/sh", "-c", script},
		User:           "root",
		Stdin:          bytes.TrimSpace(publicKey),
		TimeoutSeconds: 30,
	})
	if err != nil {
		return fmt.Errorf("configure SquadVM SSH server: %w", err)
	}
	if response.ExitCode != 0 {
		return fmt.Errorf("configure SquadVM SSH server: guest command exited %d: %s", response.ExitCode, strings.TrimSpace(response.Output))
	}
	return nil
}
