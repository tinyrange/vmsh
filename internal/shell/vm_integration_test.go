package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/tinyrange/vmsh/internal/backend"
	"j5.nz/cc/client"
)

const vmIntegrationTestImage = "vmsh-integration-alpine"

var vmIntegrationCCVMBuild struct {
	once     sync.Once
	path     string
	buildDir string
	err      error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if vmIntegrationCCVMBuild.buildDir != "" {
		_ = os.RemoveAll(vmIntegrationCCVMBuild.buildDir)
	}
	os.Exit(code)
}

func TestVMIntegrationScriptCommandsStartVMAndUseShellFeatures(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)

	mustWriteTestFile(t, filepath.Join(sh.hostCWD, "host-input.txt"), "from-host-copy\n")
	script := strings.Join([]string{
		"@" + env.image + " --vm script --memory 768 --cpus 1 --no-network",
		"printf 'guest-start:%s:%s\\n' \"$(uname -s)\" \"$(id -u)\"",
		"@status",
		"export VMSH_REALVM_EXPORT=from-vmsh",
		"printf 'guest-env:%s\\n' \"$VMSH_REALVM_EXPORT\"",
		"printf guest-host-file > vmsh-host-file.txt",
		"@copy @:vmsh-host-file.txt @host:guest-from-script.txt",
		"@copy @host:host-input.txt @host:host-copy.txt",
		"cd /tmp",
		"pwd",
		"printf guest-cwd-file > vmsh-script.txt",
		"cat vmsh-script.txt",
		"@host echo host-ok",
		"@alias sayhost=@host echo alias-host-ok",
		"sayhost",
		"@" + env.image + " --vm script --no-network printf 'direct:%s\\n' \"$(cat /tmp/vmsh-script.txt)\"",
		"printf 'alpha\\nbeta\\n' | grep beta",
		"@sudo sh -lc 'echo sudo:$(id -u)'",
		"@jobs",
		"@stop --vm script",
		"@ps",
	}, "\n")

	stdout, stderr, err := sh.runTestScript(script)
	if err != nil {
		t.Fatalf("run vmsh script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	requireContains(t, stdout, "context: vm")
	requireContains(t, stdout, "image: "+env.image)
	requireContains(t, stdout, "vm: script")
	requireContains(t, stdout, "vm status: running")
	requireContains(t, stdout, "guest-start:Linux:1000")
	requireContains(t, stdout, "guest-env:from-vmsh")
	requireContains(t, stdout, "/tmp")
	requireContains(t, stdout, "guest-cwd-file")
	requireContains(t, stdout, "host-ok")
	requireContains(t, stdout, "alias-host-ok")
	requireContains(t, stdout, "direct:guest-cwd-file")
	requireContains(t, stdout, "beta")
	requireContains(t, stdout, "sudo:0")
	requireContains(t, stdout, "No jobs")

	copied, err := os.ReadFile(filepath.Join(sh.hostCWD, "guest-from-script.txt"))
	if err != nil {
		t.Fatalf("read copied guest file: %v", err)
	}
	if string(copied) != "guest-host-file" {
		t.Fatalf("copied guest file = %q, want %q", string(copied), "guest-host-file")
	}
	hostCopy, err := os.ReadFile(filepath.Join(sh.hostCWD, "host-copy.txt"))
	if err != nil {
		t.Fatalf("read host copy: %v", err)
	}
	if string(hostCopy) != "from-host-copy\n" {
		t.Fatalf("host copy = %q, want %q", string(hostCopy), "from-host-copy\n")
	}

	state, err := env.api.InstanceStatusOf("script")
	if err != nil {
		t.Fatalf("status after stop: %v", err)
	}
	if state.Status != "stopped" {
		t.Fatalf("VM status after @stop = %q, want stopped", state.Status)
	}
}

func TestVMIntegrationManagesVMAndImages(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)

	savedImage := "vmsh-integration-saved"
	script := strings.Join([]string{
		"@" + env.image + " --vm manage --memory 768 --cpus 1 --no-network",
		"@start --vm manage",
		"printf warm-root > /tmp/vmsh-manage.txt",
		"@restart --vm manage",
		"if test -e /tmp/vmsh-manage.txt; then printf 'restart-kept\\n'; else printf 'restart-cleared\\n'; fi",
		"@save --vm manage " + savedImage,
		"@rmi " + savedImage,
		"@stop --vm manage",
	}, "\n")

	stdout, stderr, err := sh.runTestScript(script)
	if err != nil {
		t.Fatalf("run vmsh management script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	requireContains(t, stdout, "restart-cleared")
	requireContains(t, stdout, "Saved manage as "+savedImage)
	requireContains(t, stdout, "Removed "+savedImage)
	if _, err := env.api.GetImage(savedImage); err == nil {
		t.Fatalf("saved image %q still exists after @rmi", savedImage)
	}
}

func TestVMIntegrationPersistentTTYGuestShellState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent TTY shell test requires a Unix PTY")
	}
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)

	stdout, stderr, err := sh.runTestScript("@" + env.image + " --vm tty --memory 768 --cpus 1 --no-network\n")
	if err != nil {
		t.Fatalf("select VM context: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	for _, line := range []string{
		"VMSH_TTY_VAR=warm",
		"alias vm_alias='printf \"alias:%s\\n\" \"$VMSH_TTY_VAR\"'",
		"vm_func(){ printf \"func:%s:%s\\n\" \"$VMSH_TTY_VAR\" \"$PWD\"; }",
		"cd /tmp",
	} {
		if out, err := sh.evalOnTestPTY(line); err != nil {
			t.Fatalf("eval %q on TTY: %v\noutput:\n%s", line, err, out)
		}
	}

	out, err := sh.evalOnTestPTY("vm_alias")
	if err != nil {
		t.Fatalf("run persisted alias: %v\noutput:\n%s", err, out)
	}
	requireContains(t, out, "alias:warm")

	out, err = sh.evalOnTestPTY("vm_func")
	if err != nil {
		t.Fatalf("run persisted function: %v\noutput:\n%s", err, out)
	}
	requireContains(t, out, "func:warm:/tmp")
	if sh.context.CWD != "/tmp" {
		t.Fatalf("guest context cwd = %q, want /tmp", sh.context.CWD)
	}

	if err := sh.stopVM("tty"); err != nil {
		t.Fatalf("stop tty VM: %v", err)
	}
}

