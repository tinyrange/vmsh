package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/vmsh/internal/vmshdprotocol"
	"j5.nz/cc/client"
)

const InternalVMSHDEnv = "VMSH_INTERNAL_VMSHD"
const InternalCCVMSidecarModeEnv = "CCX3_CCVM_SIDECAR_MODE"
const InternalCCVMSidecarMode = "vmsh-internal"
const DaemonStateVersion = 1
const DaemonAPIVersion = "2026-06-25"

// The daemon emits its banner immediately after local cache initialization and
// listener creation, before kernel, image, or VM work. Thirty seconds leaves
// substantial room for slow local filesystems without permitting a broken
// child to hang shell startup indefinitely.
const daemonStartupHandshakeTimeout = 30 * time.Second

var ErrDaemonStartupTimeout = errors.New("daemon startup banner timed out")

var daemonStateFileMu sync.RWMutex

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
	Addr            string `json:"addr"`
	Socket          string `json:"socket,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Version         int    `json:"version,omitempty"`
	APIVersion      string `json:"api_version,omitempty"`
	TokenPath       string `json:"token_path,omitempty"`
	Generation      string `json:"generation,omitempty"`
	LaunchKey       string `json:"launch_key,omitempty"`
	PrivateCacheDir string `json:"private_cache_dir,omitempty"`
}

type CCVMLaunch struct {
	Path string
	Args []string
	Env  []string
}

func ResolveCCVMPath(path string, bundledAvailable bool) (CCVMLaunch, error) {
	return ResolveCCVMPathForCache(path, bundledAvailable, "")
}

func ResolveCCVMPathForCache(path string, bundledAvailable bool, cacheDir string) (CCVMLaunch, error) {
	if path != "" {
		return CCVMLaunch{Path: path}, nil
	}
	exePath, err := os.Executable()
	if err != nil {
		return CCVMLaunch{}, err
	}
	if bundledAvailable {
		if stablePath, err := EnsureStableVMSHDCopy(exePath, cacheDir); err == nil {
			return CCVMLaunch{Path: stablePath}, nil
		}
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

func EnsureStableVMSHDCopy(exePath, cacheDir string) (string, error) {
	exePath = strings.TrimSpace(exePath)
	if exePath == "" {
		return "", fmt.Errorf("executable path is required")
	}
	if strings.TrimSpace(cacheDir) == "" {
		cacheDir = filepath.Dir(exePath)
	}
	stablePath := filepath.Join(cacheDir, "bin", HostExecutableName("vmshd"))
	return stablePath, InstallExecutable(exePath, stablePath)
}

// InstallExecutable copies src to dst and publishes the complete executable
// atomically. A failed copy or replacement leaves the previous dst intact.
func InstallExecutable(src, dst string) error {
	src = strings.TrimSpace(src)
	dst = strings.TrimSpace(dst)
	if src == "" || dst == "" {
		return fmt.Errorf("source and destination paths are required")
	}
	if sameFileContents(src, dst) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+"-*")
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
	source, err := os.Open(src)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, source); err != nil {
		_ = source.Close()
		_ = tmp.Close()
		return err
	}
	if err := source.Close(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
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
	if err := replaceFile(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func replaceFile(src, dst string) error {
	return platformReplaceFile(src, dst)
}

func sameFileContents(a, b string) bool {
	aInfo, err := os.Stat(a)
	if err != nil {
		return false
	}
	bInfo, err := os.Stat(b)
	if err != nil || aInfo.Size() != bInfo.Size() {
		return false
	}
	aDigest, err := executableDigest(a)
	if err != nil {
		return false
	}
	bDigest, err := executableDigest(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aDigest, bDigest)
}

func executableDigest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
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
	OnReuse        func(DaemonState)
	OnStart        func(DaemonState)
	OnIncompatible func(DaemonState, error)
}

func ConnectCCVM(launch CCVMLaunch, cacheDir, statePath string) (*client.Client, error) {
	return ConnectCCVMWithOptions(launch, cacheDir, statePath, ConnectOptions{})
}

func ConnectCCVMWithOptions(launch CCVMLaunch, cacheDir, statePath string, opts ConnectOptions) (*client.Client, error) {
	lock, err := acquireDaemonFileLock(statePath+".launch.lock", daemonStartupHandshakeTimeout+5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("lock daemon discovery %s: %w", statePath, err)
	}
	defer lock.Release()
	return connectCCVMWithOptionsLocked(launch, cacheDir, statePath, opts)
}

func connectCCVMWithOptionsLocked(launch CCVMLaunch, cacheDir, statePath string, opts ConnectOptions) (*client.Client, error) {
	if err := reclaimStalePrivateDaemonCaches(cacheDir); err != nil {
		return nil, fmt.Errorf("reclaim private daemon caches: %w", err)
	}
	launchKey := DaemonLaunchKey(launch)
	var preservedState *DaemonState
	var incompatibility error
	discoverySettled := false
	for attempt := 0; attempt < 3; attempt++ {
		state, err := ReadDaemonState(statePath)
		if err != nil {
			discoverySettled = true
			break
		}
		preserveVMSHDState := func(reason error) {
			if strings.TrimSpace(state.Kind) != "vmshd" || strings.TrimSpace(state.TokenPath) == "" || preservedState != nil {
				return
			}
			preserved := state
			preservedState = &preserved
			incompatibility = firstNonNil(reason, fmt.Errorf("existing vmshd daemon is not compatible with this frontend"))
			if opts.OnIncompatible != nil {
				opts.OnIncompatible(state, incompatibility)
			}
		}
		api := NewClient(state.Addr)
		if err := ApplyDaemonStateAuth(api, state); err != nil {
			if daemonEndpointReachable(state.Addr) {
				preserveVMSHDState(err)
			}
			if preservedState == nil {
				removed, removeErr := RemoveDaemonStateIfUnchanged(statePath, state)
				if removeErr != nil {
					return nil, fmt.Errorf("reclaim stale daemon state: %w", removeErr)
				}
				if !removed {
					continue
				}
			}
		} else if err := api.HealthCheck(); err != nil {
			if daemonEndpointReachable(state.Addr) {
				preserveVMSHDState(err)
			}
			if preservedState == nil {
				removed, removeErr := RemoveDaemonStateIfUnchanged(statePath, state)
				if removeErr != nil {
					return nil, fmt.Errorf("reclaim stale daemon state: %w", removeErr)
				}
				if !removed {
					continue
				}
			}
		} else {
			reusable, compatibilityErr := daemonReusable(api, state, launchKey)
			if reusable {
				if opts.OnReuse != nil {
					opts.OnReuse(state)
				}
				return api, nil
			}
			preserveVMSHDState(compatibilityErr)
			if preservedState == nil {
				removed, removeErr := RemoveDaemonStateIfUnchanged(statePath, state)
				if removeErr != nil {
					return nil, fmt.Errorf("reclaim incompatible daemon state: %w", removeErr)
				}
				if !removed {
					continue
				}
			}
		}
		discoverySettled = true
		break
	}
	if !discoverySettled {
		return nil, fmt.Errorf("daemon state changed repeatedly during stale-state reclamation")
	}

	startCacheDir := cacheDir
	if preservedState != nil {
		var err error
		startCacheDir, err = privateDaemonCacheDir(cacheDir)
		if err != nil {
			return nil, err
		}
	}
	started, err := startDaemonProcess(launch, startCacheDir)
	if err != nil {
		if preservedState != nil {
			_ = os.RemoveAll(startCacheDir)
		}
		return nil, fmt.Errorf("start ccvm daemon %s with cache %s: %w", CCVMLaunchName(launch), startCacheDir, err)
	}
	removeFailedPrivateCache := func() {
		if preservedState != nil {
			_ = os.RemoveAll(startCacheDir)
		}
	}

	hello, err := readDaemonStartupBanner(started, launch, daemonStartupHandshakeTimeout)
	if err != nil {
		removeFailedPrivateCache()
		return nil, err
	}
	if started.release != nil {
		started.release()
	}
	if err := ValidateServerHello(hello, cacheDir); err != nil {
		started.stop()
		removeFailedPrivateCache()
		return nil, err
	}
	state := normalizeDaemonState(DaemonState{
		Addr:       hello.Addr,
		Kind:       hello.Kind,
		TokenPath:  hello.TokenPath,
		Generation: DaemonTokenGeneration(hello.TokenPath),
		LaunchKey:  launchKey,
	})
	if preservedState != nil {
		state.PrivateCacheDir = startCacheDir
	}
	writeState := preservedState == nil
	if writeState {
		if err := WriteDaemonState(statePath, state); err != nil {
			started.stop()
			removeFailedPrivateCache()
			return nil, fmt.Errorf("write daemon state %s for %s: %w", statePath, hello.Addr, err)
		}
	}
	api := NewClient(hello.Addr)
	if err := ApplyDaemonStateAuth(api, state); err != nil {
		if writeState {
			_ = os.Remove(statePath)
		}
		started.stop()
		removeFailedPrivateCache()
		return nil, fmt.Errorf("read daemon auth token: %w", err)
	}
	if err := api.HealthCheck(); err != nil {
		if writeState {
			_ = os.Remove(statePath)
		}
		started.stop()
		removeFailedPrivateCache()
		return nil, fmt.Errorf("ccvm daemon started at %s but health check failed: %w", hello.Addr, err)
	}
	if strings.TrimSpace(state.Kind) == "vmshd" && !apiCompatible(api, state) {
		if writeState {
			_ = os.Remove(statePath)
		}
		started.stop()
		removeFailedPrivateCache()
		return nil, fmt.Errorf("vmshd daemon started at %s but required routes are unavailable", hello.Addr)
	}
	if state.PrivateCacheDir != "" {
		if err := WriteDaemonState(filepath.Join(state.PrivateCacheDir, "owner.json"), state); err != nil {
			started.stop()
			_ = os.RemoveAll(state.PrivateCacheDir)
			return nil, fmt.Errorf("write private daemon owner manifest: %w", err)
		}
	}
	if opts.OnStart != nil {
		opts.OnStart(state)
	}
	return api, nil
}

func daemonEndpointReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", strings.TrimSpace(addr), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func reclaimStalePrivateDaemonCaches(cacheDir string) error {
	parent := filepath.Join(cacheDir, "private")
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "vmshd-") {
			continue
		}
		dir := filepath.Join(parent, entry.Name())
		state, err := ReadDaemonState(filepath.Join(dir, "owner.json"))
		if err != nil {
			continue
		}
		api := NewClient(state.Addr)
		if ApplyDaemonStateAuth(api, state) == nil && api.HealthCheck() == nil {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

func RemovePrivateDaemonCacheWhenStopped(api *client.Client, dir string) error {
	dir = strings.TrimSpace(dir)
	if api == nil || dir == "" {
		return nil
	}
	deadline := time.Now().Add(daemonWatchdogTimeout())
	for api.HealthCheck() == nil {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("private daemon remained active after its watchdog lease was released")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return os.RemoveAll(dir)
}

type daemonStartupResult struct {
	hello client.ServerHello
	err   error
}

func readDaemonStartupBanner(started *startedDaemonProcess, launch CCVMLaunch, timeout time.Duration) (client.ServerHello, error) {
	result := make(chan daemonStartupResult, 1)
	go func() {
		var hello client.ServerHello
		err := json.NewDecoder(started.stdout).Decode(&hello)
		result <- daemonStartupResult{hello: hello, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case decoded := <-result:
		if decoded.err != nil {
			started.stop()
			return client.ServerHello{}, fmt.Errorf("ccvm daemon did not send a startup banner from %s: %w", CCVMLaunchName(launch), decoded.err)
		}
		return decoded.hello, nil
	case <-timer.C:
		started.stop()
		_ = started.stdout.Close()
		return client.ServerHello{}, fmt.Errorf("ccvm daemon did not send a startup banner from %s within %s: %w", CCVMLaunchName(launch), timeout, ErrDaemonStartupTimeout)
	}
}

func privateDaemonCacheDir(cacheDir string) (string, error) {
	parent := filepath.Join(cacheDir, "private")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "vmshd-")
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

type startedDaemonProcess struct {
	stdout  io.ReadCloser
	release func()
	stop    func()
	done    <-chan struct{}
}

var startDaemonProcess = startDaemonCommand

func startDaemonCommand(launch CCVMLaunch, cacheDir string) (*startedDaemonProcess, error) {
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
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		_ = proc.Wait()
		close(done)
	}()
	var releaseOnce sync.Once
	var stopOnce sync.Once
	return &startedDaemonProcess{
		stdout: stdout,
		release: func() {
			releaseOnce.Do(func() {
				go func() {
					_, _ = io.Copy(io.Discard, stdout)
					_ = stdout.Close()
				}()
			})
		},
		stop: func() {
			stopOnce.Do(func() {
				if proc.Process != nil {
					_ = proc.Process.Kill()
				}
			})
			<-done
		},
		done: done,
	}, nil
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

func daemonReusable(api *client.Client, state DaemonState, launchKey string) (bool, error) {
	if strings.TrimSpace(state.Kind) != "vmshd" && state.LaunchKey != launchKey {
		return false, fmt.Errorf("daemon launch identity changed")
	}
	if !apiCompatible(api, state) {
		return false, fmt.Errorf("daemon required routes are unavailable")
	}
	if strings.TrimSpace(state.Kind) != "vmshd" {
		return true, nil
	}
	if err := vmshdProtocolCompatible(state); err != nil {
		return false, err
	}
	return true, nil
}

func vmshdProtocolCompatible(state DaemonState) error {
	req, err := http.NewRequest(http.MethodGet, "http://"+state.Addr+"/vmsh/protocol", nil)
	if err != nil {
		return err
	}
	q := req.URL.Query()
	q.Set("frontend_protocol", strconv.Itoa(vmshdprotocol.Current))
	q.Set("frontend_min_protocol", strconv.Itoa(vmshdprotocol.Minimum))
	q.Set("frontend_name", "vmsh")
	req.URL.RawQuery = q.Encode()
	if strings.TrimSpace(state.TokenPath) != "" {
		token, err := ReadDaemonToken(state.TokenPath)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("vmshd protocol discovery route is unavailable")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vmshd protocol discovery returned status %d", resp.StatusCode)
	}
	var info vmshdprotocol.Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return err
	}
	if strings.TrimSpace(info.Kind) != "vmshd" {
		return fmt.Errorf("vmshd protocol discovery returned kind %q", info.Kind)
	}
	if strings.TrimSpace(info.Protocol.Name) != vmshdprotocol.Name {
		return fmt.Errorf("vmshd protocol discovery returned protocol %q", info.Protocol.Name)
	}
	if info.Protocol.Current < vmshdprotocol.Minimum {
		return fmt.Errorf("vmshd protocol is older than this frontend supports")
	}
	if info.Protocol.Minimum > vmshdprotocol.Current {
		return fmt.Errorf("vmshd protocol is newer than this frontend supports")
	}
	if !info.Compatibility.Compatible {
		return fmt.Errorf("vmshd protocol is incompatible: %s", strings.TrimSpace(info.Compatibility.Reason))
	}
	return nil
}

func firstNonNil(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
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
	if strings.TrimSpace(hello.Kind) == "vmshd" && !isLoopbackAddr(hello.Addr) {
		return fmt.Errorf("vmshd daemon sent a non-loopback address: %s", hello.Addr)
	}
	return nil
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func NewClient(addr string) *client.Client {
	api := client.NewClient("http://"+addr, func() (net.Conn, error) {
		return net.Dial("tcp", addr)
	})
	SetFrontendProtocolHeaders(api)
	return api
}

func SetFrontendProtocolHeaders(api interface{ SetHeader(string, string) }) {
	if api == nil {
		return
	}
	api.SetHeader(vmshdprotocol.HeaderProtocol, strconv.Itoa(vmshdprotocol.Current))
	api.SetHeader(vmshdprotocol.HeaderMinProtocol, strconv.Itoa(vmshdprotocol.Minimum))
	api.SetHeader(vmshdprotocol.HeaderName, "vmsh")
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

type WatchdogLeaseError struct {
	Operation string
	LeaseID   string
	Err       error
}

func (e *WatchdogLeaseError) Error() string {
	return fmt.Sprintf("watchdog lease %s %s: %v", e.LeaseID, e.Operation, e.Err)
}

func (e *WatchdogLeaseError) Unwrap() error { return e.Err }

func StartDaemonLease(api watchdogAPI, onError ...func(error)) (func(), error) {
	report := func(error) {}
	if len(onError) > 0 && onError[0] != nil {
		report = onError[0]
	}
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
				if err := api.FeedWatchdogLease(lease.LeaseID); err != nil {
					report(&WatchdogLeaseError{Operation: "feed failed", LeaseID: lease.LeaseID, Err: err})
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
		if err := api.ReleaseWatchdogLease(lease.LeaseID); err != nil {
			report(&WatchdogLeaseError{Operation: "release failed", LeaseID: lease.LeaseID, Err: err})
		}
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
	daemonStateFileMu.RLock()
	defer daemonStateFileMu.RUnlock()
	return readDaemonState(path)
}

func readDaemonState(path string) (DaemonState, error) {
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
	return withDaemonStateWriteLock(path, func() error {
		return writeDaemonStateAtomically(path, append(data, '\n'), replaceDaemonStateFile)
	})
}

func RemoveDaemonStateIfUnchanged(path string, expected DaemonState) (bool, error) {
	removed := false
	err := withDaemonStateWriteLock(path, func() error {
		current, err := ReadDaemonState(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current != expected {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed = true
		return nil
	})
	return removed, err
}

func writeDaemonStateAtomically(path string, data []byte, replace func(string, string) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	if _, err := readDaemonState(tmpPath); err != nil {
		return err
	}
	daemonStateFileMu.Lock()
	defer daemonStateFileMu.Unlock()
	if err := replace(tmpPath, path); err != nil {
		return err
	}
	_ = syncDaemonStateDir(dir)
	return nil
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

func DaemonTokenGeneration(path string) string {
	base := filepath.Base(strings.TrimSpace(path))
	const prefix = "vmshd-"
	const suffix = ".token"
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, suffix) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(base, prefix), suffix)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
