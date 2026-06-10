package shell

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"j5.nz/cc/client"
)

func TestShellCommandPassingBuildsGuestRunRequests(t *testing.T) {
	api := newRecordingShellAPI("alpine", "alpine@amd64")
	sh := newUnitShell(t, api)
	script := strings.Join([]string{
		"@alpine --vm work --arch amd64 --memory 2g --cpus 4 --no-network --nested --cwd /work --user app",
		"printf 'hello | %s' \"$USER\"",
		"@sudo whoami",
	}, "\n")

	stdout, stderr, err := runShellUnitScript(sh, script)
	if err != nil {
		t.Fatalf("run script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	if len(api.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(api.starts))
	}
	start := api.starts[0]
	if start.id != "work" {
		t.Fatalf("start id = %q, want work", start.id)
	}
	if start.req.Image != "alpine@amd64" || start.req.MemoryMB != 2048 || start.req.CPUs != 4 || !start.req.NestedVirt {
		t.Fatalf("start request = %+v", start.req)
	}
	if start.req.Network != nil {
		t.Fatalf("start network = %+v, want nil for --no-network", start.req.Network)
	}

	if len(api.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(api.runs))
	}
	first := api.runs[0]
	if first.id != "work" {
		t.Fatalf("first run id = %q, want work", first.id)
	}
	if first.req.Image != "alpine@amd64" || first.req.WorkDir != "/work" || first.req.User != "app" {
		t.Fatalf("first run context = %+v", first.req)
	}
	if first.req.MemoryMB != 2048 || first.req.CPUs != 4 || !first.req.NestedVirt {
		t.Fatalf("first run resources = %+v", first.req)
	}
	if first.req.Network != nil {
		t.Fatalf("first run network = %+v, want nil for --no-network", first.req.Network)
	}
	if len(first.req.Shares) != 1 || first.req.Shares[0].Mount != guestHostMount || !first.req.Shares[0].Writable || !first.req.Shares[0].MapOwner {
		t.Fatalf("first run shares = %+v", first.req.Shares)
	}
	if !strings.HasSuffix(first.req.Command[2], "printf 'hello | %s' \"$USER\"") {
		t.Fatalf("first run command = %#v", first.req.Command)
	}

	second := api.runs[1]
	if second.req.User != "root" {
		t.Fatalf("sudo run user = %q, want root", second.req.User)
	}
	if !strings.HasSuffix(second.req.Command[2], "whoami") {
		t.Fatalf("sudo command = %#v", second.req.Command)
	}
}

func TestSudoAliasExpandsAcrossVMShCommandLists(t *testing.T) {
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "alpine", Network: true}

	var stdout, stderr bytes.Buffer
	if err := sh.eval("@alias sudo=@sudo", &stdout, &stderr); err != nil {
		t.Fatalf("set sudo alias: %v", err)
	}
	if err := sh.eval("sudo first && sudo second", &stdout, &stderr); err != nil {
		t.Fatalf("run sudo alias command list: %v\nstderr:\n%s", err, stderr.String())
	}
	if len(api.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(api.runs))
	}
	for i, run := range api.runs {
		if run.req.User != "root" {
			t.Fatalf("run %d user = %q, want root", i, run.req.User)
		}
	}
	if !strings.Contains(api.runs[0].req.Command[2], "first") || !strings.Contains(api.runs[1].req.Command[2], "second") {
		t.Fatalf("commands = %#v, %#v", api.runs[0].req.Command, api.runs[1].req.Command)
	}

	api.runs = nil
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		code := 0
		if len(api.runs) == 1 {
			code = 1
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: code})
	}
	if err := sh.eval("sudo false && sudo skipped", &stdout, &stderr); err != nil {
		t.Fatalf("run short-circuit sudo alias command list: %v", err)
	}
	if len(api.runs) != 1 {
		t.Fatalf("short-circuit runs = %d, want 1", len(api.runs))
	}
	if sh.lastCode != 1 {
		t.Fatalf("lastCode = %d, want 1", sh.lastCode)
	}
}