type vmIntegrationTestEnv struct {
	api      *client.Client
	cacheDir string
	image    string
}

func newVMIntegrationTestEnv(t *testing.T) *vmIntegrationTestEnv {
	t.Helper()
	skipUnsupportedVMIntegrationPlatform(t)

	cacheDir := filepath.Join(t.TempDir(), "cache")
	statePath := filepath.Join(cacheDir, "ccvm.json")
	ccvm := buildVMIntegrationCCVM(t)
	api, err := backend.ConnectCCVM(backend.CCVMLaunch{
		Path: ccvm,
		Env: []string{
			"CCX3_OCI_SHARED_CACHE_DIR=" + filepath.Join(cacheDir, "oci-shared"),
			"CCX3_VM_BOOT_TIMEOUT=" + vmIntegrationTimeoutSeconds(),
			"VMSH_VM_BOOT_TIMEOUT=" + vmIntegrationTimeoutSeconds(),
		},
	}, cacheDir, statePath)
	if err != nil {
		t.Fatalf("start ccvm daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = api.Shutdown()
		_ = os.Remove(statePath)
	})

	caps, err := api.Capabilities()
	if err != nil {
		t.Fatalf("get ccvm capabilities: %v", err)
	}
	if !caps.VMSupported {
		t.Skipf("VM runtime unsupported on this host (%s/%s, backend %s): %s", runtime.GOOS, runtime.GOARCH, caps.Backend, caps.VMError)
	}

	fixture := filepath.Join(vmIntegrationRepoRoot(t), "cc", "fixtures", "alpine.simg")
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("Alpine SIMG fixture is unavailable at %s: %v", fixture, err)
	}
	if err := api.PullImageStream(vmIntegrationTestImage, client.PullImageRequest{
		Source:       fixture,
		Architecture: "amd64",
	}, nil); err != nil {
		t.Fatalf("import Alpine SIMG fixture: %v", err)
	}

	return &vmIntegrationTestEnv{
		api:      api,
		cacheDir: cacheDir,
		image:    vmIntegrationTestImage,
	}
}

