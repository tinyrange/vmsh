package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"j5.nz/cc/client"
)

const InternalVMSHDEnv = "VMSH_INTERNAL_VMSHD"
const InternalCCVMSidecarModeEnv = "CCX3_CCVM_SIDECAR_MODE"
const InternalCCVMSidecarMode = "vmsh-internal"
const DaemonStateVersion = 1
const DaemonAPIVersion = "2026-06-25"

type API interface {
	HealthCheck() error
	Capabilities() (client.CapabilitiesResponse, error)
	GetImage(string) (client.ImageState, error)
	PullImageStream(string, client.PullImageRequest, func(client.ProgressEvent) error) error
	DeleteImage(string) error
	SaveInstanceImage(string, client.SaveImageRequest) (client.ImageState, error)
	StartInstanceStreamWithID(string, client.StartInstanceRequest, func(client.BootEvent) error) (client.InstanceState, error)
	ShutdownInstanceWithID(string) error
	InstanceStatusOf(string) (client.InstanceState, error)
	InstanceStatuses() ([]client.InstanceState, error)
	AddPortForwardTo(string, client.PortForward) error
	AllowServiceProxyPortTo(string, int) error
	CreateWatchdogLease(client.WatchdogLeaseRequest) (client.WatchdogLeaseResponse, error)
	FeedWatchdogLease(string) error
	ReleaseWatchdogLease(string) error
	RunStreamIn(string, client.RunRequest, func(client.ExecEvent) error) error
	RunStreamInContext(context.Context, string, client.RunRequest, func(client.ExecEvent) error) error
	RunInteractiveStreamIn(string, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
	RunInteractiveStreamInContext(context.Context, string, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
	ExecStreamIn(string, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
}

type DaemonState struct {
	Addr       string `json:"addr"`
	Socket     string `json:"socket,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Version    int    `json:"version,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	TokenPath  string `json:"token_path,omitempty"`
	LaunchKey  string `json:"launch_key,omitempty"`
}

type CCVMLaunch struct {
	Path string
	Args []string
	Env  []string
}

func ResolveCCVMPath(path string, bundledAvailable bool) (CCVMLaunch, error) {
	if path != "" {
		return CCVMLaunch{Path: path}, nil
	}
	exePath, err := os.Executable()
	if err != nil {
		return CCVMLaunch{}, err
	}
	if bundledAvailable {
		return CCVMLaunch{Path: exePath, Env: []string{InternalVMSHDEnv + "=1"}}, nil
	}
	for _, candidate := range CCVMPathCandidates(exePath) {
		if _, err := os.Stat(candidate); err == nil {
			return CCVMLaunch{Path: candidate}, nil
		}
	}
	if found, err := exec.LookPath("ccvm"); err == nil {
		return CCVMLaunch{Path: found}, nil
	}
	return CCVMLaunch{}, fmt.Errorf("ccvm binary not found next to %s, bundled in vmsh, or on PATH; pass -ccvm", exePath)
}

func CCVMPathCandidates(exePath string) []string {
	return []string{
		filepath.Join(filepath.Dir(exePath), HostExecutableName("ccvm")),
		CompanionExecutablePath(exePath, "vm"),
	}
}

func HostExecutableName(name string) string {
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		return name + ".exe"
	}
	return name
}

func CompanionExecutablePath(exePath, suffix string) string {
	if runtime.GOOS != "windows" {
		return exePath + suffix
	}
	ext := filepath.Ext(exePath)
	if ext == "" {
		return exePath + suffix + ".exe"
	}
	return strings.TrimSuffix(exePath, ext) + suffix + ext
}

type ConnectOptions struct {
	OnReuse func(DaemonState)
	OnStart func(DaemonState)
}

func ConnectCCVM(launch CCVMLaunch, cacheDir, statePath string) (*client.Client, error) {
	return ConnectCCVMWithOptions(launch, cacheDir, statePath, ConnectOptions{})
}

func ConnectCCVMWithOptions(launch CCVMLaunch, cacheDir, statePath string, opts ConnectOptions) (*client.Client, error) {
	launchKey := DaemonLaunchKey(launch)
	if state, err := ReadDaemonState(statePath); err == nil {
		api := NewClient(state.Addr)
		if err := ApplyDaemonStateAuth(api, state); err != nil {
			_ = os.Remove(statePath)
		} else if state.LaunchKey == launchKey && api.HealthCheck() == nil && apiCompatible(api, state) {
			if opts.OnReuse != nil {
				opts.OnReuse(state)
			}
			return api, nil
		}
		_ = os.Remove(statePath)
	}

	args := append([]string{}, launch.Args...)
	args = append(args, "-cache-dir", cacheDir)
	proc := exec.Command(launch.Path, args...)
	if len(launch.Env) != 0 {
		proc.Env = append(os.Environ(), launch.Env...)
	}
	proc.Stderr = os.Stderr
	detachDaemonCommand(proc)
	stdout, err := proc.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("prepare ccvm stdout pipe for %s: %w", CCVMLaunchName(launch), err)
	}
	if err := proc.Start(); err != nil {
		return nil, fmt.Errorf("start ccvm daemon %s with cache %s: %w", CCVMLaunchName(launch), cacheDir, err)
	}

	var hello client.ServerHello
	if err := json.NewDecoder(stdout).Decode(&hello); err != nil {
		_ = proc.Wait()
		return nil, fmt.Errorf("ccvm daemon did not send a startup banner from %s: %w", CCVMLaunchName(launch), err)
	}
	if err := ValidateServerHello(hello, cacheDir); err != nil {
		_ = proc.Process.Kill()
		_ = proc.Wait()
		return nil, err
	}
	state := normalizeDaemonState(DaemonState{Addr: hello.Addr, Kind: hello.Kind, TokenPath: hello.TokenPath, LaunchKey: launchKey})
	if err := WriteDaemonState(statePath, state); err != nil {
		_ = proc.Process.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("write daemon state %s for %s: %w", statePath, hello.Addr, err)
	}
	api := NewClient(hello.Addr)
	if err := ApplyDaemonStateAuth(api, state); err != nil {
		_ = os.Remove(statePath)
		_ = proc.Process.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("read daemon auth token: %w", err)
	}
	if err := api.HealthCheck(); err != nil {
		_ = os.Remove(statePath)
		_ = proc.Process.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("ccvm daemon started at %s but health check failed: %w", hello.Addr, err)
	}
	if strings.TrimSpace(state.Kind) == "vmshd" && !apiCompatible(api, state) {
		_ = os.Remove(statePath)
		_ = proc.Process.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("vmshd daemon started at %s but required routes are unavailable", hello.Addr)
	}
	if opts.OnStart != nil {
		opts.OnStart(state)
	}
	return api, nil
}

func apiCompatible(api *client.Client, state DaemonState) bool {
	if api == nil {
		return false
	}
	_, err := api.Capabilities()
	if err != nil {
		return false
	}
	for _, route := range []string{"/watchdog/lease", "/vm/start"} {
		if !api.RouteExists(route) {
			return false
		}
	}
	if strings.TrimSpace(state.Kind) == "vmshd" && !api.RouteExists("/vmsh/status") {
		return false
	}
	return true
}

func CCVMLaunchName(launch CCVMLaunch) string {
	if len(launch.Args) == 0 {
		return launch.Path
	}
	return launch.Path + " " + strings.Join(launch.Args, " ")
}

func DaemonLaunchKey(launch CCVMLaunch) string {
	path := launch.Path
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	parts := []string{"path=" + path}
	if info, err := os.Stat(launch.Path); err == nil {
		parts = append(parts,
			"size="+strconv.FormatInt(info.Size(), 10),
			"mtime="+strconv.FormatInt(info.ModTime().UnixNano(), 10),
		)
	}
	parts = append(parts, "args="+strings.Join(launch.Args, "\x1f"))
	parts = append(parts, "env="+strings.Join(launch.Env, "\x1f"))
	return strings.Join(parts, "\x00")
}

func ValidateServerHello(hello client.ServerHello, cacheDir string) error {
	if hello.Error != "" || hello.Kind == "error" {
		detail := firstNonEmpty(hello.Detail, hello.Error, "unknown startup error")
		return fmt.Errorf("ccvm daemon failed to start using cache %s: %s", cacheDir, detail)
	}
	if strings.TrimSpace(hello.Addr) == "" {
		return fmt.Errorf("ccvm daemon sent a startup banner without an address: %+v", hello)
	}
	if strings.TrimSpace(hello.Kind) == "vmshd" && strings.TrimSpace(hello.TokenPath) == "" {
		return fmt.Errorf("vmshd daemon sent a startup banner without a token path")
	}
	return nil
}

func NewClient(addr string) *client.Client {
	return client.NewClient("http://"+addr, func() (net.Conn, error) {
		return net.Dial("tcp", addr)
	})
}

func ApplyDaemonStateAuth(api *client.Client, state DaemonState) error {
	if api == nil {
		return nil
	}
	if strings.TrimSpace(state.TokenPath) == "" {
		if strings.TrimSpace(state.Kind) == "vmshd" {
			return fmt.Errorf("vmshd daemon state has no token path")
		}
		return nil
	}
	token, err := ReadDaemonToken(state.TokenPath)
	if err != nil {
		return err
	}
	api.SetBearerToken(token)
	return nil
}

func ReadDaemonToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file %s has mode %o, want private permissions", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

func StartDaemonLease(api watchdogAPI) (func(), error) {
	timeout := daemonWatchdogTimeout()
	lease, err := api.CreateWatchdogLease(client.WatchdogLeaseRequest{TimeoutSeconds: timeout.Seconds()})
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(timeout / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_ = api.FeedWatchdogLease(lease.LeaseID)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
		_ = api.ReleaseWatchdogLease(lease.LeaseID)
	}, nil
}

type watchdogAPI interface {
	CreateWatchdogLease(client.WatchdogLeaseRequest) (client.WatchdogLeaseResponse, error)
	FeedWatchdogLease(string) error
	ReleaseWatchdogLease(string) error
}

func daemonWatchdogTimeout() time.Duration {
	const fallback = 3 * time.Second
	raw := strings.TrimSpace(os.Getenv("VMSH_DAEMON_WATCHDOG_TIMEOUT"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("CCX3_DAEMON_WATCHDOG_TIMEOUT"))
	}
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds * float64(time.Second))
}

func ReadDaemonState(path string) (DaemonState, error) {
	var state DaemonState
	if info, err := os.Stat(path); err != nil {
		return state, err
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return state, fmt.Errorf("daemon state %s has mode %o, want private permissions", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if strings.TrimSpace(state.Addr) == "" {
		return state, fmt.Errorf("daemon state %s has no addr", path)
	}
	if state.Version != 0 && state.Version != DaemonStateVersion {
		return state, fmt.Errorf("daemon state %s has unsupported version %d", path, state.Version)
	}
	if strings.TrimSpace(state.APIVersion) != "" && strings.TrimSpace(state.APIVersion) != DaemonAPIVersion {
		return state, fmt.Errorf("daemon state %s has unsupported api version %q", path, state.APIVersion)
	}
	return state, nil
}

func WriteDaemonState(path string, state DaemonState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	state = normalizeDaemonState(state)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func normalizeDaemonState(state DaemonState) DaemonState {
	if state.Version == 0 {
		state.Version = DaemonStateVersion
	}
	if strings.TrimSpace(state.APIVersion) == "" {
		state.APIVersion = DaemonAPIVersion
	}
	return state
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