func TestCompletionsUseCachedImagesOptionsAndHostMappedPaths(t *testing.T) {
	api := newRecordingShellAPI("alpine", "ubuntu")
	sh := newUnitShell(t, api)
	for _, dir := range []string{
		filepath.Join(sh.rootCache, "images", "alpine"),
		filepath.Join(sh.rootCache, "images", "ubuntu"),
		filepath.Join(sh.rootCache, "images", "blobs"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create image cache dir: %v", err)
		}
	}
	if err := os.Mkdir(filepath.Join(sh.hostCWD, "alpha dir"), 0o755); err != nil {
		t.Fatalf("create host dir: %v", err)
	}

	c := newVMSHCompleter(sh)
	candidates, replaceLen, kind := c.CompleteWithKind([]rune("@al"), len("@al"))
	if kind != completionAt || replaceLen != len("@al") || !hasString(candidates, "pine") {
		t.Fatalf("@ image completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
	if hasString(candidates, "blobs") {
		t.Fatalf("internal image cache dir completed: %q", candidates)
	}

	candidates, replaceLen, kind = c.CompleteWithKind([]rune("@alpine --n"), len("@alpine --n"))
	if kind != completionOption || replaceLen != len("--n") || !hasString(candidates, "etwork") || !hasString(candidates, "ested") {
		t.Fatalf("option completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}

	candidates, replaceLen, kind = c.CompleteWithKind([]rune("@rmi al"), len("@rmi al"))
	if kind != completionAt || replaceLen != len("al") || !reflect.DeepEqual(candidates, []string{"pine"}) {
		t.Fatalf("@rmi completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}

	_, guestCWD, err := guestHostPaths(sh.hostCWD)
	if err != nil {
		t.Fatalf("guest host paths: %v", err)
	}
	sh.context = commandContext{Mode: modeVM, VMID: "vm", Image: "alpine", CWD: guestCWD}
	candidates, replaceLen, kind = c.CompleteWithKind([]rune("cat a"), len("cat a"))
	if kind != completionPath || replaceLen != len("a") || !hasString(candidates, "lpha\\ dir/") {
		t.Fatalf("host-mapped path completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
}

func TestCompletionsUseCurrentCommandSegmentAndGuestCommands(t *testing.T) {
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "vmtool\n"}); err != nil {
			return err
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "alpine"}
	c := newVMSHCompleter(sh)

	line := []rune("echo ok && vm")
	candidates, replaceLen, kind := c.CompleteWithKind(line, len(line))
	if kind != completionCommand || replaceLen != len("vm") || !hasString(candidates, "tool") {
		t.Fatalf("guest command completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
	if len(api.runs) != 1 || api.runs[0].id != "default" {
		t.Fatalf("guest command completion runs = %+v", api.runs)
	}

	line = []rune("printf x | @host ec")
	candidates, replaceLen, kind = c.CompleteWithKind(line, len(line))
	if kind != completionCommand || replaceLen != len("ec") || !hasString(candidates, "ho") {
		t.Fatalf("host command completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
}

func TestPromptCWDColorDistinguishesContextStorage(t *testing.T) {
	sh := newUnitShell(t, newRecordingShellAPI("alpine"))
	sh.context = commandContext{Mode: modeHost}
	if got := sh.promptCWDColor(sh.hostCWD); got != colorCyan {
		t.Fatalf("host cwd color = %q", got)
	}

	sh.context = commandContext{Mode: modeVM, Image: "alpine"}
	if got := sh.promptCWDColor(guestHostMount + "/tmp"); got != colorCyan {
		t.Fatalf("shared cwd color = %q", got)
	}
	if got := sh.promptCWDColor("/tmp"); got != colorYellow {
		t.Fatalf("guest-local cwd color = %q", got)
	}

	sh.context.Isolated = true
	if got := sh.promptCWDColor("/tmp"); got != colorMagenta {
		t.Fatalf("isolated cwd color = %q", got)
	}
}

func TestHostCommandInterruptIsNotFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host interrupt test uses POSIX shell commands")
	}
	session, err := startPersistentHostShell(t.TempDir(), nil, 80, 24, "")
	if err != nil {
		t.Fatalf("start persistent host shell: %v", err)
	}
	t.Cleanup(session.close)

	var interrupted atomic.Bool
	errCh := make(chan error, 1)
	go func() {
		errCh <- session.run("sleep 30", io.Discard, io.Discard, func() (func(), error) {
			go func() {
				time.Sleep(100 * time.Millisecond)
				interrupted.Store(true)
				_, _ = session.tty.Write([]byte{0x03})
			}()
			return func() {}, nil
		})
	}()

	select {
	case err := <-errCh:
		if interrupted.Load() && err != nil && sessionLastCode(err) < 0 {
			err = persistentShellExit{code: 130}
		}
		if sessionLastCode(err) != 130 {
			t.Fatalf("interrupted host command error = %v, code %d; want 130", err, sessionLastCode(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("interrupted host command did not return")
	}
}

func TestPersistentHostShellRunsShortCommandsAndPipelines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent host shell requires a Unix PTY")
	}
	sh := newUnitShell(t, newRecordingShellAPI())

	if err := sh.runHost("printf short > short.txt", io.Discard, io.Discard); err != nil {
		t.Fatalf("run short command: %v", err)
	}
	shortData, err := os.ReadFile(filepath.Join(sh.hostCWD, "short.txt"))
	if err != nil {
		t.Fatalf("read short command output: %v", err)
	}
	if string(shortData) != "short" {
		t.Fatalf("short command wrote %q", string(shortData))
	}

	if err := sh.runHost(`printf 'alpha\nbeta\n' | grep beta > pipeline.txt`, io.Discard, io.Discard); err != nil {
		t.Fatalf("run pipeline command: %v", err)
	}
	pipelineData, err := os.ReadFile(filepath.Join(sh.hostCWD, "pipeline.txt"))
	if err != nil {
		t.Fatalf("read pipeline output: %v", err)
	}
	if string(pipelineData) != "beta\n" {
		t.Fatalf("pipeline command wrote %q", string(pipelineData))
	}
}

func TestPersistentHostShellCanReadForwardedInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent host shell requires a Unix PTY")
	}
	dir := t.TempDir()
	session, err := startPersistentHostShell(dir, nil, 80, 24, "")
	if err != nil {
		t.Fatalf("start persistent host shell: %v", err)
	}
	t.Cleanup(session.close)

	err = session.run("read value; printf '%s' \"$value\" > input.txt", io.Discard, io.Discard, func() (func(), error) {
		go func() {
			time.Sleep(50 * time.Millisecond)
			_, _ = session.tty.Write([]byte("from-stdin\n"))
		}()
		return func() {}, nil
	})
	if err != nil {
		t.Fatalf("run persistent command with forwarded input: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "input.txt"))
	if err != nil {
		t.Fatalf("read forwarded input output: %v", err)
	}
	if string(data) != "from-stdin" {
		t.Fatalf("forwarded input wrote %q", string(data))
	}
}

func TestPersistentHostShellStreamsPartialOutputBeforeCompletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent host shell requires a Unix PTY")
	}
	session, err := startPersistentHostShell(t.TempDir(), nil, 80, 24, "")
	if err != nil {
		t.Fatalf("start persistent host shell: %v", err)
	}
	t.Cleanup(session.close)

	stdout := newNotifyWriter("partial")
	errCh := make(chan error, 1)
	go func() {
		errCh <- session.run(`printf '\160\141\162\164\151\141\154'; sleep 1; printf done`, stdout, io.Discard, nil)
	}()

	select {
	case <-stdout.seen:
	case err := <-errCh:
		t.Fatalf("command returned before streaming partial output: %v", err)
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("partial output was not streamed before command completion")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("persistent command failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("persistent command did not finish")
	}
	if !strings.Contains(stdout.String(), "partialdone") {
		t.Fatalf("streamed output = %q", stdout.String())
	}
}

func TestPersistentHostShellMarkerScannerHandlesSplitMarker(t *testing.T) {
	p := &persistentHostShell{}
	out, _, _, done := p.consumeOutputChunk("frame")
	if out != "frame" || done {
		t.Fatalf("first chunk out=%q done=%t", out, done)
	}
	out, _, _, done = p.consumeOutputChunk("__VMSH_")
	if out != "" || done {
		t.Fatalf("marker prefix chunk out=%q done=%t", out, done)
	}
	out, code, cwd, done := p.consumeOutputChunk("DONE__:7:/tmp\r\n")
	if out != "" || !done || code != 7 || cwd != "/tmp" {
		t.Fatalf("marker completion out=%q done=%t code=%d cwd=%q", out, done, code, cwd)
	}
}

func TestPipelineParsingHandlesShellOperatorsAndQuotedPipes(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantOK  bool
		want    []string
		wantErr string
	}{
		{
			name:   "quoted pipes split only at real pipeline",
			line:   `printf 'a|b' | grep 'a|b' | wc -l`,
			wantOK: true,
			want:   []string{`printf 'a|b'`, `grep 'a|b'`, `wc -l`},
		},
		{
			name:   "double pipe is shell operator",
			line:   `false || printf fallback`,
			wantOK: false,
		},
		{
			name:   "escaped pipe is literal",
			line:   `printf \|`,
			wantOK: false,
		},
		{
			name:    "empty segment",
			line:    `printf x | | cat`,
			wantOK:  true,
			wantErr: "pipeline segment is empty",
		},
		{
			name:   "unfinished quote without pipeline is normal shell input",
			line:   `printf 'unterminated`,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := splitPipelineLine(tt.line)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				if ok != tt.wantOK {
					t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
				}
				return
			}
			if err != nil {
				t.Fatalf("split pipeline: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("segments = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestHostPipelineExecutionHandlesQuotedPipesAndShellOr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host pipeline test uses POSIX shell commands")
	}
	sh := newUnitShell(t, newRecordingShellAPI())
	var stdout, stderr bytes.Buffer

	err := sh.eval(`printf 'a|b\nskip\n' | grep 'a|b' | wc -l`, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run quoted host pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "1" {
		t.Fatalf("pipeline stdout = %q, want line count 1", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	err = sh.eval(`false || printf fallback`, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run shell OR command: %v\nstderr:\n%s", err, stderr.String())
	}
	if stdout.String() != "fallback" {
		t.Fatalf("OR stdout = %q, want fallback", stdout.String())
	}
}

func TestPlainGuestPipelineRunsInsideGuestShell(t *testing.T) {
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		t.Fatalf("plain guest pipeline used vmsh streaming path")
		return nil
	}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "alpine", Network: true}

	var stdout, stderr bytes.Buffer
	if err := sh.eval(`printf 'alpha\nbeta\n' | grep beta`, &stdout, &stderr); err != nil {
		t.Fatalf("run plain guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if len(api.runs) != 1 {
		t.Fatalf("guest runs = %d, want one shell command", len(api.runs))
	}
	if !strings.Contains(api.runs[0].req.Command[2], "| grep beta") {
		t.Fatalf("guest command = %#v", api.runs[0].req.Command)
	}
}

func TestMixedPipelineStreamsHostInputToGuestAndGuestOutputToHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mixed pipeline test uses POSIX host commands")
	}
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	guestStdin := make(chan []byte, 1)
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		data, closeEvents := drainExecInputStream(inputs)
		if closeEvents != 0 {
			t.Fatalf("pipeline input sent explicit stdin_close events = %d, want channel EOF", closeEvents)
		}
		guestStdin <- data
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	sh := newUnitShell(t, api)

	var stdout, stderr bytes.Buffer
	if err := sh.eval(`printf guest-data | @alpine cat`, &stdout, &stderr); err != nil {
		t.Fatalf("run host-to-guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	select {
	case stdin := <-guestStdin:
		if string(stdin) != "guest-data" {
			t.Fatalf("guest stdin = %q, want guest-data", string(stdin))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("guest stdin was not drained")
	}

	stdout.Reset()
	stderr.Reset()
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "guest-output\nother\n"}); err != nil {
			return err
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	if err := sh.eval(`@alpine printf ignored | @host grep guest-output`, &stdout, &stderr); err != nil {
		t.Fatalf("run guest-to-host pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if stdout.String() != "guest-output\n" {
		t.Fatalf("guest-to-host stdout = %q", stdout.String())
	}
}

func TestGuestPipelineStreamsStdinForGuestStages(t *testing.T) {
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "alpine", Network: true}
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		command := ""
		if len(req.Command) > 2 {
			command = req.Command[2]
		}
		if strings.Contains(command, "printf 'script-from-guest'") {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "script-from-guest"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		data, closeEvents := drainExecInputStream(inputs)
		if closeEvents != 0 {
			return fmt.Errorf("pipeline input sent explicit stdin_close events = %d", closeEvents)
		}
		if string(data) != "script-from-guest" {
			return fmt.Errorf("guest stdin = %q", string(data))
		}
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent != nil {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "guest-stage-ok\n"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	if err := sh.eval(`printf 'script-from-guest' | @alpine sh`, &stdout, &stderr); err != nil {
		t.Fatalf("run guest-to-guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if stdout.String() != "guest-stage-ok\n" {
		t.Fatalf("guest pipeline stdout = %q", stdout.String())
	}
	if len(api.runs) != 2 {
		t.Fatalf("guest runs = %d, want 2", len(api.runs))
	}

	api.runs = nil
	stdout.Reset()
	stderr.Reset()
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		data, closeEvents := drainExecInputStream(inputs)
		if len(data) != 0 || closeEvents != 0 {
			return fmt.Errorf("empty pipeline data=%d closeEvents=%d", len(data), closeEvents)
		}
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	if err := sh.eval(`true | @alpine cat`, &stdout, &stderr); err != nil {
		t.Fatalf("run empty guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if len(api.runs) != 2 {
		t.Fatalf("empty guest pipeline runs = %d, want 2", len(api.runs))
	}
	if len(api.runs[1].req.Stdin) != 0 {
		t.Fatalf("empty guest pipeline stdin = %q", string(api.runs[1].req.Stdin))
	}
}

func TestGuestPipelineStreamsLargeInputInChunks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("large pipeline test uses POSIX host commands")
	}
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	chunkCount := make(chan int, 1)
	byteCount := make(chan int, 1)
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		total := 0
		chunks := 0
		for input := range inputs {
			if input.Kind != "stdin" {
				continue
			}
			chunks++
			if len(input.Data) > 0 {
				total += len(input.Data)
			} else {
				total += len(input.Input)
			}
		}
		chunkCount <- chunks
		byteCount <- total
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	sh := newUnitShell(t, api)

	var stdout, stderr bytes.Buffer
	if err := sh.eval(`dd if=/dev/zero bs=1024 count=128 2>/dev/null | @alpine wc -c`, &stdout, &stderr); err != nil {
		t.Fatalf("run large host-to-guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	select {
	case got := <-byteCount:
		if got != 128*1024 {
			t.Fatalf("streamed bytes = %d, want %d", got, 128*1024)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("guest stdin was not drained")
	}
	if chunks := <-chunkCount; chunks < 2 {
		t.Fatalf("streamed chunks = %d, want multiple chunks", chunks)
	}
}

func TestCopyEndpointResolutionAndGuestHostPathSafety(t *testing.T) {
	api := newRecordingShellAPI("alpine", "ubuntu")
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "vm", Image: "alpine"}
	_, guestCWD, err := guestHostPaths(sh.hostCWD)
	if err != nil {
		t.Fatalf("guest host paths: %v", err)
	}

	guest, err := sh.parseCopyEndpoint("@:notes.txt")
	if err != nil {
		t.Fatalf("parse current guest endpoint: %v", err)
	}
	if guest.ctx.Mode != modeVM || guest.path != path.Join(guestCWD, "notes.txt") {
		t.Fatalf("current guest endpoint = %+v", guest)
	}

	host, err := sh.parseCopyEndpoint("@host:relative.txt")
	if err != nil {
		t.Fatalf("parse host endpoint: %v", err)
	}
	if host.ctx.Mode != modeHost || host.path != filepath.Join(sh.hostCWD, "relative.txt") {
		t.Fatalf("host endpoint = %+v", host)
	}

	ubuntu, err := sh.parseCopyEndpoint("@ubuntu:~/result.txt")
	if err != nil {
		t.Fatalf("parse named guest endpoint: %v", err)
	}
	if ubuntu.ctx.Image != "ubuntu" || ubuntu.path != "/home/ubuntu/result.txt" {
		t.Fatalf("named guest endpoint = %+v", ubuntu)
	}

	if _, err := sh.parseCopyEndpoint("@ubuntu"); err == nil || !strings.Contains(err.Error(), "must use @target:path") {
		t.Fatalf("parse malformed endpoint error = %v", err)
	}
	if hostPath, ok := guestHostPathToHost(sh.hostCWD, "/tmp/file"); ok || hostPath != "" {
		t.Fatalf("non-host guest path mapped to %q", hostPath)
	}
}

func TestExtractTarToHostRejectsTraversal(t *testing.T) {
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatalf("create dst: %v", err)
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: int64(len("nope"))}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write([]byte("nope")); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	err := extractTarToHost(bytes.NewReader(archive.Bytes()), dst, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe tar path") {
		t.Fatalf("extract traversal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal file exists or stat failed unexpectedly: %v", err)
	}
}

func newUnitShell(t *testing.T, api *recordingShellAPI) *shellState {
	t.Helper()
	hostCWD := t.TempDir()
	rootCache := t.TempDir()
	sh := &shellState{
		api:        api,
		context:    defaultContext("default", "", false),
		hostCWD:    hostCWD,
		rootCache:  rootCache,
		imageCache: map[string]bool{},
		vmRunning:  map[string]bool{},
		contextCWD: map[string]string{},
		promptOut:  io.Discard,
		env:        map[string]string{},
		aliases:    map[string]string{},
		confirmPull: func(string, io.Writer) (bool, error) {
			return false, nil
		},
		confirmVMRestart: func(string, io.Writer) (bool, error) {
			return true, nil
		},
	}
	sh.completion = newVMSHCompleter(sh)
	return sh
}

func runShellUnitScript(sh *shellState, script string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := sh.evalScriptLines(strings.NewReader(script), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

type notifyWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	target string
	seen   chan struct{}
	once   sync.Once
}

func newNotifyWriter(target string) *notifyWriter {
	return &notifyWriter{target: target, seen: make(chan struct{})}
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.target) {
		w.once.Do(func() {
			close(w.seen)
		})
	}
	return n, err
}

func (w *notifyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type recordingShellAPI struct {
	images         map[string]client.ImageState
	instances      map[string]client.InstanceState
	starts         []recordedStart
	runs           []recordedRun
	execs          []recordedExec
	forwards       []recordedForward
	deleted        []string
	runStream      func(context.Context, string, client.RunRequest, func(client.ExecEvent) error) error
	runInteractive func(string, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
}

type recordedStart struct {
	id  string
	req client.StartInstanceRequest
}

type recordedRun struct {
	id  string
	req client.RunRequest
}

type recordedExec struct {
	id  string
	req client.ExecRequest
}

type recordedForward struct {
	id      string
	forward client.PortForward
}

func newRecordingShellAPI(images ...string) *recordingShellAPI {
	api := &recordingShellAPI{
		images:    map[string]client.ImageState{},
		instances: map[string]client.InstanceState{},
	}
	for _, image := range images {
		api.images[image] = client.ImageState{Name: image, Status: "ready"}
	}
	return api
}

func (a *recordingShellAPI) HealthCheck() error { return nil }

func (a *recordingShellAPI) Capabilities() (client.CapabilitiesResponse, error) {
	return client.CapabilitiesResponse{VMSupported: true, SupportsNestedVirt: true}, nil
}

func (a *recordingShellAPI) GetImage(name string) (client.ImageState, error) {
	if image, ok := a.images[name]; ok {
		return image, nil
	}
	return client.ImageState{}, fmt.Errorf("image %q not found", name)
}

func (a *recordingShellAPI) PullImageStream(name string, req client.PullImageRequest, onEvent func(client.ProgressEvent) error) error {
	a.images[name] = client.ImageState{Name: name, Source: req.Source, Status: "ready"}
	return nil
}

func (a *recordingShellAPI) DeleteImage(name string) error {
	a.deleted = append(a.deleted, name)
	delete(a.images, name)
	return nil
}

func (a *recordingShellAPI) SaveInstanceImage(id string, req client.SaveImageRequest) (client.ImageState, error) {
	state := client.ImageState{Name: req.Name, Source: "vm:" + id, Status: "ready"}
	a.images[req.Name] = state
	return state, nil
}

func (a *recordingShellAPI) StartInstanceStreamWithID(id string, req client.StartInstanceRequest, onEvent func(client.BootEvent) error) (client.InstanceState, error) {
	a.starts = append(a.starts, recordedStart{id: id, req: req})
	state := client.InstanceState{ID: id, Status: "running", Image: req.Image, MemoryMB: req.MemoryMB, CPUs: req.CPUs, NestedVirt: req.NestedVirt}
	a.instances[id] = state
	if onEvent != nil {
		if err := onEvent(client.BootEvent{Kind: "ready", State: state}); err != nil {
			return client.InstanceState{}, err
		}
	}
	return state, nil
}

func (a *recordingShellAPI) ShutdownInstanceWithID(id string) error {
	a.instances[id] = client.InstanceState{ID: id, Status: "stopped"}
	return nil
}

func (a *recordingShellAPI) InstanceStatusOf(id string) (client.InstanceState, error) {
	if state, ok := a.instances[id]; ok {
		return state, nil
	}
	return client.InstanceState{ID: id, Status: "stopped"}, nil
}

func (a *recordingShellAPI) InstanceStatuses() ([]client.InstanceState, error) {
	var states []client.InstanceState
	for _, state := range a.instances {
		states = append(states, state)
	}
	return states, nil
}

func (a *recordingShellAPI) AddPortForwardTo(id string, forward client.PortForward) error {
	a.forwards = append(a.forwards, recordedForward{id: id, forward: forward})
	return nil
}

func (a *recordingShellAPI) CreateWatchdogLease(req client.WatchdogLeaseRequest) (client.WatchdogLeaseResponse, error) {
	return client.WatchdogLeaseResponse{LeaseID: "lease", TimeoutSeconds: req.TimeoutSeconds}, nil
}

func (a *recordingShellAPI) FeedWatchdogLease(string) error { return nil }

func (a *recordingShellAPI) ReleaseWatchdogLease(string) error { return nil }

func (a *recordingShellAPI) RunStreamIn(id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
	return a.RunStreamInContext(context.Background(), id, req, onEvent)
}

func (a *recordingShellAPI) RunStreamInContext(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
	if a.runStream != nil {
		return a.runStream(ctx, id, req, onEvent)
	}
	a.runs = append(a.runs, recordedRun{id: id, req: req})
	if onEvent != nil {
		if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "guest-output\n"}); err != nil {
			return err
		}
		if err := onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0}); err != nil {
			return err
		}
	}
	return nil
}

func (a *recordingShellAPI) RunInteractiveStreamIn(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return a.RunInteractiveStreamInContext(context.Background(), id, req, inputs, onEvent)
}

func (a *recordingShellAPI) RunInteractiveStreamInContext(ctx context.Context, id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	if a.runInteractive != nil {
		return a.runInteractive(id, req, inputs, onEvent)
	}
	return a.RunStreamInContext(ctx, id, req, onEvent)
}

func (a *recordingShellAPI) ExecStreamIn(id string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	a.execs = append(a.execs, recordedExec{id: id, req: req})
	if onEvent != nil {
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	return nil
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func drainExecInputStream(inputs <-chan client.ExecInput) ([]byte, int) {
	var data bytes.Buffer
	closeEvents := 0
	for input := range inputs {
		switch input.Kind {
		case "stdin":
			if len(input.Data) > 0 {
				data.Write(input.Data)
			} else {
				data.WriteString(input.Input)
			}
		case "stdin_close":
			closeEvents++
		}
	}
	return data.Bytes(), closeEvents
}
