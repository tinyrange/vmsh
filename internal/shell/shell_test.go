package shell

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"j5.nz/cc/client"
)

func TestSplitShellFields(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{input: `@image alpine`, want: []string{"@image", "alpine"}},
		{input: `cd "two words"`, want: []string{"cd", "two words"}},
		{input: `@vm use 'work vm'`, want: []string{"@vm", "use", "work vm"}},
		{input: `cd a\ b`, want: []string{"cd", "a b"}},
	}
	for _, tt := range tests {
		got, err := splitShellFields(tt.input)
		if err != nil {
			t.Fatalf("splitShellFields(%q) error = %v", tt.input, err)
		}
		if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
			t.Fatalf("splitShellFields(%q) = %#v, want %#v", tt.input, got, tt.want)
		}
	}
}

func TestSplitShellFieldsErrors(t *testing.T) {
	for _, input := range []string{`"unterminated`, `abc\`} {
		if _, err := splitShellFields(input); err == nil {
			t.Fatalf("splitShellFields(%q) error = nil, want error", input)
		}
	}
}

func TestParseCD(t *testing.T) {
	target, ok, err := parseCD(`cd "hello world"`)
	if err != nil {
		t.Fatalf("parseCD() error = %v", err)
	}
	if !ok || target != "hello world" {
		t.Fatalf("parseCD() = %q, %v; want hello world, true", target, ok)
	}

	if _, ok, err := parseCD(`echo cd`); err != nil || ok {
		t.Fatalf("parseCD(non-cd) = _, %v, %v; want false, nil", ok, err)
	}

	if _, ok, err := parseCD(`cd one two`); !ok || err == nil {
		t.Fatalf("parseCD(extra args) = _, %v, %v; want true and error", ok, err)
	}
}

func TestPersistentHostShellPreservesState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host pty smoke test is unix-only")
	}
	dir := t.TempDir()
	session, err := startPersistentHostShell(dir, os.Environ(), 80, 24, "")
	if err != nil {
		t.Fatalf("startPersistentHostShell() error = %v", err)
	}
	defer session.close()

	var stdout, stderr bytes.Buffer
	if err := session.run("printf 'first-host\\n'", &stdout, &stderr); err != nil {
		t.Fatalf("run(first) error = %v; stderr=%q", err, stderr.String())
	}
	if got := normalizeTerminalOutput(stdout.String()); got != "first-host\n" {
		t.Fatalf("first output = %q, want first-host", got)
	}

	stdout.Reset()
	stderr.Reset()
	if err := session.run("alias hp='echo host-persist'", &stdout, &stderr); err != nil {
		t.Fatalf("run(alias) error = %v; stderr=%q", err, stderr.String())
	}
	if err := session.run("hp", &stdout, &stderr); err != nil {
		t.Fatalf("run(alias use) error = %v; stderr=%q", err, stderr.String())
	}
	if got := normalizeTerminalOutput(stdout.String()); !strings.Contains(got, "host-persist\n") {
		t.Fatalf("alias output = %q, want host-persist", got)
	}

	stdout.Reset()
	stderr.Reset()
	if err := session.run("cd /tmp", &stdout, &stderr); err != nil {
		t.Fatalf("run(cd) error = %v; stderr=%q", err, stderr.String())
	}
	if got := session.cwd(); got != "/tmp" {
		t.Fatalf("cwd after cd = %q, want /tmp", got)
	}
}

func TestChdirHostKeepsWarmShellInSync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host pty smoke test is unix-only")
	}
	dir := t.TempDir()
	next := filepath.Join(dir, "next")
	if err := os.Mkdir(next, 0o755); err != nil {
		t.Fatal(err)
	}
	session, err := startPersistentHostShell(dir, os.Environ(), 80, 24, "")
	if err != nil {
		t.Fatalf("startPersistentHostShell() error = %v", err)
	}
	defer session.close()

	sh := &shellState{hostCWD: dir, hostShell: session}
	if err := sh.chdirHost(next); err != nil {
		t.Fatalf("chdirHost() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := session.run("pwd", &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("pwd error = %v", err)
	}
	if got := strings.TrimSpace(normalizeTerminalOutput(stdout.String())); got != next {
		t.Fatalf("warm shell pwd = %q, want %q", got, next)
	}
}

func normalizeTerminalOutput(value string) string {
	return strings.ReplaceAll(value, "\r\n", "\n")
}

func TestParseAtLineTargetOptionsAndCommand(t *testing.T) {
	got, err := parseAtLine(`@ubuntu:24.04 --vm work --memory 2g --cpus=4 pytest -q --maxfail=1`)
	if err != nil {
		t.Fatalf("parseAtLine() error = %v", err)
	}
	if got.Target != "ubuntu:24.04" || got.Options.VMID != "work" || got.Options.MemoryMB != 2048 || got.Options.CPUs != 4 {
		t.Fatalf("parseAtLine() = %#v", got)
	}
	if got.Command != "pytest -q --maxfail=1" {
		t.Fatalf("command = %q", got.Command)
	}
}

func TestParseAtLineCurrentContextOptions(t *testing.T) {
	got, err := parseAtLine(`@ --vm work --cwd /src`)
	if err != nil {
		t.Fatalf("parseAtLine() error = %v", err)
	}
	if got.Target != "" || got.Options.VMID != "work" || got.Options.CWD != "/src" || got.Command != "" {
		t.Fatalf("parseAtLine() = %#v", got)
	}
}

func TestParseAtLineSudoOption(t *testing.T) {
	got, err := parseAtLine(`@ --sudo apt update`)
	if err != nil {
		t.Fatalf("parseAtLine() error = %v", err)
	}
	if !got.Options.Sudo || got.Command != "apt update" {
		t.Fatalf("parseAtLine() = %#v", got)
	}
}

func TestParseAtLineArchOption(t *testing.T) {
	got, err := parseAtLine(`@ubuntu --arch x86_64 uname -m`)
	if err != nil {
		t.Fatalf("parseAtLine() error = %v", err)
	}
	if got.Options.Arch != "amd64" || got.Command != "uname -m" {
		t.Fatalf("parseAtLine() = %#v, want arch amd64 command", got)
	}
}

func TestParseAtLineIsolatedOption(t *testing.T) {
	got, err := parseAtLine(`@alpine --isolated sh`)
	if err != nil {
		t.Fatalf("parseAtLine() error = %v", err)
	}
	if got.Options.Isolated == nil || !*got.Options.Isolated {
		t.Fatalf("isolated option = %#v, want true", got.Options.Isolated)
	}
	got, err = parseAtLine(`@alpine --shared sh`)
	if err != nil {
		t.Fatalf("parseAtLine(shared) error = %v", err)
	}
	if got.Options.Isolated == nil || *got.Options.Isolated {
		t.Fatalf("shared option = %#v, want false", got.Options.Isolated)
	}
}

func TestBareOCISelectsCurrentContextAndPreparesImage(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "default", Status: "stopped"}}
	sh := &shellState{api: api, context: defaultContext("default", "", false), hostCWD: t.TempDir()}
	if err := sh.eval(`@alpine`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("eval(@alpine) error = %v", err)
	}
	if sh.context.Mode != modeVM || sh.context.Image != "alpine" {
		t.Fatalf("context = %#v, want vm/alpine", sh.context)
	}
	if len(api.starts) != 0 {
		t.Fatalf("starts = %d, want 0", len(api.starts))
	}
}

func TestBareOCIPullsMissingImageWithoutBooting(t *testing.T) {
	api := &fakeVMSHAPI{
		status:      client.InstanceState{ID: "default", Status: "stopped"},
		missingImgs: map[string]bool{"alpine": true},
		pullEvents: []client.ProgressEvent{{
			Status:             "downloading",
			Artifact:           "alpine",
			Blob:               "rootfs",
			BytesDownloaded:    1024,
			BytesTotal:         2048,
			FilesDownloaded:    1,
			FilesTotal:         2,
			RateBytesPerSecond: 512,
			ETASeconds:         2,
		}},
	}
	var prompts []string
	sh := &shellState{
		api:     api,
		context: defaultContext("default", "", false),
		hostCWD: t.TempDir(),
		confirmPull: func(source string, stderr io.Writer) (bool, error) {
			prompts = append(prompts, source)
			return true, nil
		},
	}
	var stderr bytes.Buffer
	if err := sh.eval(`@alpine`, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatalf("eval(@alpine) error = %v", err)
	}
	if len(prompts) != 1 || prompts[0] != "docker.io/library/alpine:latest" {
		t.Fatalf("prompts = %#v, want normalized alpine source", prompts)
	}
	if len(api.pulls) != 1 || api.pulls[0].name != "alpine" {
		t.Fatalf("pulls = %#v, want alpine", api.pulls)
	}
	if len(api.starts) != 0 {
		t.Fatalf("starts = %d, want 0", len(api.starts))
	}
	if sh.context.Mode != modeVM || sh.context.Image != "alpine" {
		t.Fatalf("context = %#v, want vm/alpine", sh.context)
	}
	gotStatus := stderr.String()
	if !strings.Contains(gotStatus, "Pull alpine") || !strings.Contains(gotStatus, "downloading") || strings.Contains(gotStatus, "{") {
		t.Fatalf("pull status = %q, want human-readable pull progress", gotStatus)
	}
}

func TestBareOCISelectsIsolatedContext(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "default", Status: "stopped"}}
	sh := &shellState{api: api, context: defaultContext("default", "", false), hostCWD: t.TempDir(), contextCWD: map[string]string{}}
	if err := sh.eval(`@alpine --isolated`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("eval(@alpine --isolated) error = %v", err)
	}
	if sh.context.Mode != modeVM || sh.context.Image != "alpine" || !sh.context.Isolated {
		t.Fatalf("context = %#v, want isolated vm/alpine", sh.context)
	}
	if len(api.starts) != 0 {
		t.Fatalf("starts = %d, want 0", len(api.starts))
	}
	if !strings.Contains(sh.prompt(), "vm isolated:") {
		t.Fatalf("prompt = %q, want isolated signal", sh.prompt())
	}
}

func TestIsolatedPromptUsesGuestDefaultCWD(t *testing.T) {
	hostRoot := t.TempDir()
	hostCWD := filepath.Join(hostRoot, "host-work")
	if err := os.Mkdir(hostCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	sh := &shellState{
		context:    commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Isolated: true},
		hostCWD:    hostCWD,
		contextCWD: map[string]string{},
	}

	prompt := sh.prompt()
	if strings.Contains(prompt, "host-work") {
		t.Fatalf("prompt = %q, used host cwd in isolated context", prompt)
	}
	if !strings.Contains(prompt, "cc") {
		t.Fatalf("prompt = %q, want guest home leaf", prompt)
	}
}

func TestPromptColorCodesWorkingDirectoryContext(t *testing.T) {
	root := t.TempDir()
	hostCWD := filepath.Join(root, "project")
	if err := os.Mkdir(hostCWD, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		ctx   commandContext
		leaf  string
		color string
	}{
		{
			name:  "host",
			ctx:   commandContext{Mode: modeHost},
			leaf:  "project",
			color: colorCyan,
		},
		{
			name:  "shared host directory",
			ctx:   commandContext{Mode: modeVM, Image: "alpine"},
			leaf:  "project",
			color: colorCyan,
		},
		{
			name:  "shared guest directory",
			ctx:   commandContext{Mode: modeVM, Image: "alpine", CWD: "/work"},
			leaf:  "work",
			color: colorYellow,
		},
		{
			name:  "isolated guest",
			ctx:   commandContext{Mode: modeVM, Image: "alpine", Isolated: true, CWD: "/work"},
			leaf:  "work",
			color: colorMagenta,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sh := &shellState{context: tt.ctx, hostCWD: hostCWD}
			prompt := sh.prompt()
			if !strings.Contains(prompt, tt.color+tt.leaf+colorReset) {
				t.Fatalf("prompt = %q, want %s-colored cwd %q", prompt, tt.name, tt.leaf)
			}
		})
	}
}

func TestBareOCIPullCanBeCancelled(t *testing.T) {
	api := &fakeVMSHAPI{
		status:      client.InstanceState{ID: "default", Status: "stopped"},
		missingImgs: map[string]bool{"ubuntu": true},
	}
	sh := &shellState{
		api:     api,
		context: defaultContext("default", "", false),
		hostCWD: t.TempDir(),
		confirmPull: func(source string, stderr io.Writer) (bool, error) {
			if source != "docker.io/library/ubuntu:latest" {
				t.Fatalf("source = %q, want normalized ubuntu source", source)
			}
			return false, nil
		},
	}
	err := sh.eval(`@ubuntu`, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "image pull cancelled") {
		t.Fatalf("eval(@ubuntu) error = %v, want cancelled pull", err)
	}
	if len(api.pulls) != 0 {
		t.Fatalf("pulls = %#v, want none after cancellation", api.pulls)
	}
}

func TestDisplayPullSourceNormalizesCommonOCIRefs(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		{source: "ubuntu", want: "docker.io/library/ubuntu:latest"},
		{source: "ubuntu:24.04", want: "docker.io/library/ubuntu:24.04"},
		{source: "j5.nz/tool", want: "j5.nz/tool:latest"},
		{source: "ghcr.io/acme/tool:v1", want: "ghcr.io/acme/tool:v1"},
	}
	for _, tt := range tests {
		if got := displayPullSource(tt.source); got != tt.want {
			t.Fatalf("displayPullSource(%q) = %q, want %q", tt.source, got, tt.want)
		}
	}
}

func TestAliasExpandsBeforeCommandDispatch(t *testing.T) {
	sh := &shellState{hostCWD: t.TempDir()}
	if err := sh.eval(`@alias say=@host echo alias-ok`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("set alias error = %v", err)
	}
	var stdout bytes.Buffer
	if err := sh.eval(`say`, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("eval(alias) error = %v", err)
	}
	if strings.TrimSpace(stdout.String()) != "alias-ok" {
		t.Fatalf("stdout = %q, want alias-ok", stdout.String())
	}
}

func TestAliasCanPointToHostClearCommand(t *testing.T) {
	sh := &shellState{}
	if err := sh.eval(`@alias clear=@host clear`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("set alias error = %v", err)
	}
	got, err := sh.expandAliasLine("clear")
	if err != nil {
		t.Fatalf("expandAliasLine() error = %v", err)
	}
	if got != "@host clear" {
		t.Fatalf("expanded clear = %q, want @host clear", got)
	}
}

func TestAliasPreservesCommandArguments(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeHost, VMID: "work", Network: true}, hostCWD: t.TempDir()}
	if err := sh.eval(`@alias u=@ubuntu echo`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("set alias error = %v", err)
	}
	if err := sh.eval(`u hello --flag`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("eval(alias) error = %v", err)
	}
	if len(api.streams) != 1 || api.streams[0].req.Image != "ubuntu" {
		t.Fatalf("streams = %#v, want ubuntu run", api.streams)
	}
	if got := strings.Join(api.streams[0].req.Command, " "); !strings.Contains(got, "echo hello --flag") {
		t.Fatalf("command = %#v, want appended alias arguments", api.streams[0].req.Command)
	}
}

func TestGuestCommandUsesArchitectureSpecificImage(t *testing.T) {
	api := &fakeVMSHAPI{
		status:      client.InstanceState{ID: "work", Status: "stopped"},
		missingImgs: map[string]bool{"ubuntu@amd64": true},
	}
	sh := &shellState{
		api:     api,
		context: commandContext{Mode: modeHost, VMID: "work", Network: true},
		hostCWD: t.TempDir(),
		confirmPull: func(source string, stderr io.Writer) (bool, error) {
			if source != "docker.io/library/ubuntu:latest (amd64)" {
				t.Fatalf("pull prompt source = %q", source)
			}
			return true, nil
		},
	}
	if err := sh.eval(`@ubuntu --arch amd64 uname -m`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("eval() error = %v", err)
	}
	if len(api.pulls) != 1 || api.pulls[0].name != "ubuntu@amd64" || api.pulls[0].source != "ubuntu" || api.pulls[0].arch != "amd64" {
		t.Fatalf("pulls = %#v, want ubuntu@amd64 from ubuntu arch amd64", api.pulls)
	}
	if len(api.starts) != 1 || api.starts[0].req.Image != "ubuntu@amd64" {
		t.Fatalf("starts = %#v, want boot image ubuntu@amd64", api.starts)
	}
	if len(api.streams) != 1 || api.streams[0].req.Image != "ubuntu@amd64" {
		t.Fatalf("streams = %#v, want run image ubuntu@amd64", api.streams)
	}
}

func TestAliasListsAndDeletesAliases(t *testing.T) {
	sh := &shellState{}
	if err := sh.eval(`@alias b=@host echo b`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("set b error = %v", err)
	}
	if err := sh.eval(`@alias a=@host echo a`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("set a error = %v", err)
	}
	var out bytes.Buffer
	if err := sh.eval(`@alias`, &out, &bytes.Buffer{}); err != nil {
		t.Fatalf("list aliases error = %v", err)
	}
	if out.String() != "a=@host echo a\nb=@host echo b\n" {
		t.Fatalf("aliases = %q", out.String())
	}
	if err := sh.eval(`@alias -d a`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("delete alias error = %v", err)
	}
	out.Reset()
	if err := sh.eval(`@alias`, &out, &bytes.Buffer{}); err != nil {
		t.Fatalf("list aliases error = %v", err)
	}
	if out.String() != "b=@host echo b\n" {
		t.Fatalf("aliases after delete = %q", out.String())
	}
}

func TestAliasExpansionRejectsRecursiveAliases(t *testing.T) {
	sh := &shellState{}
	if err := sh.eval(`@alias a=b`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("set a error = %v", err)
	}
	if err := sh.eval(`@alias b=a`, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("set b error = %v", err)
	}
	err := sh.eval(`a`, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "alias expansion exceeded") {
		t.Fatalf("recursive alias error = %v, want expansion limit", err)
	}
}

func TestVMSHRCAcceptsCommentsAndAliases(t *testing.T) {
	sh := &shellState{}
	rc := strings.NewReader(`
# personal vmsh aliases
sudo=@sudo
@alias clear=@host clear
`)
	if err := sh.evalVMSHRCLines(".vmshrc", rc); err != nil {
		t.Fatalf("evalVMSHRCLines() error = %v", err)
	}
	if got := sh.aliases["sudo"]; got != "@sudo" {
		t.Fatalf("sudo alias = %q, want @sudo", got)
	}
	if got := sh.aliases["clear"]; got != "@host clear" {
		t.Fatalf("clear alias = %q, want @host clear", got)
	}
}

func TestVMSHRCRejectsCommands(t *testing.T) {
	sh := &shellState{}
	err := sh.evalVMSHRCLines(".vmshrc", strings.NewReader("echo nope\n"))
	if err == nil || !strings.Contains(err.Error(), ".vmshrc:1") || !strings.Contains(err.Error(), "only supports aliases") {
		t.Fatalf("evalVMSHRCLines() error = %v, want rc subset error", err)
	}
}

func TestLoadVMSHRCIgnoresMissingFile(t *testing.T) {
	sh := &shellState{}
	if err := sh.loadVMSHRC(filepath.Join(t.TempDir(), ".vmshrc")); err != nil {
		t.Fatalf("loadVMSHRC(missing) error = %v", err)
	}
}

func TestStartVMReportsBootProgressToStderr(t *testing.T) {
	api := &fakeVMSHAPI{
		status: client.InstanceState{ID: "work", Status: "stopped"},
		bootEvents: []client.BootEvent{
			{Kind: "status", Message: "preparing kernel"},
			{Kind: "status", Message: "starting VM"},
			{Kind: "ready", State: client.InstanceState{ID: "work", Status: "running", Image: "alpine"}},
		},
	}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true}, hostCWD: t.TempDir()}
	var stderr bytes.Buffer
	if err := sh.eval(`@start --vm work`, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatalf("eval(@start) error = %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "Boot: preparing kernel") || !strings.Contains(got, "Boot: starting VM") || !strings.Contains(got, "Boot: ready alpine") {
		t.Fatalf("boot status = %q, want detailed boot progress", got)
	}
}

func TestBootStatusIgnoresSerialForTTYSpinner(t *testing.T) {
	var stderr bytes.Buffer
	status := &bootStatus{terminalHoldStatus: &terminalHoldStatus{w: &stderr, tty: true}}

	status.Update(client.BootEvent{Kind: "status", Message: "starting VM"})
	status.Update(client.BootEvent{Kind: "serial", Data: "["})

	status.mu.Lock()
	got := status.message
	status.mu.Unlock()
	if got != "Boot: starting VM" {
		t.Fatalf("spinner message = %q, want Boot: starting VM", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("serial wrote to tty spinner output: %q", stderr.String())
	}
}

func TestBootStatusWritesSerialRawForNonTTY(t *testing.T) {
	var stderr bytes.Buffer
	status := newBootStatus(&stderr)

	status.Update(client.BootEvent{Kind: "serial", Data: "ab"})
	status.Update(client.BootEvent{Kind: "serial", Data: "c\n"})

	if got := stderr.String(); got != "abc\n" {
		t.Fatalf("serial output = %q, want raw serial bytes", got)
	}
}

func TestRestartVMRequiresConfirmation(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{
		api:     api,
		context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true, MemoryMB: 512},
		hostCWD: t.TempDir(),
		confirmVMRestart: func(id string, stderr io.Writer) (bool, error) {
			if id != "work" {
				t.Fatalf("confirm id = %q, want work", id)
			}
			return true, nil
		},
	}

	if err := sh.evalAt("@restart", io.Discard, io.Discard); err != nil {
		t.Fatalf("evalAt(@restart) error = %v", err)
	}
	if len(api.shutdowns) != 1 || api.shutdowns[0] != "work" {
		t.Fatalf("shutdowns = %#v, want work", api.shutdowns)
	}
	if len(api.starts) != 1 || api.starts[0].id != "work" || api.starts[0].req.Image != "alpine" || api.starts[0].req.MemoryMB != 512 {
		t.Fatalf("starts = %#v, want work alpine memory 512", api.starts)
	}
	if !sh.vmRunning["work"] {
		t.Fatalf("vmRunning[work] = false, want true")
	}
}

func TestRestartVMCancelled(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{
		api:     api,
		context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine"},
		hostCWD: t.TempDir(),
		confirmVMRestart: func(string, io.Writer) (bool, error) {
			return false, nil
		},
	}

	err := sh.evalAt("@restart", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "restart cancelled") {
		t.Fatalf("evalAt(@restart cancelled) error = %v, want cancellation", err)
	}
	if len(api.shutdowns) != 0 || len(api.starts) != 0 {
		t.Fatalf("shutdowns=%#v starts=%#v, want none", api.shutdowns, api.starts)
	}
}

func TestRestartVMPreservesRunningStateWhenHostSelected(t *testing.T) {
	api := &fakeVMSHAPI{
		status: client.InstanceState{
			ID:          "two",
			Status:      "running",
			Image:       "alpine",
			MemoryMB:    768,
			CPUs:        3,
			NestedVirt:  true,
			NetworkIPv4: "10.42.0.3",
		},
	}
	sh := &shellState{
		api:     api,
		context: commandContext{Mode: modeHost, VMID: "default"},
		hostCWD: t.TempDir(),
		confirmVMRestart: func(id string, stderr io.Writer) (bool, error) {
			return id == "two", nil
		},
	}

	if err := sh.evalAt("@restart --vm two", io.Discard, io.Discard); err != nil {
		t.Fatalf("evalAt(@restart --vm two) error = %v", err)
	}
	if len(api.shutdowns) != 1 || api.shutdowns[0] != "two" {
		t.Fatalf("shutdowns = %#v, want two", api.shutdowns)
	}
	if len(api.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(api.starts))
	}
	req := api.starts[0].req
	if api.starts[0].id != "two" || req.Image != "alpine" || req.MemoryMB != 768 || req.CPUs != 3 || !req.NestedVirt || req.Network == nil || !req.Network.Enabled {
		t.Fatalf("restart start = %#v, want preserved running state", api.starts[0])
	}
}

func TestRestartVMRejectsCommand(t *testing.T) {
	sh := &shellState{api: &fakeVMSHAPI{}, context: commandContext{VMID: "work"}, hostCWD: t.TempDir()}
	err := sh.evalAt("@restart echo nope", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "usage: @restart") {
		t.Fatalf("evalAt(@restart command) error = %v, want usage", err)
	}
}

func TestSplitPipelineLine(t *testing.T) {
	got, ok, err := splitPipelineLine(`printf 'a|b' | @alpine grep a || true`)
	if err != nil {
		t.Fatalf("splitPipelineLine() error = %v", err)
	}
	if !ok {
		t.Fatal("splitPipelineLine() ok=false, want true")
	}
	want := []string{`printf 'a|b'`, `@alpine grep a || true`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("segments = %#v, want %#v", got, want)
	}

	if _, ok, err := splitPipelineLine("false || true"); err != nil || ok {
		t.Fatalf("splitPipelineLine(||) = ok %v err %v, want no pipeline", ok, err)
	}
}

func TestHostPipelineStreamsData(t *testing.T) {
	sh := &shellState{
		api:     &fakeVMSHAPI{},
		context: commandContext{Mode: modeHost, VMID: "default"},
		hostCWD: t.TempDir(),
		env:     map[string]string{},
		aliases: map[string]string{},
	}
	var stdout bytes.Buffer
	line := "printf 'hello\\nworld\\n' | grep hello"
	want := "hello\n"
	if runtime.GOOS == "windows" {
		line = "echo hello| findstr hello"
		want = "hello\r\n"
	}
	if err := sh.eval(line, &stdout, io.Discard); err != nil {
		t.Fatalf("eval(host pipeline) error = %v", err)
	}
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want hello", got)
	}
}

func TestGuestPipelineStreamsBetweenStages(t *testing.T) {
	api := &fakeVMSHAPI{
		status: client.InstanceState{ID: "work", Status: "running"},
		streamEventsByCommand: map[string][]client.ExecEvent{
			"printf hello": {{Kind: "stdout", Data: []byte("hello")}, {Kind: "exit", ExitCode: 0}},
			"cat":          {{Kind: "exit", ExitCode: 0}},
		},
	}
	sh := &shellState{
		api:        api,
		context:    commandContext{Mode: modeVM, VMID: "work", Image: "alpine"},
		hostCWD:    t.TempDir(),
		imageCache: map[string]bool{"alpine": true},
		vmRunning:  map[string]bool{"work": true},
		env:        map[string]string{},
		aliases:    map[string]string{},
	}

	if err := sh.eval("printf hello | cat", io.Discard, io.Discard); err != nil {
		t.Fatalf("eval(guest pipeline) error = %v", err)
	}
	input := api.inputForCommand("cat")
	if input != "hello" {
		t.Fatalf("cat stdin = %q, want hello", input)
	}
	if len(api.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(api.streams))
	}
	for _, run := range api.streams {
		if run.req.TTY {
			t.Fatalf("pipeline stage %#v used TTY", run.req.Command)
		}
	}
}

func TestScriptSendsLinesThroughCurrentContext(t *testing.T) {
	dir := t.TempDir()
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: defaultContext("default", "", false), hostCWD: dir}
	script := strings.NewReader(`
# ignored
@alpine --vm work --memory 512
echo hello --flag
@host
`)
	if err := sh.runScript(script, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if len(api.streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(api.streams))
	}
	run := api.streams[0]
	if run.id != "work" || run.req.Image != "alpine" || run.req.MemoryMB != 512 {
		t.Fatalf("run = %#v", run)
	}
	if run.req.NestedVirt {
		t.Fatalf("nested virtualization = true, want default false without capability")
	}
	if run.req.Network == nil || !run.req.Network.Enabled || !run.req.Network.AllowInternet {
		t.Fatalf("network = %#v, want enabled internet", run.req.Network)
	}
	wantEnv := []string{"HOME=/home/cc", "USER=cc", "LOGNAME=cc"}
	if run.req.User != defaultGuestUser {
		t.Fatalf("user = %q, want %q", run.req.User, defaultGuestUser)
	}
	if len(run.req.Command) != 3 || run.req.Command[0] != "sh" || run.req.Command[1] != "-lc" || !strings.HasSuffix(run.req.Command[2], "echo hello --flag") {
		t.Fatalf("command = %#v", run.req.Command)
	}
	for _, want := range wantEnv {
		if !envContains(run.req.Env, want) {
			t.Fatalf("env = %#v, missing %q", run.req.Env, want)
		}
	}
	if len(run.req.Shares) != 1 {
		t.Fatalf("shares = %#v", run.req.Shares)
	}
	wantHostRoot, wantWorkDir, err := guestHostPaths(dir)
	if err != nil {
		t.Fatalf("guestHostPaths() error = %v", err)
	}
	if run.req.Shares[0].Source != wantHostRoot || run.req.Shares[0].Mount != guestHostMount {
		t.Fatalf("host share = %#v, want root at /host", run.req.Shares[0])
	}
	if !run.req.Shares[0].MapOwner || run.req.Shares[0].OwnerUID != defaultGuestUID || run.req.Shares[0].OwnerGID != defaultGuestGID {
		t.Fatalf("host share owner = %#v, want mapped default guest user", run.req.Shares[0])
	}
	if run.req.WorkDir != wantWorkDir {
		t.Fatalf("workdir = %q, want %q", run.req.WorkDir, wantWorkDir)
	}
	if sh.context.Mode != modeHost {
		t.Fatalf("mode = %q, want host", sh.context.Mode)
	}
}

func TestGuestRunsAsRootWithSudoOption(t *testing.T) {
	dir := t.TempDir()
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true}, hostCWD: dir}
	if err := sh.runScript(strings.NewReader("@ --sudo id -u\n@sudo id -u\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if len(api.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(api.streams))
	}
	for _, run := range api.streams {
		if run.req.User != "root" {
			t.Fatalf("user = %q, want root", run.req.User)
		}
		for _, want := range []string{"HOME=/root", "USER=root", "LOGNAME=root"} {
			if !envContains(run.req.Env, want) {
				t.Fatalf("env = %#v, missing %q", run.req.Env, want)
			}
		}
	}
}

func TestAtImageCommandUsesDefaultGuestUser(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "default", Status: "running"}}
	sh := &shellState{api: api, context: defaultContext("default", "", false), hostCWD: t.TempDir()}
	if err := sh.eval("@alpine whoami", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("eval(@alpine whoami) error = %v", err)
	}
	if len(api.streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(api.streams))
	}
	if api.streams[0].req.User != defaultGuestUser {
		t.Fatalf("user = %q, want %q", api.streams[0].req.User, defaultGuestUser)
	}
}

func TestAtSudoUsesHostSudoWhenHostSelected(t *testing.T) {
	ctx, command := sudoCommandContext(commandContext{Mode: modeHost, VMID: "work", Network: true}, "whoami")
	if ctx.Mode != modeHost || command != "sudo whoami" {
		t.Fatalf("sudoCommandContext(host) = %#v, %q; want host sudo command", ctx, command)
	}
}

func TestAtSudoUsesGuestRootWhenVMSelected(t *testing.T) {
	ctx, command := sudoCommandContext(commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true}, "whoami")
	if ctx.Mode != modeVM || ctx.User != "root" || command != "whoami" {
		t.Fatalf("sudoCommandContext(vm) = %#v, %q; want guest root command", ctx, command)
	}
}

func TestPersistentGuestCommandAllowedDoesNotDependOnHostSupport(t *testing.T) {
	if !persistentGuestCommandAllowed("ls") {
		t.Fatal("persistentGuestCommandAllowed(ls) = false, want true")
	}
	if persistentGuestCommandAllowed("cat") {
		t.Fatal("persistentGuestCommandAllowed(cat) = true, want false without arguments")
	}
}

func TestGuestRunRequestsUseStreamingPath(t *testing.T) {
	dir := t.TempDir()
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine"}, hostCWD: dir}
	if err := sh.runScript(strings.NewReader("ls\nuname -a\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if len(api.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(api.streams))
	}
	for _, run := range api.streams {
		if run.id != "work" || run.req.Image != "alpine" {
			t.Fatalf("stream = %#v", run)
		}
	}
}

func TestIsolatedGuestRunDoesNotMountHost(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Isolated: true}, hostCWD: t.TempDir(), imageCache: map[string]bool{}}

	if err := sh.runGuest(sh.context, "pwd", io.Discard, io.Discard); err != nil {
		t.Fatalf("runGuest() error = %v", err)
	}
	if len(api.streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(api.streams))
	}
	req := api.streams[0].req
	if len(req.Shares) != 0 {
		t.Fatalf("shares = %#v, want none", req.Shares)
	}
	if req.WorkDir != "/home/cc" {
		t.Fatalf("workdir = %q, want guest home", req.WorkDir)
	}
}

func TestSharedGuestRunStillMountsHost(t *testing.T) {
	dir := t.TempDir()
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine"}, hostCWD: dir, imageCache: map[string]bool{}}

	if err := sh.runGuest(sh.context, "pwd", io.Discard, io.Discard); err != nil {
		t.Fatalf("runGuest() error = %v", err)
	}
	req := api.streams[0].req
	if len(req.Shares) != 1 || req.Shares[0].Mount != guestHostMount {
		t.Fatalf("shares = %#v, want /host mount", req.Shares)
	}
	_, wantCWD, err := guestHostPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if req.WorkDir != wantCWD {
		t.Fatalf("workdir = %q, want %q", req.WorkDir, wantCWD)
	}
}

func TestContextCWDIsRememberedPerIsolationMode(t *testing.T) {
	sh := &shellState{
		context:    commandContext{Mode: modeVM, VMID: "work", Image: "alpine", CWD: "/shared"},
		hostCWD:    t.TempDir(),
		contextCWD: map[string]string{},
	}
	sh.activateContext(commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Isolated: true})
	if sh.context.CWD != "" {
		t.Fatalf("isolated cwd = %q, want empty/new context", sh.context.CWD)
	}
	sh.context.CWD = "/iso"
	sh.activateContext(commandContext{Mode: modeVM, VMID: "work", Image: "alpine"})
	if sh.context.CWD != "/shared" {
		t.Fatalf("shared cwd = %q, want /shared", sh.context.CWD)
	}
	sh.activateContext(commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Isolated: true})
	if sh.context.CWD != "/iso" {
		t.Fatalf("isolated restored cwd = %q, want /iso", sh.context.CWD)
	}
}

func TestGuestTTYLSAliasUsesBusyBoxWidthFlag(t *testing.T) {
	command := strings.Join(guestCommand("ls", true), " ")
	if strings.Contains(command, "--width=") {
		t.Fatalf("guest tty command contains GNU-only --width flag: %s", command)
	}
	if !strings.Contains(command, "-w ${COLUMNS:-80}") {
		t.Fatalf("guest tty command = %s, want BusyBox-compatible -w width flag", command)
	}
}

func TestPersistentHostShellPreludeAllowsBashExtglobFunctions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent host shell uses Windows command shell")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	t.Setenv("SHELL", bash)
	prelude := bashHostShellOptionsPrelude() + "\n" + strings.Join([]string{
		"__vmsh_extglob_probe() {",
		"  case \"$1\" in",
		"    --!(no-*)dir*) return 0 ;;",
		"    *) return 1 ;;",
		"  esac",
		"}",
	}, "\n") + "\n"
	session, err := startPersistentHostShell(t.TempDir(), os.Environ(), 80, 24, prelude)
	if err != nil {
		t.Fatalf("startPersistentHostShell() error = %v", err)
	}
	defer session.close()
	var stdout bytes.Buffer
	if err := session.run("__vmsh_extglob_probe --color-dir && echo extglob-ok", &stdout, io.Discard); err != nil {
		t.Fatalf("run extglob probe error = %v; stdout=%q", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "extglob-ok") {
		t.Fatalf("stdout = %q, want extglob-ok", stdout.String())
	}
}

func TestNoNetworkOptionDisablesGuestNetwork(t *testing.T) {
	dir := t.TempDir()
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeHost, VMID: "work", Network: true}, hostCWD: dir}
	if err := sh.runScript(strings.NewReader("@alpine --no-network echo hi\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if len(api.streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(api.streams))
	}
	if api.streams[0].req.Network != nil {
		t.Fatalf("network = %#v, want nil", api.streams[0].req.Network)
	}
}

func TestGuestBootUsesContextNetwork(t *testing.T) {
	dir := t.TempDir()
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "stopped"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true}, hostCWD: dir}
	if err := sh.runScript(strings.NewReader("echo hi\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if len(api.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(api.starts))
	}
	if api.starts[0].req.Network == nil || !api.starts[0].req.Network.Enabled || !api.starts[0].req.Network.AllowInternet {
		t.Fatalf("start network = %#v, want enabled internet", api.starts[0].req.Network)
	}
	if api.starts[0].req.Image != "alpine" {
		t.Fatalf("start image = %q, want alpine", api.starts[0].req.Image)
	}
	if api.starts[0].req.TimeoutSeconds != vmshBootTimeoutSeconds() {
		t.Fatalf("start timeout = %.1f, want %.1f", api.starts[0].req.TimeoutSeconds, vmshBootTimeoutSeconds())
	}
}

func TestVMSHBootTimeoutReadsEnvironment(t *testing.T) {
	t.Setenv("VMSH_VM_BOOT_TIMEOUT", "123.5")
	if got := vmshBootTimeoutSeconds(); got != 123.5 {
		t.Fatalf("vmshBootTimeoutSeconds() = %.1f, want 123.5", got)
	}
	t.Setenv("VMSH_VM_BOOT_TIMEOUT", "")
	t.Setenv("CCX3_VM_BOOT_TIMEOUT", "98")
	if got := vmshBootTimeoutSeconds(); got != 98 {
		t.Fatalf("vmshBootTimeoutSeconds() fallback = %.1f, want 98", got)
	}
	t.Setenv("CCX3_VM_BOOT_TIMEOUT", "bad")
	if got := vmshBootTimeoutSeconds(); got != defaultVMSHBootTimeoutSeconds {
		t.Fatalf("vmshBootTimeoutSeconds() invalid = %.1f, want %.1f", got, float64(defaultVMSHBootTimeoutSeconds))
	}
}

func TestGuestBootAndRunUseNestedVirtualizationContext(t *testing.T) {
	dir := t.TempDir()
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "stopped"}}
	sh := &shellState{
		api:     api,
		context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true, NestedVirt: true},
		hostCWD: dir,
	}
	if err := sh.runScript(strings.NewReader("echo hi\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if len(api.starts) != 1 || !api.starts[0].req.NestedVirt {
		t.Fatalf("starts = %#v, want nested virtualization enabled", api.starts)
	}
	if len(api.streams) != 1 || !api.streams[0].req.NestedVirt {
		t.Fatalf("streams = %#v, want nested virtualization enabled", api.streams)
	}
}

func TestStreamGuestRunWritesEventsAndExit(t *testing.T) {
	api := &fakeVMSHAPI{
		streamEvents: []client.ExecEvent{
			{Kind: "stdout", Data: []byte("Linux\n")},
			{Kind: "stderr", Output: "warn\n"},
			{Kind: "exit", ExitCode: 0},
		},
	}
	sh := &shellState{api: api}
	var stdout, stderr bytes.Buffer
	err := sh.streamGuestRun("work", client.RunRequest{Image: "ubuntu", Command: []string{"uname", "-a"}}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("streamGuestRun() error = %v", err)
	}
	if stdout.String() != "Linux\n" || stderr.String() != "warn\n" || sh.lastCode != 0 {
		t.Fatalf("stdout=%q stderr=%q code=%d", stdout.String(), stderr.String(), sh.lastCode)
	}
}

func TestStreamGuestRunRecordsNonzeroExitWithoutLog(t *testing.T) {
	api := &fakeVMSHAPI{
		streamEvents: []client.ExecEvent{
			{Kind: "exit", ExitCode: 42},
		},
	}
	sh := &shellState{api: api}
	var stdout, stderr bytes.Buffer
	err := sh.streamGuestRun("work", client.RunRequest{Image: "ubuntu", Command: []string{"false"}}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("streamGuestRun() error = %v", err)
	}
	if stdout.String() != "" || stderr.String() != "" || sh.lastCode != 42 {
		t.Fatalf("stdout=%q stderr=%q code=%d", stdout.String(), stderr.String(), sh.lastCode)
	}
}

func TestScriptStopsOnErrors(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "default", Status: "running"}}
	sh := &shellState{api: api, context: defaultContext("default", "", false), hostCWD: t.TempDir()}
	err := sh.runScript(strings.NewReader("@ --bogus\n@alpine echo nope\n"), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown vmsh option") {
		t.Fatalf("runScript() error = %v, want unknown option", err)
	}
	if len(api.streams) != 0 {
		t.Fatalf("streams = %d, want 0", len(api.streams))
	}
}

func TestLoopRequiresInteractiveTerminal(t *testing.T) {
	sh := &shellState{context: defaultContext("default", "", false), hostCWD: t.TempDir()}
	err := sh.loop(strings.NewReader("echo nope\n"), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "requires an interactive terminal") {
		t.Fatalf("loop() error = %v, want interactive terminal error", err)
	}
}

func TestShouldSaveHistory(t *testing.T) {
	for _, line := range []string{"ls", "  @ubuntu echo hi  "} {
		if !shouldSaveHistory(line) {
			t.Fatalf("shouldSaveHistory(%q) = false, want true", line)
		}
	}
	for _, line := range []string{"", "   ", "# comment", "  # comment"} {
		if shouldSaveHistory(line) {
			t.Fatalf("shouldSaveHistory(%q) = true, want false", line)
		}
	}
}

func TestTerminalEnvForwardsColorAndSize(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("LS_COLORS", "di=34")
	got := terminalEnv(120, 40)
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LS_COLORS=di=34",
		"CLICOLOR=1",
		"COLUMNS=120",
		"LINES=40",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("terminalEnv() = %#v, missing %q", got, want)
		}
	}
}

func TestTerminalEnvDefaultsTERM(t *testing.T) {
	t.Setenv("TERM", "")
	got := terminalEnv(0, 0)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "TERM=") {
		t.Fatalf("terminalEnv() = %#v, missing TERM", got)
	}
}

func TestExportPersistsIntoGuestCommands(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true}, hostCWD: t.TempDir()}
	if err := sh.runScript(strings.NewReader("export FOO=bar\nprintenv FOO\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if len(api.streams) != 1 || !envContains(api.streams[0].req.Env, "FOO=bar") {
		t.Fatalf("stream env = %#v, want FOO=bar", api.streams)
	}
}

func TestVMCDUsesGuestCWD(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true}, hostCWD: t.TempDir()}
	if err := sh.runScript(strings.NewReader("cd /\npwd\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if len(api.streams) != 1 || api.streams[0].req.WorkDir != "/" {
		t.Fatalf("workdir = %#v, want /", api.streams)
	}
}

func TestVMCDUsesGuestHome(t *testing.T) {
	tests := []struct {
		name  string
		ctx   commandContext
		input string
		want  string
	}{
		{
			name:  "ubuntu default user",
			ctx:   commandContext{Mode: modeVM, VMID: "work", Image: "ubuntu", Network: true},
			input: "cd\npwd\n",
			want:  "/home/ubuntu",
		},
		{
			name:  "ubuntu tilde child",
			ctx:   commandContext{Mode: modeVM, VMID: "work", Image: "ubuntu", Network: true},
			input: "cd ~/src\npwd\n",
			want:  "/home/ubuntu/src",
		},
		{
			name:  "created default user",
			ctx:   commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true},
			input: "cd ~\npwd\n",
			want:  "/home/cc",
		},
		{
			name:  "root user",
			ctx:   commandContext{Mode: modeVM, VMID: "work", Image: "ubuntu", User: "root", Network: true},
			input: "cd\npwd\n",
			want:  "/root",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
			sh := &shellState{api: api, context: tt.ctx, hostCWD: t.TempDir()}
			if err := sh.runScript(strings.NewReader(tt.input), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatalf("runScript() error = %v", err)
			}
			if len(api.streams) != 1 || api.streams[0].req.WorkDir != tt.want {
				t.Fatalf("workdir = %#v, want %s", api.streams, tt.want)
			}
		})
	}
}

func TestPrintVMsIsHumanReadableWhenEmpty(t *testing.T) {
	sh := &shellState{api: &fakeVMSHAPI{}}
	var out bytes.Buffer
	if err := sh.printVMs(&out); err != nil {
		t.Fatalf("printVMs() error = %v", err)
	}
	if strings.TrimSpace(out.String()) != "No VMs" {
		t.Fatalf("printVMs() = %q, want No VMs", out.String())
	}
}

func TestBackgroundJobIsTracked(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Network: true}, hostCWD: t.TempDir()}
	var out bytes.Buffer
	if err := sh.eval("echo hi &", &out, &bytes.Buffer{}); err != nil {
		t.Fatalf("eval() error = %v", err)
	}
	if !strings.Contains(out.String(), "[1] running echo hi") {
		t.Fatalf("background output = %q", out.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sh.jobsMu.Lock()
		done := len(sh.jobs) == 1 && sh.jobs[0].Done
		sh.jobsMu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job did not complete: %#v", sh.jobs)
}

func TestCompleterSuggestsAtCommandsAndPaths(t *testing.T) {
	t.Setenv("PATH", "")

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "alpha dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, "images", "ubuntu"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh := &shellState{hostCWD: dir, rootCache: cache}
	completer := newVMSHCompleter(sh)
	got, _ := completer.Do([]rune("@st"), 3)
	if !completionContains(got, "atus") || !completionContains(got, "art") || !completionContains(got, "op") {
		t.Fatalf("@ completions = %#v", got)
	}
	got, _ = completer.Do([]rune("@rm"), 3)
	if !completionContains(got, "i") {
		t.Fatalf("@rmi completion = %#v", got)
	}
	got, _ = completer.Do([]rune("@tm"), 3)
	if !completionContains(got, "ux") {
		t.Fatalf("@tmux completion = %#v", got)
	}
	got, _ = completer.Do([]rune("@ub"), 3)
	if !completionContains(got, "untu") {
		t.Fatalf("image completions = %#v", got)
	}
	got, _ = completer.Do([]rune("@rmi ub"), 7)
	if !completionContains(got, "untu") {
		t.Fatalf("@rmi image completions = %#v", got)
	}
	got, _ = completer.Do([]rune("@rmi "), 5)
	if !completionContains(got, "ubuntu") {
		t.Fatalf("@rmi empty image completions = %#v", got)
	}
	got, _ = completer.Do([]rune("@ubuntu --s"), 11)
	if !completionContains(got, "udo") {
		t.Fatalf("option completions = %#v", got)
	}
	got, _ = completer.Do([]rune("ec"), 2)
	if !completionContains(got, "ho") {
		t.Fatalf("command completions = %#v", got)
	}
	got, _ = completer.Do([]rune("@ubuntu ec"), 10)
	if !completionContains(got, "ho") {
		t.Fatalf("@ command completions = %#v", got)
	}
	got, _ = completer.Do([]rune("cd al"), 5)
	if !completionContains(got, `pha\ dir/`) {
		t.Fatalf("path completions = %#v", got)
	}
}

func TestCompleterDoesNotSuggestCommandsWithoutPrefix(t *testing.T) {
	sh := &shellState{hostCWD: t.TempDir()}
	completer := newVMSHCompleter(sh)
	if got, _ := completer.Complete([]rune(""), 0); len(got) != 0 {
		t.Fatalf("empty command completions = %#v, want none", got)
	}
}

func TestCompleterSuggestsCommandsAtVMCommandPosition(t *testing.T) {
	t.Setenv("PATH", "")

	sh := &shellState{context: commandContext{Mode: modeHost, VMID: "work"}, hostCWD: t.TempDir()}
	completer := newVMSHCompleter(sh)
	got, _ := completer.Do([]rune("@ubuntu "), 8)
	if !completionContains(got, "echo") || !completionContains(got, "python3") {
		t.Fatalf("empty @ command completions = %#v, want command names", got)
	}
	got, _ = completer.Do([]rune("@ubuntu cat && ec"), len("@ubuntu cat && ec"))
	if !completionContains(got, "ho") {
		t.Fatalf("@ shell separator command completions = %#v, want echo", got)
	}
	got, _ = completer.Do([]rune("pwd && ec"), 9)
	if !completionContains(got, "ho") {
		t.Fatalf("shell separator command completions = %#v, want echo", got)
	}
}

func TestCompleterUsesGuestPathForVMCommands(t *testing.T) {
	t.Setenv("PATH", "")

	api := &fakeVMSHAPI{
		status:       client.InstanceState{ID: "work", Status: "running"},
		streamEvents: []client.ExecEvent{{Kind: "stdout", Data: []byte("stat\nstrace\n")}, {Kind: "exit", ExitCode: 0}},
	}
	sh := &shellState{
		api:     api,
		context: commandContext{Mode: modeHost, VMID: "work"},
		hostCWD: t.TempDir(),
	}
	completer := newVMSHCompleter(sh)
	got, _ := completer.Do([]rune("@ubuntu st"), len("@ubuntu st"))
	if !completionContains(got, "at") || !completionContains(got, "race") {
		t.Fatalf("guest command completions = %#v, want stat and strace", got)
	}
	if len(api.streams) != 1 || api.streams[0].req.Image != "ubuntu" {
		t.Fatalf("guest command completion streams = %#v, want ubuntu command scan", api.streams)
	}
}

func TestCompleterSuggestsPathsAtVMArgumentPosition(t *testing.T) {
	t.Setenv("PATH", "")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh := &shellState{context: commandContext{Mode: modeHost, VMID: "work"}, hostCWD: dir}
	completer := newVMSHCompleter(sh)
	got, _ := completer.Do([]rune("@host cat "), 10)
	if !completionContains(got, "input.txt") {
		t.Fatalf("@host argument path completions = %#v, want input.txt", got)
	}
}

func TestCompleterMapsGuestHostPaths(t *testing.T) {
	root := t.TempDir()
	hostDir := filepath.Join(root, "tmp", "vmsh host")
	if err := os.MkdirAll(filepath.Join(hostDir, "project one"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, guestCWD, err := guestHostPaths(hostDir)
	if err != nil {
		t.Fatalf("guestHostPaths() error = %v", err)
	}
	sh := &shellState{
		hostCWD: root,
		context: commandContext{
			Mode: modeVM,
			CWD:  guestCWD,
		},
	}
	completer := newVMSHCompleter(sh)
	got, _ := completer.Do([]rune("cd pro"), 6)
	if !completionContains(got, `ject\ one/`) {
		t.Fatalf("guest /host path completions = %#v", got)
	}
}

func TestCompleterHandlesEscapedSpacePathTokens(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "alpha dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha dir", "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh := &shellState{hostCWD: root}
	completer := newVMSHCompleter(sh)
	got, replacementLen := completer.Do([]rune(`cat alpha\ dir/no`), len(`cat alpha\ dir/no`))
	if !completionContains(got, "te.txt") {
		t.Fatalf("escaped-space path completions = %#v, want te.txt", got)
	}
	if replacementLen != len("no") {
		t.Fatalf("replacement length = %d, want %d", replacementLen, len("no"))
	}
}

func TestCompleterCompletesGuestAbsolutePathsWithoutFind(t *testing.T) {
	api := &fakeVMSHAPI{
		status:       client.InstanceState{ID: "work", Status: "running"},
		streamEvents: []client.ExecEvent{{Kind: "stdout", Data: []byte("-release\n")}, {Kind: "exit", ExitCode: 0}},
	}
	sh := &shellState{
		api:     api,
		hostCWD: t.TempDir(),
		context: commandContext{Mode: modeVM, VMID: "work", Image: "ubuntu", Network: true},
	}
	completer := newVMSHCompleter(sh)
	got, _ := completer.Do([]rune("cat /etc/os"), 11)
	if !completionContains(got, "-release") {
		t.Fatalf("guest path completions = %#v, want -release", got)
	}
	if len(api.streams) != 1 {
		t.Fatalf("guest completion streams = %d, want 1", len(api.streams))
	}
	run := api.streams[0]
	if run.id != "work" || run.req.Image != "ubuntu" || run.req.Command[0] != "sh" || strings.Contains(strings.Join(run.req.Command, " "), "find ") {
		t.Fatalf("guest completion run = %#v, want sh glob completion without find", run)
	}
	if run.req.WorkDir != "/etc" {
		t.Fatalf("guest completion workdir = %q, want /etc", run.req.WorkDir)
	}
}

func TestCompleterCompletesSharedGuestLocalPaths(t *testing.T) {
	api := &fakeVMSHAPI{
		status:       client.InstanceState{ID: "work", Status: "running"},
		streamEvents: []client.ExecEvent{{Kind: "stdout", Data: []byte(".txt\n")}, {Kind: "exit", ExitCode: 0}},
	}
	sh := &shellState{
		api:     api,
		hostCWD: t.TempDir(),
		context: commandContext{Mode: modeVM, VMID: "work", Image: "ubuntu", CWD: "/home/cc"},
	}
	completer := newVMSHCompleter(sh)
	got, _ := completer.Do([]rune("cat no"), 6)
	if !completionContains(got, ".txt") {
		t.Fatalf("shared guest-local path completions = %#v, want .txt", got)
	}
	if len(api.streams) != 1 || api.streams[0].req.WorkDir != "/home/cc" {
		t.Fatalf("guest completion streams = %#v, want guest-local cwd", api.streams)
	}
}

func TestCompleterUsesArchitectureSpecificGuestImage(t *testing.T) {
	api := &fakeVMSHAPI{
		status:       client.InstanceState{ID: "work", Status: "running"},
		streamEvents: []client.ExecEvent{{Kind: "stdout", Data: []byte("-release\n")}, {Kind: "exit", ExitCode: 0}},
	}
	sh := &shellState{
		api:     api,
		hostCWD: t.TempDir(),
		context: commandContext{Mode: modeVM, VMID: "work", Image: "ubuntu", Arch: "amd64", Network: true},
	}
	completer := newVMSHCompleter(sh)
	got, _ := completer.Do([]rune("cat /etc/os"), 11)
	if !completionContains(got, "-release") {
		t.Fatalf("guest path completions = %#v, want -release", got)
	}
	if len(api.streams) != 1 || api.streams[0].req.Image != "ubuntu@amd64" {
		t.Fatalf("guest completion streams = %#v, want ubuntu@amd64 completion", api.streams)
	}
}

func TestCompleterExpandsAliasesBeforePathCompletion(t *testing.T) {
	api := &fakeVMSHAPI{
		status:       client.InstanceState{ID: "work", Status: "running"},
		streamEvents: []client.ExecEvent{{Kind: "stdout", Data: []byte("-release\n")}, {Kind: "exit", ExitCode: 0}},
	}
	sh := &shellState{
		api:     api,
		aliases: map[string]string{"c": "@ubuntu cat"},
		hostCWD: t.TempDir(),
		context: commandContext{Mode: modeHost, VMID: "work", Network: true},
	}
	completer := newVMSHCompleter(sh)
	got, replacementLen := completer.Do([]rune("c /etc/os"), 9)
	if !completionContains(got, "-release") {
		t.Fatalf("alias guest path completions = %#v, want -release", got)
	}
	if replacementLen != len("os") {
		t.Fatalf("replacement length = %d, want %d", replacementLen, len("os"))
	}
	if len(api.streams) != 1 || api.streams[0].req.Image != "ubuntu" {
		t.Fatalf("guest completion streams = %#v, want ubuntu completion", api.streams)
	}
}

func TestCompleterReturnsLargeCandidateSets(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("item-%02d", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sh := &shellState{hostCWD: dir}
	completer := newVMSHCompleter(sh)

	got, _ := completer.Do([]rune("cat i"), 5)
	if len(got) != 20 {
		t.Fatalf("completion count = %d, want all 20: %#v", len(got), got)
	}
}

func TestCompleterPrioritizesDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zz-file", "aa-file", "src", "bin"} {
		path := filepath.Join(dir, name)
		if name == "src" || name == "bin" {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sh := &shellState{hostCWD: dir}
	completer := newVMSHCompleter(sh)

	got, _ := completer.Do([]rune("cat "), 4)
	if len(got) < 2 || string(got[0]) != "bin/" || string(got[1]) != "src/" {
		t.Fatalf("path ordering = %#v, want directories first", got)
	}
}

func TestMergedEnvOverridesValues(t *testing.T) {
	got := mergedEnv([]string{"TERM=dumb", "PATH=/bin"}, []string{"TERM=xterm-256color", "COLUMNS=120"})
	joined := strings.Join(got, "\n")
	for _, want := range []string{"TERM=xterm-256color", "PATH=/bin", "COLUMNS=120"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mergedEnv() = %#v, missing %q", got, want)
		}
	}
	if strings.Contains(joined, "TERM=dumb") {
		t.Fatalf("mergedEnv() = %#v, kept old TERM", got)
	}
}

func TestGuestCommandEnvPrefersExplicitExportsOverTerminalEnv(t *testing.T) {
	ctx := commandContext{Image: "ubuntu"}
	got := guestCommandEnv(ctx, map[string]string{"TERM": "xterm"}, []string{"TERM=xterm-ghostty", "COLUMNS=183", "LINES=50"})
	joined := strings.Join(got, "\n")
	wantHome := "HOME=/home/ubuntu"
	for _, want := range []string{"TERM=xterm", "COLUMNS=183", "LINES=50", wantHome} {
		if !strings.Contains(joined, want) {
			t.Fatalf("guestCommandEnv() = %#v, missing %q", got, want)
		}
	}
	if strings.Contains(joined, "TERM=xterm-ghostty") {
		t.Fatalf("guestCommandEnv() = %#v, terminal TERM overrode export", got)
	}
}

func envContains(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func completionContains(items [][]rune, want string) bool {
	for _, item := range items {
		if string(item) == want {
			return true
		}
	}
	return false
}

func TestStreamGuestStdinForwardsTTYControlBytes(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()

	done := make(chan struct{})
	out := make(chan client.ExecInput, 4)

	go streamGuestStdin(read, out, done)
	if _, err := write.Write([]byte("ab\x03cd")); err != nil {
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-out:
		if got.Kind != "stdin" || string(got.Data) != "ab\x03cd" {
			t.Fatalf("input = %#v, want raw stdin with control byte", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stdin")
	}

	select {
	case got := <-out:
		if got.Kind != "stdin_close" {
			t.Fatalf("close input = %#v, want stdin_close", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stdin close")
	}
}

func TestGuestCommandPullsMissingImageBeforeRun(t *testing.T) {
	api := &fakeVMSHAPI{
		status:      client.InstanceState{ID: "default", Status: "running"},
		missingImgs: map[string]bool{"ubuntu": true},
	}
	sh := &shellState{
		api:     api,
		context: defaultContext("default", "", false),
		hostCWD: t.TempDir(),
		confirmPull: func(source string, stderr io.Writer) (bool, error) {
			return true, nil
		},
	}
	if err := sh.eval("@ubuntu echo hi", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("eval() error = %v", err)
	}
	if len(api.pulls) != 1 || api.pulls[0].name != "ubuntu" || api.pulls[0].source != "ubuntu" {
		t.Fatalf("pulls = %#v, want ubuntu from ubuntu", api.pulls)
	}
	if len(api.streams) != 1 || api.streams[0].req.Image != "ubuntu" {
		t.Fatalf("streams = %#v, want ubuntu run", api.streams)
	}
}

func TestSaveVMUsesSelectedVMAndCachesImage(t *testing.T) {
	api := &fakeVMSHAPI{}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine"}, hostCWD: t.TempDir(), imageCache: map[string]bool{}}
	var stdout bytes.Buffer

	if err := sh.evalAt("@save saved-tag", &stdout, io.Discard); err != nil {
		t.Fatalf("evalAt(@save) error = %v", err)
	}
	if len(api.saves) != 1 {
		t.Fatalf("saves = %d, want 1", len(api.saves))
	}
	if api.saves[0].id != "work" || api.saves[0].req.Name != "saved-tag" || api.saves[0].req.Image != "alpine" {
		t.Fatalf("save request = %#v, want work saved-tag from alpine", api.saves[0])
	}
	if !sh.imageCache["saved-tag"] {
		t.Fatalf("saved-tag was not cached")
	}
	if got := stdout.String(); !strings.Contains(got, "Saved work as saved-tag") {
		t.Fatalf("stdout = %q, want save message", got)
	}
}

func TestSaveVMAllowsVMOption(t *testing.T) {
	api := &fakeVMSHAPI{}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "selected"}, hostCWD: t.TempDir()}

	if err := sh.evalAt("@save --vm other saved-tag", io.Discard, io.Discard); err != nil {
		t.Fatalf("evalAt(@save --vm) error = %v", err)
	}
	if len(api.saves) != 1 {
		t.Fatalf("saves = %d, want 1", len(api.saves))
	}
	if api.saves[0].id != "other" || api.saves[0].req.Name != "saved-tag" {
		t.Fatalf("save request = %#v, want other saved-tag", api.saves[0])
	}
}

func TestSaveVMRejectsUnsupportedOptions(t *testing.T) {
	api := &fakeVMSHAPI{}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work"}, hostCWD: t.TempDir()}

	err := sh.evalAt("@save --memory 2048 saved-tag", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "usage: @save") {
		t.Fatalf("handleAtLine(save --memory) error = %v, want usage", err)
	}
	if len(api.saves) != 0 {
		t.Fatalf("saves = %d, want 0", len(api.saves))
	}
}

func TestRemoveImageDeletesCachedImage(t *testing.T) {
	api := &fakeVMSHAPI{}
	sh := &shellState{api: api, hostCWD: t.TempDir(), imageCache: map[string]bool{"alpine-gcc": true}}
	var stdout bytes.Buffer

	if err := sh.evalAt("@rmi alpine-gcc", &stdout, io.Discard); err != nil {
		t.Fatalf("evalAt(@rmi) error = %v", err)
	}
	if len(api.deletes) != 1 || api.deletes[0] != "alpine-gcc" {
		t.Fatalf("deletes = %#v, want alpine-gcc", api.deletes)
	}
	if sh.imageCache["alpine-gcc"] {
		t.Fatalf("alpine-gcc remained in image cache")
	}
	if got := stdout.String(); !strings.Contains(got, "Removed alpine-gcc") {
		t.Fatalf("stdout = %q, want remove message", got)
	}
}

func TestRemoveImageRejectsUnsupportedOptions(t *testing.T) {
	api := &fakeVMSHAPI{}
	sh := &shellState{api: api, hostCWD: t.TempDir()}

	err := sh.evalAt("@rmi --vm work alpine-gcc", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "usage: @rmi") {
		t.Fatalf("evalAt(@rmi --vm) error = %v, want usage", err)
	}
	if len(api.deletes) != 0 {
		t.Fatalf("deletes = %d, want 0", len(api.deletes))
	}
}

func TestTmuxLaunchUsesSharedVMShCommand(t *testing.T) {
	var got []string
	sh := &shellState{
		api:       &fakeVMSHAPI{},
		hostCWD:   t.TempDir(),
		rootCache: "/tmp/vmsh cache",
		vmshPath:  "/tmp/bin/vmsh",
		ccvmPath:  "/tmp/bin/ccvm",
		context:   commandContext{VMID: "work", Image: "alpine"},
		tmuxExec: func(args []string) error {
			got = append([]string(nil), args...)
			return nil
		},
	}

	if err := sh.evalAt("@tmux work", io.Discard, io.Discard); err != nil {
		t.Fatalf("evalAt(@tmux) error = %v", err)
	}
	if len(got) == 0 || got[0] != "tmux" {
		t.Fatalf("tmux args = %#v, want tmux command", got)
	}
	vmshPath, err := filepath.Abs("/tmp/bin/vmsh")
	if err != nil {
		t.Fatalf("Abs(vmsh) error = %v", err)
	}
	command := "exec " + shellQuote(vmshPath) + " -cache-dir '/tmp/vmsh cache' -ccvm '/tmp/bin/ccvm' -vm 'work' -image 'alpine'"
	want := []string{
		"tmux", "new-session", "-d", "-A", "-s", "work", "-n", "vmsh", command,
		";", "set-option", "-t", "work", "default-command", command,
		";", "attach-session", "-t", "work",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tmux args = %#v, want %#v", got, want)
	}
}

func TestTmuxLaunchSwitchesClientInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux,1,0")
	var got []string
	sh := &shellState{
		api:       &fakeVMSHAPI{},
		hostCWD:   t.TempDir(),
		rootCache: "/tmp/cache",
		vmshPath:  "/tmp/bin/vmsh",
		tmuxExec: func(args []string) error {
			got = append([]string(nil), args...)
			return nil
		},
	}

	if err := sh.evalAt("@tmux", io.Discard, io.Discard); err != nil {
		t.Fatalf("evalAt(@tmux) error = %v", err)
	}
	if len(got) < 3 || got[len(got)-3] != "switch-client" || got[len(got)-1] != "vmsh" {
		t.Fatalf("tmux args = %#v, want switch-client to vmsh", got)
	}
}

func TestGuestCommandCachesImageAndRunningVMState(t *testing.T) {
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{api: api, context: commandContext{Mode: modeVM, VMID: "work", Image: "ubuntu", Network: true}, hostCWD: t.TempDir()}
	if err := sh.runScript(strings.NewReader("true\ntrue\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runScript() error = %v", err)
	}
	if api.imageGets != 1 {
		t.Fatalf("image gets = %d, want 1", api.imageGets)
	}
	if api.statusGets != 1 {
		t.Fatalf("status gets = %d, want 1", api.statusGets)
	}
	if len(api.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(api.streams))
	}
}

func TestPersistentGuestShellPreservesState(t *testing.T) {
	inputs := make(chan client.ExecInput, 8)
	events := make(chan client.ExecEvent, 8)
	done := make(chan error, 1)
	session := &persistentGuestShell{
		inputs:  inputs,
		events:  events,
		done:    done,
		lastCWD: "/work",
	}
	go func() {
		cwd := "/work"
		alias := false
		for input := range inputs {
			if input.Kind == "stdin_close" {
				break
			}
			line := strings.TrimSpace(string(input.Data))
			switch {
			case strings.HasPrefix(line, "alias gp="):
				alias = true
			case line == "gp" && alias:
				events <- client.ExecEvent{Kind: "stdout", Data: []byte("guest-persist\n")}
			case line == "cd /tmp":
				cwd = "/tmp"
			case line == "pwd":
				events <- client.ExecEvent{Kind: "stdout", Data: []byte(cwd + "\n")}
			}
			events <- client.ExecEvent{Kind: "stdout", Data: []byte("__VMSH_DONE__:0:" + cwd + "\n")}
		}
		close(events)
		done <- nil
	}()
	var out bytes.Buffer
	if err := session.run("alias gp='echo guest-persist'", &out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("alias run error = %v", err)
	}
	if err := session.run("gp", &out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("gp run error = %v", err)
	}
	if err := session.run("cd /tmp", &out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("cd run error = %v", err)
	}
	if err := session.run("pwd", &out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("pwd run error = %v", err)
	}
	session.close()
	if got := out.String(); got != "guest-persist\n/tmp\n" {
		t.Fatalf("output = %q, want guest-persist and /tmp", got)
	}
	if got := session.cwd(); got != "/tmp" {
		t.Fatalf("cwd = %q, want /tmp", got)
	}
}

func TestPersistentGuestShellStartsForwardingAfterCommandInjection(t *testing.T) {
	inputs := make(chan client.ExecInput, 8)
	events := make(chan client.ExecEvent, 8)
	session := &persistentGuestShell{
		inputs: inputs,
		events: events,
		done:   make(chan error, 1),
	}
	started := false
	err := session.run("python3 -m http.server", io.Discard, io.Discard, func() (func(), error) {
		started = true
		select {
		case input := <-inputs:
			if input.Kind != "stdin" || string(input.Data) != "python3 -m http.server\n" {
				t.Fatalf("first input before forwarding = %#v, want command line", input)
			}
		default:
			t.Fatal("forwarding started before command line was injected")
		}
		events <- client.ExecEvent{Kind: "stdout", Data: []byte("__VMSH_DONE__:0:/work\n")}
		return func() {}, nil
	})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !started {
		t.Fatal("forwarding hook was not called")
	}
}

func TestGuestInputForwardingStopSignalsBeforeRestore(t *testing.T) {
	var order []string
	stop := stopGuestInputForwarding(func() {
		order = append(order, "restore")
	}, func() {
		order = append(order, "stop")
	})
	stop()
	if fmt.Sprint(order) != "[stop restore]" {
		t.Fatalf("stop order = %v, want stop before restore", order)
	}
}

func TestForwardGuestSignalsUsesShellInterruptChannel(t *testing.T) {
	inputs := make(chan client.ExecInput, 4)
	done := make(chan struct{})
	interrupts := make(chan os.Signal, 1)
	var stderr bytes.Buffer
	var forwarded []string

	go forwardGuestSignals(inputs, done, false, io.Discard, &stderr, interrupts, func(name string) {
		forwarded = append(forwarded, name)
	})
	interrupts <- os.Interrupt

	select {
	case got := <-inputs:
		if got.Kind != "signal" || got.Signal != "INT" {
			t.Fatalf("forwarded input = %#v, want INT signal", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for forwarded interrupt")
	}
	close(done)
	if fmt.Sprint(forwarded) != "[INT]" {
		t.Fatalf("forwarded callbacks = %#v, want INT", forwarded)
	}
	if stderr.String() != "\n" {
		t.Fatalf("stderr = %q, want interrupt newline", stderr.String())
	}
}

func TestWithoutInterruptSignal(t *testing.T) {
	got := withoutInterruptSignal([]os.Signal{syscall.SIGHUP, os.Interrupt, syscall.SIGTERM})
	if fmt.Sprint(got) != fmt.Sprint([]os.Signal{syscall.SIGHUP, syscall.SIGTERM}) {
		t.Fatalf("withoutInterruptSignal() = %#v", got)
	}
}

func TestPersistentGuestShellKeyIncludesArchitecture(t *testing.T) {
	api := &fakeVMSHAPI{
		streamEvents: []client.ExecEvent{{Kind: "stdout", Data: []byte("__VMSH_READY__:/work\n")}},
	}
	sh := &shellState{api: api}
	req := client.RunRequest{Image: "ubuntu", User: defaultGuestUser, WorkDir: "/work"}
	ctx := commandContext{VMID: "work", Image: "ubuntu"}
	if _, err := sh.guestPersistentShell(ctx, req); err != nil {
		t.Fatalf("guestPersistentShell(native) error = %v", err)
	}
	req.Image = "ubuntu@amd64"
	ctx.Arch = "amd64"
	if _, err := sh.guestPersistentShell(ctx, req); err != nil {
		t.Fatalf("guestPersistentShell(amd64) error = %v", err)
	}
	sh.closeSessions()
	if len(api.streams) != 2 {
		t.Fatalf("streams = %d, want separate native and amd64 shells", len(api.streams))
	}
	if api.streams[0].req.Image != "ubuntu" || api.streams[1].req.Image != "ubuntu@amd64" {
		t.Fatalf("stream images = %q, %q; want ubuntu and ubuntu@amd64", api.streams[0].req.Image, api.streams[1].req.Image)
	}
}

func TestPersistentGuestShellConsumesSplitMarker(t *testing.T) {
	session := &persistentGuestShell{}
	before, _, _, ok := session.consumeOutput("hello\n__VMSH_DONE__:")
	if ok {
		t.Fatalf("consumeOutput partial marker ok=true")
	}
	if before != "hello\n" {
		t.Fatalf("before = %q, want hello output", before)
	}
	before, code, cwd, ok := session.consumeOutput("7:/tmp\n")
	if !ok || before != "" || code != 7 || cwd != "/tmp" {
		t.Fatalf("consumeOutput marker = before %q code %d cwd %q ok %t", before, code, cwd, ok)
	}
}

func TestPersistentGuestShellWaitReadyIncludesStartupStderr(t *testing.T) {
	session := &persistentGuestShell{
		events: make(chan client.ExecEvent, 2),
		done:   make(chan error, 1),
	}
	session.events <- client.ExecEvent{Kind: "stderr", Data: []byte("ccx3-init: exec error: chdir /work: no such file or directory\n")}
	session.events <- client.ExecEvent{Kind: "exit", ExitCode: 126}

	err := session.waitReady()
	if err == nil {
		t.Fatal("waitReady() error = nil, want startup error")
	}
	if !strings.Contains(err.Error(), "ccx3-init: exec error") {
		t.Fatalf("waitReady() error = %q, want startup stderr", err)
	}
}

func TestPersistentStartupMessageKeepsErrorAndDiagnostics(t *testing.T) {
	errorLine := "ccx3-init: exec error: fork/exec /bin/sh: no such file or directory"
	diagnostics := strings.Repeat("diagnostic line\n", 120) + "/proc/sys/fs/binfmt_misc/status: no such file or directory"

	msg := persistentStartupMessage(errorLine + "\n" + diagnostics)

	if !strings.Contains(msg, errorLine) {
		t.Fatalf("persistentStartupMessage() = %q, want initial error", msg)
	}
	if !strings.Contains(msg, "...\n") {
		t.Fatalf("persistentStartupMessage() = %q, want truncation marker", msg)
	}
	if !strings.Contains(msg, "/proc/sys/fs/binfmt_misc/status") {
		t.Fatalf("persistentStartupMessage() = %q, want trailing diagnostics", msg)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`a'b`); got != `'a'"'"'b'` {
		t.Fatalf("shellQuote() = %q", got)
	}
}

