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
	"reflect"
	"runtime"
	"sort"
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
	vmshPath string
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
		"@sudo",
		"printf 'sudo-shell:%s\\n' \"$(id -u)\"",
		"exit",
		"printf 'after-sudo-shell:%s\\n' \"$(id -u)\"",
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
	requireContains(t, stdout, "sudo-shell:0")
	requireContains(t, stdout, "after-sudo-shell:1000")
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

func TestVMIntegrationFreeBSDBuiltinRunsCommandsAndCopiesFiles(t *testing.T) {
	if os.Getenv("VMSH_TEST_FREEBSD") == "" {
		t.Skip("set VMSH_TEST_FREEBSD=1 to run FreeBSD vmsh integration test")
	}
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)
	t.Cleanup(func() {
		_ = env.api.ShutdownInstanceWithID("freebsd")
	})

	mustWriteTestFile(t, filepath.Join(sh.hostCWD, "host-input.txt"), "from-host\n")
	script := strings.Join([]string{
		"@freebsd --vm freebsd --memory 1024 --cpus 1",
		"printf 'guest:%s:%s\\n' \"$(uname -s)\" \"$(id -u)\"",
		"@status",
		"pwd",
		"printf root-workdir > root-workdir.txt",
		"cat root-workdir.txt",
		"@copy @host:host-input.txt @:/tmp/vmsh-freebsd-input.txt",
		"cat /tmp/vmsh-freebsd-input.txt",
		"printf freebsd-output > /tmp/vmsh-freebsd-output.txt",
		"@copy @:/tmp/vmsh-freebsd-output.txt @host:guest-output.txt",
		"@vm:freebsd printf 'direct:%s\\n' \"$(cat /tmp/vmsh-freebsd-input.txt)\"",
		"@stop --vm freebsd",
	}, "\n")

	stdout, stderr, err := sh.runTestScriptWithTimeout(script, 3*time.Minute)
	if err != nil {
		t.Fatalf("run FreeBSD vmsh script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	requireContains(t, stdout, "guest:FreeBSD:0")
	requireContains(t, stdout, "context: vm")
	requireContains(t, stdout, "image: @freebsd")
	requireContains(t, stdout, "vm: freebsd")
	requireContains(t, stdout, "/root")
	requireContains(t, stdout, "root-workdir")
	requireContains(t, stdout, "from-host")
	requireContains(t, stdout, "direct:from-host")
	copied, err := os.ReadFile(filepath.Join(sh.hostCWD, "guest-output.txt"))
	if err != nil {
		t.Fatalf("read copied FreeBSD file: %v", err)
	}
	if string(copied) != "freebsd-output" {
		t.Fatalf("copied FreeBSD file = %q, want freebsd-output", string(copied))
	}
	state, err := env.api.InstanceStatusOf("freebsd")
	if err != nil {
		t.Fatalf("status after FreeBSD stop: %v", err)
	}
	if state.Status != "stopped" {
		t.Fatalf("FreeBSD VM status after @stop = %q, want stopped", state.Status)
	}
}

func TestVMIntegrationCopiesDirectoryMetadataHostToVMToHost(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)
	t.Cleanup(func() {
		_ = env.api.ShutdownInstanceWithID("copy-meta")
	})

	src := filepath.Join(sh.hostCWD, "meta-src")
	dst := filepath.Join(sh.hostCWD, "meta-back")
	fileMtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.Local)
	mustWriteTestFile(t, filepath.Join(src, "script.sh"), "#!/bin/sh\necho hi\n")
	mustWriteTestFile(t, filepath.Join(src, "nested", "file.txt"), "nested\n")
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatalf("create empty dir: %v", err)
	}
	if err := os.Chmod(filepath.Join(src, "script.sh"), 0o755); err != nil {
		t.Fatalf("chmod script: %v", err)
	}
	if err := os.Symlink("script.sh", filepath.Join(src, "script-link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	for _, path := range []string{
		filepath.Join(src, "script.sh"),
		filepath.Join(src, "nested", "file.txt"),
		filepath.Join(src, "empty"),
	} {
		if err := os.Chtimes(path, fileMtime, fileMtime); err != nil {
			t.Fatalf("set mtime %s: %v", path, err)
		}
	}

	script := strings.Join([]string{
		"@" + env.image + " --vm copy-meta --memory 768 --cpus 1 --no-network",
		"@copy @host:meta-src @vm:copy-meta:/tmp/vmsh-meta-vm",
		"@vm:copy-meta test -x /tmp/vmsh-meta-vm/script.sh",
		"@vm:copy-meta test -L /tmp/vmsh-meta-vm/script-link",
		"@vm:copy-meta test \"$(readlink /tmp/vmsh-meta-vm/script-link)\" = script.sh",
		"@copy @vm:copy-meta:/tmp/vmsh-meta-vm @host:meta-back",
		"@stop --vm copy-meta",
	}, "\n")

	stdout, stderr, err := sh.runTestScriptWithTimeout(script, 45*time.Second)
	if err != nil {
		t.Fatalf("run metadata copy script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	assertCopiedMetadataTree(t, src, dst)
}

func TestVMIntegrationCopiesLargeFileHostToVM(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)
	t.Cleanup(func() {
		_ = env.api.ShutdownInstanceWithID("copy-large")
	})

	const largeSize = 128 * 1024 * 1024
	src := filepath.Join(sh.hostCWD, "large.bin")
	file, err := os.Create(src)
	if err != nil {
		t.Fatalf("create large source: %v", err)
	}
	if err := file.Truncate(largeSize); err != nil {
		_ = file.Close()
		t.Fatalf("size large source: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close large source: %v", err)
	}

	script := strings.Join([]string{
		"@" + env.image + " --vm copy-large --memory 768 --cpus 1 --no-network",
		"@copy @host:large.bin @vm:copy-large:/root/large.bin",
		fmt.Sprintf("@vm:copy-large sh -lc 'test \"$(wc -c < /root/large.bin)\" = %d'", largeSize),
		"@stop --vm copy-large",
	}, "\n")

	stdout, stderr, err := sh.runTestScriptWithTimeout(script, 45*time.Second)
	if err != nil {
		t.Fatalf("run large copy script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
}

func TestVMIntegrationCopiesDirectoryMetadataThroughIsolatedVM(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)
	t.Cleanup(func() {
		_ = env.api.ShutdownInstanceWithID("copy-iso-isolated")
	})

	src := filepath.Join(sh.hostCWD, "meta-src")
	dst := filepath.Join(sh.hostCWD, "meta-back")
	createMetadataCopyFixture(t, src)

	script := strings.Join([]string{
		"@" + env.image + " --vm copy-iso --isolated --memory 768 --cpus 1 --no-network",
		"@copy @host:meta-src @:/tmp/vmsh-meta-vm",
		"test -x /tmp/vmsh-meta-vm/script.sh",
		"test -d /tmp/vmsh-meta-vm/empty",
		"test -L /tmp/vmsh-meta-vm/script-link",
		"test \"$(readlink /tmp/vmsh-meta-vm/script-link)\" = script.sh",
		"@copy @:/tmp/vmsh-meta-vm @host:meta-back",
	}, "\n")

	stdout, stderr, err := sh.runTestScriptWithTimeout(script, 60*time.Second)
	if err != nil {
		t.Fatalf("run isolated metadata copy script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	assertCopiedMetadataTree(t, src, dst)
}

func TestVMIntegrationCopiesDirectoryMetadataBetweenVMs(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)
	t.Cleanup(func() {
		_ = env.api.ShutdownInstanceWithID("copy-src")
		_ = env.api.ShutdownInstanceWithID("copy-dst")
	})

	src := filepath.Join(sh.hostCWD, "meta-src")
	dst := filepath.Join(sh.hostCWD, "meta-back")
	createMetadataCopyFixture(t, src)

	script := strings.Join([]string{
		"@" + env.image + " --vm copy-src --memory 768 --cpus 1 --no-network",
		"@" + env.image + " --vm copy-dst --memory 768 --cpus 1 --no-network",
		"@copy @host:meta-src @vm:copy-src:/tmp/vmsh-meta-src",
		"@copy @vm:copy-src:/tmp/vmsh-meta-src @vm:copy-dst:/tmp/vmsh-meta-dst",
		"@vm:copy-dst test -x /tmp/vmsh-meta-dst/script.sh",
		"@vm:copy-dst test -d /tmp/vmsh-meta-dst/empty",
		"@vm:copy-dst test -L /tmp/vmsh-meta-dst/script-link",
		"@vm:copy-dst test \"$(readlink /tmp/vmsh-meta-dst/script-link)\" = script.sh",
		"@copy @vm:copy-dst:/tmp/vmsh-meta-dst @host:meta-back",
		"@stop --vm copy-src",
		"@stop --vm copy-dst",
	}, "\n")

	stdout, stderr, err := sh.runTestScriptWithTimeout(script, 75*time.Second)
	if err != nil {
		t.Fatalf("run VM-to-VM metadata copy script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	assertCopiedMetadataTree(t, src, dst)
}

func TestVMIntegrationCopiesWeirdFilenamesThroughVMAndIsolatedVM(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)
	t.Cleanup(func() {
		_ = env.api.ShutdownInstanceWithID("copy-weird")
		_ = env.api.ShutdownInstanceWithID("copy-weird-iso-isolated")
	})

	src := filepath.Join(sh.hostCWD, "weird-src")
	vmBack := filepath.Join(sh.hostCWD, "vm-back")
	isoBack := filepath.Join(sh.hostCWD, "iso-back")
	names := createWeirdNameCopyFixture(t, src)

	script := strings.Join([]string{
		"@" + env.image + " --vm copy-weird --memory 768 --cpus 1 --no-network",
		"@copy @host:weird-src @vm:copy-weird:/tmp/weird-vm",
		"@copy @vm:copy-weird:/tmp/weird-vm @host:vm-back",
		"@" + env.image + " --vm copy-weird-iso --isolated --memory 768 --cpus 1 --no-network",
		"@copy @host:weird-src @:/tmp/weird-iso",
		"@copy @:/tmp/weird-iso @host:iso-back",
	}, "\n")

	stdout, stderr, err := sh.runTestScriptWithTimeout(script, 75*time.Second)
	if err != nil {
		t.Fatalf("run weird filename VM copy script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	assertWeirdNameCopyTree(t, vmBack, names)
	assertWeirdNameCopyTree(t, isoBack, names)
}

func TestVMIntegrationPastedCopyDirectoryMetadataHostToVMToHost(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)
	t.Cleanup(func() {
		_ = env.api.ShutdownInstanceWithID("copy-paste")
	})

	root := t.TempDir()
	src := filepath.Join(root, "meta-src")
	dst := filepath.Join(root, "meta-back")
	createMetadataCopyFixture(t, src)

	paste := strings.Join([]string{
		"@" + env.image + " --vm copy-paste --memory 768 --cpus 1 --no-network",
		"@copy @host:" + src + " @vm:copy-paste:/tmp/vmsh-meta-vm",
		"@vm:copy-paste test -x /tmp/vmsh-meta-vm/script.sh",
		"@vm:copy-paste test -L /tmp/vmsh-meta-vm/script-link",
		"@vm:copy-paste test \"$(readlink /tmp/vmsh-meta-vm/script-link)\" = script.sh",
		"@copy @vm:copy-paste:/tmp/vmsh-meta-vm @host:" + dst,
		"@stop --vm copy-paste",
	}, "\n")

	stdout, stderr, err := sh.runPastedLinesWithTimeout(paste, 45*time.Second)
	if err != nil {
		t.Fatalf("run pasted metadata copy: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	assertCopiedMetadataTree(t, src, dst)
}

func TestVMIntegrationInteractivePasteCopiesDirectoryMetadataHostToVMToHost(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	t.Cleanup(func() {
		_ = env.api.ShutdownInstanceWithID("copy-pty-paste")
	})

	vmsh := buildVMIntegrationVMSH(t)
	hostCWD := t.TempDir()
	src := filepath.Join(hostCWD, "meta-src")
	dst := filepath.Join(hostCWD, "meta-back")
	createMetadataCopyFixture(t, src)

	session := startVMIntegrationPTY(t, vmsh, env.cacheDir, buildVMIntegrationCCVM(t), hostCWD)
	defer session.close()
	session.expect("vmsh", 10*time.Second)

	paste := strings.Join([]string{
		"@" + env.image + " --vm copy-pty-paste --memory 768 --cpus 1 --no-network",
		"@copy @host:meta-src @vm:copy-pty-paste:/tmp/vmsh-meta-vm",
		"@vm:copy-pty-paste test -x /tmp/vmsh-meta-vm/script.sh",
		"@vm:copy-pty-paste test -L /tmp/vmsh-meta-vm/script-link",
		"@vm:copy-pty-paste test \"$(readlink /tmp/vmsh-meta-vm/script-link)\" = script.sh",
		"@copy @vm:copy-pty-paste:/tmp/vmsh-meta-vm @host:meta-back",
		"@stop --vm copy-pty-paste",
		"@host echo VM_COPY_PASTE_DONE",
	}, "\n") + "\n"
	session.write(paste)
	session.expectOccurrences("VM_COPY_PASTE_DONE", 2, 60*time.Second)
	waitForPath(t, filepath.Join(dst, "script.sh"), 60*time.Second, func() string {
		return session.snapshot()
	})

	assertCopiedMetadataTree(t, src, dst)
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

func TestVMIntegrationSavesLoadedVMFilesystemFiles(t *testing.T) {
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)

	savedImage := "vmsh-integration-loaded-save"
	t.Cleanup(func() {
		_ = env.api.DeleteImage(savedImage)
	})

	mustWriteTestFile(t, filepath.Join(sh.hostCWD, "seed", "root.txt"), "loaded-root\n")
	mustWriteTestFile(t, filepath.Join(sh.hostCWD, "seed", "nested", "child.txt"), "loaded-child\n")
	script := strings.Join([]string{
		"@" + env.image + " --vm save-load --memory 768 --cpus 1 --no-network",
		"@copy @host:seed/root.txt @:~/loaded/root.txt",
		"@copy @host:seed/nested/child.txt @:~/loaded/nested/child.txt",
		"printf 'before-save:%s:%s\\n' \"$(cat ~/loaded/root.txt)\" \"$(cat ~/loaded/nested/child.txt)\"",
		"@save --vm save-load " + savedImage,
		"@stop --vm save-load",
		"@" + savedImage + " --vm saved-load --memory 768 --cpus 1 --no-network",
		"printf 'after-save:%s:%s\\n' \"$(cat ~/loaded/root.txt)\" \"$(cat ~/loaded/nested/child.txt)\"",
		"@rmi " + savedImage,
		"@stop --vm saved-load",
	}, "\n")

	stdout, stderr, err := sh.runTestScript(script)
	if err != nil {
		t.Fatalf("run vmsh save loaded filesystem script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	requireContains(t, stdout, "before-save:loaded-root:loaded-child")
	requireContains(t, stdout, "Saved save-load as "+savedImage)
	requireContains(t, stdout, "after-save:loaded-root:loaded-child")
	requireContains(t, stdout, "Removed "+savedImage)
	if _, err := env.api.GetImage(savedImage); err == nil {
		t.Fatalf("saved image %q still exists after @rmi", savedImage)
	}
}

func TestVMIntegrationSavesAfterPersistentTTYGuestShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent TTY shell test requires a Unix PTY")
	}
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)

	savedImage := "vmsh-integration-tty-save"
	t.Cleanup(func() {
		_ = env.api.DeleteImage(savedImage)
	})

	stdout, stderr, err := sh.runTestScript("@" + env.image + " --vm tty-save --memory 768 --cpus 1 --no-network\n")
	if err != nil {
		t.Fatalf("select VM context: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	out, err := sh.evalOnTestPTY("mkdir -p ~/saved && printf tty-persist > ~/saved/value.txt")
	if err != nil {
		t.Fatalf("write persistent TTY shell file: %v\noutput:\n%s", err, out)
	}

	script := strings.Join([]string{
		"@save --vm tty-save " + savedImage,
		"@stop --vm tty-save",
		"@" + savedImage + " --vm tty-saved --memory 768 --cpus 1 --no-network",
		"printf 'saved-tty:%s\\n' \"$(cat ~/saved/value.txt)\"",
		"@rmi " + savedImage,
		"@stop --vm tty-saved",
	}, "\n")
	stdout, stderr, err = sh.runTestScript(script)
	if err != nil {
		t.Fatalf("run vmsh TTY save script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	requireContains(t, stdout, "Saved tty-save as "+savedImage)
	requireContains(t, stdout, "saved-tty:tty-persist")
	requireContains(t, stdout, "Removed "+savedImage)
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

func TestVMIntegrationPersistentTTYSudoSubshell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent TTY shell test requires a Unix PTY")
	}
	env := newVMIntegrationTestEnv(t)
	sh := env.newShell(t)

	stdout, stderr, err := sh.runTestScript("@" + env.image + " --vm sudo-tty --memory 768 --cpus 1 --no-network\n")
	if err != nil {
		t.Fatalf("select VM context: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	out, err := sh.evalOnTestPTY("id -u")
	if err != nil {
		t.Fatalf("run id before sudo subshell: %v\noutput:\n%s", err, out)
	}
	requireContains(t, out, "1000")

	out, err = sh.evalOnTestPTY("@sudo")
	if err != nil {
		t.Fatalf("enter sudo subshell: %v\noutput:\n%s", err, out)
	}
	if sh.context.User != "root" {
		t.Fatalf("sudo context user = %q, want root", sh.context.User)
	}

	out, err = sh.evalOnTestPTY("id -u")
	if err != nil {
		t.Fatalf("run id in sudo subshell: %v\noutput:\n%s", err, out)
	}
	requireContains(t, out, "0")

	out, err = sh.evalOnTestPTY("exit")
	if err != nil {
		t.Fatalf("exit sudo subshell: %v\noutput:\n%s", err, out)
	}
	if sh.context.User == "root" {
		t.Fatalf("sudo context did not restore: %+v", sh.context)
	}

	out, err = sh.evalOnTestPTY("id -u")
	if err != nil {
		t.Fatalf("run id after sudo subshell: %v\noutput:\n%s", err, out)
	}
	requireContains(t, out, "1000")

	if err := sh.stopVM("sudo-tty"); err != nil {
		t.Fatalf("stop sudo-tty VM: %v", err)
	}
}

type vmIntegrationTestEnv struct {
	api      *client.Client
	cacheDir string
	image    string
}

type vmIntegrationPTY struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	done   chan error
	mu     sync.Mutex
	output bytes.Buffer
}

func newVMIntegrationTestEnv(t *testing.T) *vmIntegrationTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping VM integration test in short mode")
	}
	skipUnsupportedVMIntegrationPlatform(t)

	cacheBase := os.TempDir()
	if runtime.GOOS == "darwin" {
		cacheBase = "/tmp"
	}
	cacheParent, err := os.MkdirTemp(cacheBase, "vmsh-it-*")
	if err != nil {
		t.Fatalf("create VM integration cache dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(cacheParent)
	})
	cacheDir := filepath.Join(cacheParent, "cache")
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
		for _, id := range []string{"default", "script", "freebsd", "manage", "save-load", "saved-load", "tty-save", "tty-saved", "tty", "sudo-tty"} {
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

func (s *shellState) runTestScriptWithTimeout(script string, timeout time.Duration) (string, string, error) {
	type result struct {
		stdout string
		stderr string
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		stdout, stderr, err := s.runTestScript(script)
		ch <- result{stdout: stdout, stderr: stderr, err: err}
	}()
	select {
	case res := <-ch:
		return res.stdout, res.stderr, res.err
	case <-time.After(timeout):
		s.closeSessions()
		return "", "", fmt.Errorf("script timed out after %s", timeout)
	}
}

func (s *shellState) runPastedLinesWithTimeout(paste string, timeout time.Duration) (string, string, error) {
	type result struct {
		stdout string
		stderr string
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		err := s.evalPastedLines(paste, &stdout, &stderr)
		ch <- result{stdout: stdout.String(), stderr: stderr.String(), err: err}
	}()
	select {
	case res := <-ch:
		return res.stdout, res.stderr, res.err
	case <-time.After(timeout):
		s.closeSessions()
		return "", "", fmt.Errorf("pasted script timed out after %s", timeout)
	}
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
		for _, goarch := range []string{"arm64", "amd64"} {
			payload := filepath.Join(root, "cc", "internal", "guestinit", "guest-init-linux-"+goarch)
			cmd := exec.CommandContext(ctx, "go", "build", "-o", payload, "./internal/cmd/init")
			cmd.Dir = filepath.Join(root, "cc")
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goarch)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Run(); err != nil {
				vmIntegrationCCVMBuild.err = fmt.Errorf("go build guest init %s: %w\n%s", goarch, err, output.String())
				return
			}
		}
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

func buildVMIntegrationVMSH(t *testing.T) string {
	t.Helper()
	if vmsh := strings.TrimSpace(os.Getenv("VMSH_TEST_VMSH")); vmsh != "" {
		return vmsh
	}
	_ = buildVMIntegrationCCVM(t)
	vmIntegrationCCVMBuild.once.Do(func() {})
	if vmIntegrationCCVMBuild.err != nil {
		t.Fatalf("build ccvm for VM integration tests: %v", vmIntegrationCCVMBuild.err)
	}
	if vmIntegrationCCVMBuild.vmshPath != "" {
		return vmIntegrationCCVMBuild.vmshPath
	}
	root := vmIntegrationRepoRoot(t)
	buildDir := vmIntegrationCCVMBuild.buildDir
	if buildDir == "" {
		var err error
		buildDir, err = os.MkdirTemp("", "vmsh-integration-build-*")
		if err != nil {
			t.Fatalf("create vmsh integration build dir: %v", err)
		}
		vmIntegrationCCVMBuild.buildDir = buildDir
	}
	out := filepath.Join(buildDir, backend.HostExecutableName("vmsh"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/vmsh")
	cmd.Dir = root
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build vmsh: %v\n%s", err, output.String())
	}
	vmIntegrationCCVMBuild.vmshPath = out
	return out
}

func startVMIntegrationPTY(t *testing.T, vmsh, cacheDir, ccvm, cwd string) *vmIntegrationPTY {
	t.Helper()
	cmd := exec.Command(vmsh, "-ccvm", ccvm, "-cache-dir", cacheDir)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"CCX3_VM_BOOT_TIMEOUT="+vmIntegrationTimeoutSeconds(),
		"VMSH_VM_BOOT_TIMEOUT="+vmIntegrationTimeoutSeconds(),
	)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 120})
	if err != nil {
		t.Fatalf("start vmsh pty: %v", err)
	}
	s := &vmIntegrationPTY{
		t:      t,
		cmd:    cmd,
		master: master,
		done:   make(chan error, 1),
	}
	go func() {
		var buf [4096]byte
		for {
			n, err := master.Read(buf[:])
			if n > 0 {
				s.mu.Lock()
				_, _ = s.output.Write(buf[:n])
				s.mu.Unlock()
			}
			if err != nil {
				break
			}
		}
		s.done <- cmd.Wait()
	}()
	return s
}

func (s *vmIntegrationPTY) write(text string) {
	s.t.Helper()
	if _, err := io.WriteString(s.master, text); err != nil {
		s.t.Fatalf("write to vmsh pty: %v\noutput:\n%s", err, s.snapshot())
	}
}

func (s *vmIntegrationPTY) expect(want string, timeout time.Duration) {
	s.t.Helper()
	s.expectOccurrences(want, 1, timeout)
}

func (s *vmIntegrationPTY) expectOccurrences(want string, count int, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if strings.Count(s.snapshot(), want) >= count {
			return
		}
		select {
		case err := <-s.done:
			s.t.Fatalf("vmsh exited before %d occurrences of %q: %v\noutput:\n%s", count, want, err, s.snapshot())
		case <-ticker.C:
			if time.Now().After(deadline) {
				s.t.Fatalf("timed out waiting for %d occurrences of %q\noutput:\n%s", count, want, s.snapshot())
			}
		}
	}
}

func (s *vmIntegrationPTY) close() {
	_ = s.master.Close()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (s *vmIntegrationPTY) snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.ReplaceAll(s.output.String(), "\r\n", "\n")
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

func createMetadataCopyFixture(t *testing.T, src string) {
	t.Helper()
	fileMtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.Local)
	mustWriteTestFile(t, filepath.Join(src, "script.sh"), "#!/bin/sh\necho hi\n")
	mustWriteTestFile(t, filepath.Join(src, "nested", "file.txt"), "nested\n")
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatalf("create empty dir: %v", err)
	}
	if err := os.Chmod(filepath.Join(src, "script.sh"), 0o755); err != nil {
		t.Fatalf("chmod script: %v", err)
	}
	if err := os.Symlink("script.sh", filepath.Join(src, "script-link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	for _, path := range []string{
		filepath.Join(src, "script.sh"),
		filepath.Join(src, "nested", "file.txt"),
		filepath.Join(src, "empty"),
	} {
		if err := os.Chtimes(path, fileMtime, fileMtime); err != nil {
			t.Fatalf("set mtime %s: %v", path, err)
		}
	}
}

func assertCopiedMetadataTree(t *testing.T, src, dst string) {
	t.Helper()
	script := filepath.Join(dst, "script.sh")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("stat copied script: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("copied script mode = %#o, want 0755", got)
	}
	if got, want := info.ModTime().Unix(), mustStat(t, filepath.Join(src, "script.sh")).ModTime().Unix(); got != want {
		t.Fatalf("copied script mtime = %d, want %d", got, want)
	}
	linkInfo, err := os.Lstat(filepath.Join(dst, "script-link"))
	if err != nil {
		t.Fatalf("lstat copied symlink: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("copied link mode = %s, want symlink", linkInfo.Mode())
	}
	if target, err := os.Readlink(filepath.Join(dst, "script-link")); err != nil || target != "script.sh" {
		t.Fatalf("copied symlink target = %q err=%v, want script.sh", target, err)
	}
	nested := filepath.Join(dst, "nested", "file.txt")
	if got, err := os.ReadFile(nested); err != nil || string(got) != "nested\n" {
		t.Fatalf("copied nested file = %q err=%v, want nested", string(got), err)
	}
	if got, want := mustStat(t, nested).ModTime().Unix(), mustStat(t, filepath.Join(src, "nested", "file.txt")).ModTime().Unix(); got != want {
		t.Fatalf("copied nested mtime = %d, want %d", got, want)
	}
	if info, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("copied empty dir info = %v err=%v, want directory", info, err)
	}
}

func waitForPath(t *testing.T, path string, timeout time.Duration, debug func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			msg := ""
			if debug != nil {
				msg = "\noutput:\n" + debug()
			}
			t.Fatalf("timed out waiting for %s%s", path, msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func createWeirdNameCopyFixture(t *testing.T, src string) []string {
	t.Helper()
	names := []string{
		"two words.txt",
		"quote'file",
		"dash - file",
		"-leading",
	}
	if runtime.GOOS != "windows" {
		names = append(names, "colon:file", "line\nbreak")
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("create weird source dir: %v", err)
	}
	for _, name := range names {
		payload := "payload:" + name + "\n"
		if err := os.WriteFile(filepath.Join(src, name), []byte(payload), 0o644); err != nil {
			t.Fatalf("write weird file %q: %v", name, err)
		}
	}
	return names
}

func assertWeirdNameCopyTree(t *testing.T, dst string, names []string) {
	t.Helper()
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("read weird copy dir %s: %v", dst, err)
	}
	gotNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		gotNames = append(gotNames, entry.Name())
	}
	sort.Strings(gotNames)
	wantNames := append([]string(nil), names...)
	sort.Strings(wantNames)
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("copied weird names = %#v, want %#v", gotNames, wantNames)
	}
	for _, name := range names {
		path := filepath.Join(dst, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read weird copied file %q: %v", name, err)
		}
		if want := "payload:" + name + "\n"; string(data) != want {
			t.Fatalf("weird copied file %q = %q, want %q", name, string(data), want)
		}
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info
}