func (e *vmIntegrationTestEnv) newShell(t *testing.T) *shellState {
	t.Helper()
	caps, err := e.api.Capabilities()
	if err != nil {
		t.Fatalf("get capabilities: %v", err)
	}
	hostCWD := t.TempDir()
	sh := &shellState{
		api:        e.api,
		context:    defaultContext("default", e.image, caps.SupportsNestedVirt),
		hostCWD:    hostCWD,
		rootCache:  e.cacheDir,
		imageCache: map[string]bool{e.image: true},
		vmRunning:  map[string]bool{},
		contextCWD: map[string]string{},
		promptOut:  io.Discard,
		env:        map[string]string{},
		aliases:    map[string]string{},
		confirmPull: func(string, io.Writer) (bool, error) {
			return true, nil
		},
		confirmVMRestart: func(string, io.Writer) (bool, error) {
			return true, nil
		},
	}
	sh.completion = newVMSHCompleter(sh)
	t.Cleanup(sh.closeSessions)
	t.Cleanup(func() {
		for _, id := range []string{"default", "script", "manage", "tty"} {
			_ = e.api.ShutdownInstanceWithID(id)
		}
	})
	return sh
}

func (s *shellState) runTestScript(script string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := s.evalScriptLines(strings.NewReader(script), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func (s *shellState) evalOnTestPTY(line string) (string, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return "", err
	}
	defer master.Close()

	outCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		_, copyErr := io.Copy(&out, master)
		outCh <- strings.ReplaceAll(out.String(), "\r\n", "\n")
		errCh <- copyErr
	}()

	var stderr bytes.Buffer
	evalErr := s.eval(line, slave, &stderr)
	_ = slave.Close()

	var out string
	select {
	case out = <-outCh:
	case <-time.After(5 * time.Second):
		_ = master.Close()
		out = <-outCh
	}
	copyErr := <-errCh
	if evalErr != nil {
		return out + stderr.String(), evalErr
	}
	if copyErr != nil && !isPTYClosedError(copyErr) {
		return out + stderr.String(), copyErr
	}
	return out + stderr.String(), nil
}

func buildVMIntegrationCCVM(t *testing.T) string {
	t.Helper()
	if ccvm := strings.TrimSpace(os.Getenv("VMSH_TEST_CCVM")); ccvm != "" {
		return ccvm
	}
	vmIntegrationCCVMBuild.once.Do(func() {
		root := vmIntegrationRepoRoot(t)
		buildDir, err := os.MkdirTemp("", "vmsh-integration-build-*")
		if err != nil {
			vmIntegrationCCVMBuild.err = err
			return
		}
		vmIntegrationCCVMBuild.buildDir = buildDir
		out := filepath.Join(buildDir, backend.HostExecutableName("ccvm"))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-tags", "embed_guestinit", "-o", out, "./cmd/ccvm")
		cmd.Dir = filepath.Join(root, "cc")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Run(); err != nil {
			vmIntegrationCCVMBuild.err = fmt.Errorf("go build ccvm: %w\n%s", err, output.String())
			return
		}
		vmIntegrationCCVMBuild.path = out
	})
	if vmIntegrationCCVMBuild.err != nil {
		t.Fatalf("build ccvm for VM integration tests: %v", vmIntegrationCCVMBuild.err)
	}
	return vmIntegrationCCVMBuild.path
}

func vmIntegrationRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func skipUnsupportedVMIntegrationPlatform(t *testing.T) {
	t.Helper()
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64":
		return
	default:
		t.Skipf("VM integration tests require KVM/HVF/WHP; unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

func vmIntegrationTimeoutSeconds() string {
	if value := strings.TrimSpace(os.Getenv("VMSH_VM_INTEGRATION_TIMEOUT_SECONDS")); value != "" {
		return value
	}
	return "180"
}

func isPTYClosedError(err error) bool {
	if err == nil || errors.Is(err, os.ErrClosed) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "input/output error") ||
		strings.Contains(text, "file already closed") ||
		strings.Contains(text, "bad file descriptor")
}

func mustWriteTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func requireContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("output does not contain %q\noutput:\n%s", want, text)
	}
}