func TestGuestHostPaths(t *testing.T) {
	hostCWD := filepath.Join(string(filepath.Separator), "Users", "me", "src")
	abs, err := filepath.Abs(hostCWD)
	if err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
	hostRoot, guestCWD, err := guestHostPaths(hostCWD)
	if err != nil {
		t.Fatalf("guestHostPaths() error = %v", err)
	}
	wantRoot := string(filepath.Separator)
	if volume := filepath.VolumeName(abs); volume != "" {
		wantRoot = volume + string(filepath.Separator)
	}
	rel, err := filepath.Rel(wantRoot, abs)
	if err != nil {
		t.Fatalf("Rel() error = %v", err)
	}
	if hostRoot != wantRoot {
		t.Fatalf("hostRoot = %q", hostRoot)
	}
	if want := path.Join(guestHostMount, filepath.ToSlash(rel)); guestCWD != want {
		t.Fatalf("guestCWD = %q", guestCWD)
	}
}

func TestGuestHostPathToHostPreservesMountedRoot(t *testing.T) {
	hostCWD := filepath.Join(string(filepath.Separator), "Users", "me", "src")
	_, guestCWD, err := guestHostPaths(hostCWD)
	if err != nil {
		t.Fatalf("guestHostPaths() error = %v", err)
	}
	got, ok := guestHostPathToHost(hostCWD, guestCWD)
	if !ok {
		t.Fatalf("guestHostPathToHost() ok=false")
	}
	want, err := filepath.Abs(hostCWD)
	if err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
	if got != want {
		t.Fatalf("guestHostPathToHost() = %q, want %q", got, want)
	}
}

func TestCopySharedGuestPathUsesHostMountMapping(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	_, guestDst, err := guestHostPaths(dst)
	if err != nil {
		t.Fatal(err)
	}
	sh := &shellState{
		context: commandContext{Mode: modeVM, VMID: "work", Image: "alpine", CWD: guestDst},
		hostCWD: dir,
	}

	if err := sh.evalAt("@copy @host:./src .", io.Discard, io.Discard); err != nil {
		t.Fatalf("@copy shared host->guest error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "src", "main.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("copied file = %q, want hello", got)
	}
}

func TestCopyHostToIsolatedGuestUsesAgentFSWrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{
		api:        api,
		context:    commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Isolated: true, CWD: "/work"},
		hostCWD:    dir,
		imageCache: map[string]bool{},
	}

	if err := sh.evalAt("@copy @host:./src .", io.Discard, io.Discard); err != nil {
		t.Fatalf("@copy isolated host->guest error = %v", err)
	}
	if len(api.streams) != 0 {
		t.Fatalf("streams = %d, want 0", len(api.streams))
	}
	if len(api.execStreams) != 1 {
		t.Fatalf("exec streams = %d, want fs_write", len(api.execStreams))
	}
	req := api.execStreams[0].req
	if req.Kind != "fs_write" || req.Path != "/work/src/main.txt" {
		t.Fatalf("exec request = %#v, want fs_write /work/src/main.txt", req)
	}
	if string(req.Stdin) != "hello" {
		t.Fatalf("stdin = %q, want hello", string(req.Stdin))
	}
}

func TestCopyHostFileToGuestCurrentDirectoryUsesAgentFSWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeVMSHAPI{status: client.InstanceState{ID: "work", Status: "running"}}
	sh := &shellState{
		api:        api,
		context:    commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Isolated: true, CWD: "/home/cc"},
		hostCWD:    dir,
		imageCache: map[string]bool{},
	}

	if err := sh.evalAt("@copy @host:./go.mod .", io.Discard, io.Discard); err != nil {
		t.Fatalf("@copy isolated file error = %v", err)
	}
	if len(api.streams) != 0 {
		t.Fatalf("streams = %d, want 0", len(api.streams))
	}
	if len(api.execStreams) != 1 {
		t.Fatalf("exec streams = %d, want fs_write", len(api.execStreams))
	}
	req := api.execStreams[0].req
	if req.Kind != "fs_write" || req.Path != "/home/cc/go.mod" {
		t.Fatalf("exec request = %#v, want fs_write /home/cc/go.mod", req)
	}
	if string(req.Stdin) != "module example.test\n" {
		t.Fatalf("stdin = %q, want module payload", string(req.Stdin))
	}
}

func TestCopyIsolatedGuestToHostUsesAgentFSArchive(t *testing.T) {
	dir := t.TempDir()
	srcRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcRoot, "out.txt"), []byte("guest"), 0o644); err != nil {
		t.Fatal(err)
	}
	var tarData bytes.Buffer
	if err := writePathTar(&tarData, filepath.Join(srcRoot, "out.txt"), "out.txt"); err != nil {
		t.Fatal(err)
	}
	api := &fakeVMSHAPI{
		status: client.InstanceState{ID: "work", Status: "running"},
		streamEventsByCommand: map[string][]client.ExecEvent{
			"fs_archive:/work/out.txt": {
				{Kind: "stdout", Data: tarData.Bytes()},
				{Kind: "exit", ExitCode: 0},
			},
		},
	}
	sh := &shellState{
		api:        api,
		context:    commandContext{Mode: modeVM, VMID: "work", Image: "alpine", Isolated: true, CWD: "/work"},
		hostCWD:    dir,
		imageCache: map[string]bool{},
	}

	if err := sh.evalAt("@copy ./out.txt @host:./copied.txt", io.Discard, io.Discard); err != nil {
		t.Fatalf("@copy isolated guest->host error = %v", err)
	}
	if len(api.execStreams) != 1 {
		t.Fatalf("exec streams = %d, want 1", len(api.execStreams))
	}
	req := api.execStreams[0].req
	if req.Kind != "fs_archive" || req.Path != "/work/out.txt" {
		t.Fatalf("exec request = %#v, want fs_archive /work/out.txt", req)
	}
	got, err := os.ReadFile(filepath.Join(dir, "copied.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "guest" {
		t.Fatalf("copied file = %q, want guest", got)
	}
}

func TestParsePortForwardSpec(t *testing.T) {
	forward, err := parsePortForwardSpec("8080:80")
	if err != nil {
		t.Fatalf("parsePortForwardSpec() error = %v", err)
	}
	if forward.Protocol != "tcp" || forward.HostAddr != "127.0.0.1" || forward.HostPort != 8080 || forward.GuestPort != 80 {
		t.Fatalf("forward = %#v, want tcp 127.0.0.1:8080 -> 80", forward)
	}
}

type fakeVMSHAPI struct {
	mu                    sync.Mutex
	status                client.InstanceState
	statuses              []client.InstanceState
	streams               []fakeRun
	execStreams           []fakeExec
	streamInputs          []fakeRunInput
	starts                []fakeStart
	shutdowns             []string
	saves                 []fakeSave
	deletes               []string
	streamEvents          []client.ExecEvent
	streamEventsByCommand map[string][]client.ExecEvent
	bootEvents            []client.BootEvent
	pullEvents            []client.ProgressEvent
	pulls                 []fakePull
	missingImgs           map[string]bool
	saveState             client.ImageState
	saveErr               error
	deleteErr             error
	imageGets             int
	statusGets            int
}

type fakeRun struct {
	id  string
	req client.RunRequest
}

type fakeExec struct {
	id  string
	req client.ExecRequest
}

type fakeRunInput struct {
	id      string
	command string
	inputs  []client.ExecInput
}

type fakeStart struct {
	id  string
	req client.StartInstanceRequest
}

type fakePull struct {
	name   string
	source string
	arch   string
}

type fakeSave struct {
	id  string
	req client.SaveImageRequest
}

func (f *fakeVMSHAPI) HealthCheck() error { return nil }

func (f *fakeVMSHAPI) Capabilities() (client.CapabilitiesResponse, error) {
	return client.CapabilitiesResponse{}, nil
}

func (f *fakeVMSHAPI) GetImage(name string) (client.ImageState, error) {
	f.imageGets++
	if f.missingImgs != nil && f.missingImgs[name] {
		return client.ImageState{}, fmt.Errorf("missing image")
	}
	return client.ImageState{Name: name, Status: "available"}, nil
}

func (f *fakeVMSHAPI) PullImageStream(name string, req client.PullImageRequest, onEvent func(client.ProgressEvent) error) error {
	source, err := req.SourceString()
	if err != nil {
		return err
	}
	f.pulls = append(f.pulls, fakePull{name: name, source: source, arch: req.Architecture})
	if f.missingImgs != nil {
		f.missingImgs[name] = false
	}
	if onEvent != nil {
		events := f.pullEvents
		if len(events) == 0 {
			events = []client.ProgressEvent{{Status: "downloaded", Artifact: name}}
		}
		for _, event := range events {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeVMSHAPI) DeleteImage(name string) error {
	f.deletes = append(f.deletes, name)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return nil
}

func (f *fakeVMSHAPI) SaveInstanceImage(id string, req client.SaveImageRequest) (client.ImageState, error) {
	f.saves = append(f.saves, fakeSave{id: id, req: req})
	if f.saveErr != nil {
		return client.ImageState{}, f.saveErr
	}
	if f.saveState.Name != "" {
		return f.saveState, nil
	}
	return client.ImageState{Name: req.Name, Status: "downloaded"}, nil
}

func (f *fakeVMSHAPI) StartInstanceStreamWithID(id string, req client.StartInstanceRequest, onEvent func(client.BootEvent) error) (client.InstanceState, error) {
	f.starts = append(f.starts, fakeStart{id: id, req: req})
	f.status = client.InstanceState{ID: id, Status: "running"}
	if onEvent != nil {
		for _, event := range f.bootEvents {
			if err := onEvent(event); err != nil {
				return client.InstanceState{}, err
			}
		}
	}
	return f.status, nil
}

func (f *fakeVMSHAPI) ShutdownInstanceWithID(id string) error {
	f.shutdowns = append(f.shutdowns, id)
	f.status = client.InstanceState{ID: id, Status: "stopped"}
	return nil
}

func (f *fakeVMSHAPI) InstanceStatusOf(id string) (client.InstanceState, error) {
	f.statusGets++
	if f.status.ID == "" {
		return client.InstanceState{ID: id, Status: "stopped"}, nil
	}
	return f.status, nil
}

func (f *fakeVMSHAPI) InstanceStatuses() ([]client.InstanceState, error) {
	return f.statuses, nil
}

func (f *fakeVMSHAPI) AddPortForwardTo(string, client.PortForward) error {
	return nil
}

func (f *fakeVMSHAPI) CreateWatchdogLease(client.WatchdogLeaseRequest) (client.WatchdogLeaseResponse, error) {
	return client.WatchdogLeaseResponse{LeaseID: "test-lease", TimeoutSeconds: 10}, nil
}

func (f *fakeVMSHAPI) FeedWatchdogLease(string) error {
	return nil
}

func (f *fakeVMSHAPI) ReleaseWatchdogLease(string) error {
	return nil
}

func (f *fakeVMSHAPI) RunStreamIn(id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
	return f.RunStreamInContext(context.Background(), id, req, onEvent)
}

func (f *fakeVMSHAPI) RunStreamInContext(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.RunInteractiveStreamIn(id, req, nil, onEvent)
}

func (f *fakeVMSHAPI) RunInteractiveStreamIn(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	var captured []client.ExecInput
	if inputs != nil && !req.TTY {
		for input := range inputs {
			captured = append(captured, input)
		}
	}
	command := runRequestCommandString(req)
	f.mu.Lock()
	f.streams = append(f.streams, fakeRun{id: id, req: req})
	if inputs != nil {
		f.streamInputs = append(f.streamInputs, fakeRunInput{id: id, command: command, inputs: captured})
	}
	events := f.streamEvents
	if f.streamEventsByCommand != nil {
		for suffix, commandEvents := range f.streamEventsByCommand {
			if strings.HasSuffix(command, suffix) {
				events = commandEvents
				break
			}
		}
	}
	f.mu.Unlock()
	if len(events) == 0 {
		events = []client.ExecEvent{{Kind: "exit", ExitCode: 0}}
	}
	for _, event := range events {
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeVMSHAPI) ExecStreamIn(id string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	var captured []client.ExecInput
	if inputs != nil {
		for input := range inputs {
			captured = append(captured, input)
		}
	}
	f.mu.Lock()
	f.execStreams = append(f.execStreams, fakeExec{id: id, req: req})
	if inputs != nil {
		f.streamInputs = append(f.streamInputs, fakeRunInput{id: id, command: req.Kind + ":" + req.Path, inputs: captured})
	}
	events := f.streamEvents
	if f.streamEventsByCommand != nil {
		key := req.Kind + ":" + req.Path
		if commandEvents, ok := f.streamEventsByCommand[key]; ok {
			events = commandEvents
		}
	}
	f.mu.Unlock()
	if len(events) == 0 {
		events = []client.ExecEvent{{Kind: "exit", ExitCode: 0}}
	}
	for _, event := range events {
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func runRequestCommandString(req client.RunRequest) string {
	if len(req.Command) == 0 {
		return ""
	}
	return req.Command[len(req.Command)-1]
}

func (f *fakeVMSHAPI) inputForCommand(command string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, runInput := range f.streamInputs {
		if !strings.HasSuffix(runInput.command, command) {
			continue
		}
		var b strings.Builder
		for _, input := range runInput.inputs {
			if input.Kind == "stdin" {
				b.Write(input.Data)
			}
		}
		return b.String()
	}
	return ""
}
