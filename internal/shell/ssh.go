package shell

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyrange/vmsh/internal/terminal"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

var (
	sshConfigPaths = []string{"~/.ssh/config"}
	sshKnownHosts  = []string{"~/.ssh/known_hosts", "~/.ssh/known_hosts2", "/etc/ssh/ssh_known_hosts"}
)

type persistentSSHClient struct {
	key    string
	config resolvedSSHConfig
	client *ssh.Client
}

type persistentSSHShell struct {
	mu             sync.Mutex
	outputMu       sync.Mutex
	key            string
	name           string
	ctx            commandContext
	client         *persistentSSHClient
	session        *ssh.Session
	controlClient  *ssh.Client
	controlSession *ssh.Session
	stdin          io.WriteCloser
	stdout         *bufio.Reader
	control        *bufio.Reader
	output         io.Writer
	done           chan error
	controlDone    chan error
	lastCWD        string
}

type resolvedSSHConfig struct {
	Alias                 string
	HostName              string
	User                  string
	Port                  string
	IdentityAgent         string
	IdentityFiles         []string
	IdentitiesOnly        bool
	ProxyJump             string
	StrictHostKeyChecking string
	ConnectTimeout        time.Duration
}

func (s *shellState) runSSH(ctx commandContext, line string, stdout, stderr io.Writer) error {
	if sshContextsMatch(s.context, ctx) && persistentSSHCommandAllowed(line) {
		session, err := s.sshPersistentShell(ctx, stdout, stderr)
		if err == nil {
			var interrupted atomic.Bool
			interrupts := newCommandInterruptEscalator(line, stderr, func() {
				interrupted.Store(true)
				_ = session.session.Signal(ssh.SIGINT)
			}, func() {
				go func() {
					session.close()
				}()
			})
			stopInterrupts, _ := s.startInterruptWatcher(interrupts.Interrupt)
			err = session.run(line, stdout, stderr, interrupts.ForwardedInterrupt)
			stopInterrupts()
			if interrupted.Load() && sshPersistentShellEnded(err) {
				s.closeSSHSessionKey(session.key)
				s.lastCode = 130
				return nil
			}
			if sshPersistentShellEnded(err) {
				s.closeSSHSessionKey(session.key)
				s.lastCode = 1
				return nil
			}
			s.lastCode = sessionLastCode(err)
			if cwd := session.cwd(); cwd != "" {
				s.context.CWD = cwd
				s.rememberContextCWD(s.context)
			}
			if err == nil || s.lastCode >= 0 {
				return nil
			}
			return err
		}
	}
	return s.runSSHCommandWithSize(ctx, line, nil, stdout, stderr, false, 0, 0, true)
}

func (s *shellState) runSSHWithInput(ctx commandContext, line string, stdin io.Reader, stdout, stderr io.Writer) error {
	return s.runSSHCommand(ctx, line, stdin, stdout, stderr, false, false)
}

func (s *shellState) runSSHCommand(ctx commandContext, line string, stdin io.Reader, stdout, stderr io.Writer, tty, setLastCode bool) error {
	return s.runSSHCommandWithSize(ctx, line, stdin, stdout, stderr, tty, 0, 0, setLastCode)
}

func (s *shellState) runSSHCommandWithSize(ctx commandContext, line string, stdin io.Reader, stdout, stderr io.Writer, tty bool, cols, rows int, setLastCode bool) error {
	if ctx.CWD == "" {
		ctx.CWD = s.currentSSHCWD(ctx)
	}
	script := sshRemoteCommandScript(ctx, line)
	err := s.runSSHSession(ctx, "sh -lc "+shellQuote(script), stdin, stdout, stderr, tty, cols, rows)
	code := sshSessionExitCode(err)
	if setLastCode {
		s.lastCode = code
		if err != nil && code < 0 {
			return err
		}
		return nil
	}
	if err != nil && code < 0 {
		return err
	}
	if code != 0 {
		return persistentShellExit{code: code}
	}
	return nil
}

func (s *shellState) runSSHSession(ctx commandContext, command string, stdin io.Reader, stdout, stderr io.Writer, tty bool, cols, rows int) error {
	runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
	client, err := s.sshClientForContext(runCtx, ctx)
	if interrupted.Load() {
		stopInterrupts()
		return persistentShellExit{code: 130}
	}
	stopInterrupts()
	if err != nil {
		return err
	}
	session, err := client.client.NewSession()
	if err != nil {
		s.dropSSHClient(client.key)
		runCtx, stopInterrupts, interrupted = s.interruptibleCommandContext()
		client, err = s.sshClientForContext(runCtx, ctx)
		if interrupted.Load() {
			stopInterrupts()
			return persistentShellExit{code: 130}
		}
		stopInterrupts()
		if err != nil {
			return err
		}
		session, err = client.client.NewSession()
		if err != nil {
			return err
		}
	}
	defer session.Close()
	if tty {
		if cols <= 0 {
			cols = 80
		}
		if rows <= 0 {
			rows = 24
		}
		term := firstNonEmpty(os.Getenv("TERM"), "xterm-256color")
		modes := terminal.SSHTerminalModes(os.Stdin)
		modes[ssh.ECHO] = 1
		if err := session.RequestPty(term, rows, cols, modes); err != nil {
			return err
		}
	}
	setOpenSSHStyleEnv(session)
	if stdin != nil {
		session.Stdin = stdin
	} else if tty {
		session.Stdin = os.Stdin
	}
	if tty {
		session.Stdout = stdout
		session.Stderr = stderr
	} else {
		session.Stdout = terminalDisplayWriter(stdout)
		session.Stderr = terminalDisplayWriter(stderr)
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()
	interrupts := newCommandInterruptEscalator(command, stderr, func() {
		_ = session.Signal(ssh.SIGINT)
	}, func() {
		_ = session.Close()
	})
	stopRunInterrupts, runInterrupted := s.startInterruptWatcher(interrupts.Interrupt)
	defer stopRunInterrupts()
	for {
		select {
		case err := <-done:
			if runInterrupted.Load() {
				return persistentShellExit{code: 130}
			}
			return err
		}
	}
}

func persistentSSHCommandAllowed(line string) bool {
	return persistentShellCommandAllowed(line)
}

func (s *shellState) sshPersistentShell(ctx commandContext, output, stderr io.Writer) (*persistentSSHShell, error) {
	runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
	defer stopInterrupts()
	client, err := s.sshClientForContext(runCtx, ctx)
	if interrupted.Load() {
		return nil, persistentShellExit{code: 130}
	}
	if err != nil {
		return nil, wrapSSHStartupEOF(ctx, err)
	}
	key := persistentSSHShellKey(client.key, ctx)
	if shell := s.sshShellForKey(key); shell != nil {
		return shell, nil
	}
	shell, err := s.startPersistentSSHShell(runCtx, ctx, client, key, output, stderr)
	if interrupted.Load() {
		if shell != nil {
			shell.close()
		}
		return nil, persistentShellExit{code: 130}
	}
	if err != nil {
		if shell != nil {
			shell.close()
		}
		return nil, wrapSSHStartupEOF(ctx, err)
	}
	shell.ctx.CWD = shell.lastCWD
	s.sshMu.Lock()
	if s.sshShells == nil {
		s.sshShells = map[string]*persistentSSHShell{}
	}
	if existing := s.sshShells[key]; existing != nil {
		s.sshMu.Unlock()
		shell.close()
		return existing, nil
	}
	s.sshShells[key] = shell
	s.sshMu.Unlock()
	return shell, nil
}

func (s *shellState) startPersistentSSHShell(runCtx context.Context, ctx commandContext, client *persistentSSHClient, key string, output, stderr io.Writer) (*persistentSSHShell, error) {
	session, err := client.client.NewSession()
	if err != nil {
		s.dropSSHClient(client.key)
		reconnectCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
		client, err = s.sshClientForContext(reconnectCtx, ctx)
		stopInterrupts()
		if interrupted.Load() {
			return nil, persistentShellExit{code: 130}
		}
		if err != nil {
			return nil, wrapSSHStartupEOF(ctx, err)
		}
		session, err = client.client.NewSession()
		if err != nil {
			return nil, wrapSSHStartupEOF(ctx, err)
		}
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, wrapSSHStartupEOF(ctx, err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, wrapSSHStartupEOF(ctx, err)
	}
	if err := requestPersistentSSHPty(session, output); err != nil {
		_ = session.Close()
		return nil, wrapSSHStartupEOF(ctx, err)
	}
	session.Stderr = stderr
	done := make(chan error, 1)
	controlClient, err := s.dialSSHConfigContext(runCtx, client.config)
	if err != nil {
		_ = session.Close()
		return nil, wrapSSHStartupEOF(ctx, err)
	}
	controlSession, controlReader, controlDone, controlPath, err := startSSHControlSideband(controlClient, stderr)
	if err != nil {
		_ = session.Close()
		_ = controlClient.Close()
		return nil, wrapSSHStartupEOF(ctx, err)
	}
	shell := &persistentSSHShell{
		key:            key,
		name:           sshSessionDisplayName(ctx, client.config),
		ctx:            ctx,
		client:         client,
		session:        session,
		controlClient:  controlClient,
		controlSession: controlSession,
		stdin:          stdin,
		stdout:         bufio.NewReader(stdout),
		control:        controlReader,
		done:           done,
		controlDone:    controlDone,
		lastCWD:        s.currentSSHCWD(ctx),
	}
	setOpenSSHStyleEnv(session)
	script := sshPersistentShellSidebandScript(ctx, controlPath)
	if err := session.Start("sh -ic " + shellQuote(script)); err != nil {
		_ = session.Close()
		_ = controlSession.Close()
		_ = controlClient.Close()
		return nil, wrapSSHStartupEOF(ctx, err)
	}
	go func() {
		done <- session.Wait()
	}()
	go shell.forwardOutput()
	waitStop, _ := s.startInterruptWatcher(func() {
		_ = session.Signal(ssh.SIGINT)
		_ = session.Close()
	})
	err = shell.waitReady()
	waitStop()
	if err != nil {
		return shell, wrapSSHStartupEOF(ctx, err)
	}
	return shell, nil
}

func wrapSSHStartupEOF(ctx commandContext, err error) error {
	if err == nil || !errors.Is(err, io.EOF) {
		return err
	}
	host := strings.TrimSpace(ctx.SSHHost)
	if host == "" {
		host = "remote host"
	}
	return fmt.Errorf("ssh %s closed the connection before the shell was ready", host)
}

func requestPersistentSSHPty(session *ssh.Session, stdout io.Writer) error {
	_, cols, rows := terminalRequestSize(stdout)
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	term := firstNonEmpty(os.Getenv("TERM"), "xterm-256color")
	return session.RequestPty(term, rows, cols, terminal.SSHTerminalModes(os.Stdin))
}

func setOpenSSHStyleEnv(session *ssh.Session) {
	if session == nil {
		return
	}
	for _, env := range os.Environ() {
		name, value, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		if name != "LANG" && !strings.HasPrefix(name, "LC_") {
			continue
		}
		_ = session.Setenv(name, value)
	}
}

func persistentSSHShellKey(clientKey string, ctx commandContext) string {
	return clientKey
}

func startSSHControlSideband(client *ssh.Client, stderr io.Writer) (*ssh.Session, *bufio.Reader, chan error, string, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, nil, nil, "", err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, nil, nil, "", err
	}
	session.Stderr = stderr
	path := remoteControlFIFOPath()
	command := sshControlSidebandCommand(path)
	if err := session.Start("sh -lc " + shellQuote(command)); err != nil {
		_ = session.Close()
		return nil, nil, nil, "", err
	}
	reader := bufio.NewReader(stdout)
	done := make(chan error, 1)
	go func() {
		done <- session.Wait()
	}()
	if err := waitSSHControlSidebandReady(reader, done); err != nil {
		_ = session.Close()
		return nil, nil, nil, "", err
	}
	return session, reader, done, path, nil
}

func waitSSHControlSidebandReady(reader *bufio.Reader, done <-chan error) error {
	ready := make(chan error, 1)
	go func() {
		record, err := readPersistentControlRecord(reader)
		if err != nil {
			ready <- err
			return
		}
		if record.kind != "control-ready" {
			ready <- fmt.Errorf("unexpected ssh control record %q", record.kind)
			return
		}
		ready <- nil
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-ready:
		return err
	case err := <-done:
		if err != nil {
			return err
		}
		return fmt.Errorf("ssh control sideband exited before ready")
	case <-timer.C:
		return fmt.Errorf("ssh control sideband did not become ready")
	}
}

func remoteControlFIFOPath() string {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err == nil {
		return "/tmp/vmsh-control-" + hex.EncodeToString(nonce[:]) + ".fifo"
	}
	return fmt.Sprintf("/tmp/vmsh-control-%d.fifo", time.Now().UnixNano())
}

func sshControlSidebandCommand(path string) string {
	quoted := shellQuote(path)
	return strings.Join([]string{
		"__vmsh_control_path=" + quoted,
		"rm -f \"$__vmsh_control_path\"",
		"(umask 077 && mkfifo \"$__vmsh_control_path\") || exit 125",
		"trap 'rm -f \"$__vmsh_control_path\"' EXIT HUP INT TERM",
		"printf 'control-ready\\t0\\t%s\\n' \"$PWD\"",
		"cat \"$__vmsh_control_path\"",
	}, "\n")
}

func sshContextsMatch(a, b commandContext) bool {
	if a.Mode != modeSSH || b.Mode != modeSSH {
		return false
	}
	aCfg, err := resolveSSHConfig(a)
	if err != nil {
		return false
	}
	bCfg, err := resolveSSHConfig(b)
	if err != nil {
		return false
	}
	return aCfg.cacheKey() == bCfg.cacheKey()
}

func sshPersistentShellMatchesContext(shell *persistentSSHShell, ctx commandContext) bool {
	if shell == nil || ctx.Mode != modeSSH {
		return false
	}
	cfg, err := resolveSSHConfig(ctx)
	if err != nil {
		return false
	}
	return shell.key == persistentSSHShellKey(cfg.cacheKey(), ctx)
}

func sshPersistentShellSidebandScript(ctx commandContext, controlPath string) string {
	var lines []string
	cwd := strings.TrimSpace(ctx.CWD)
	if cwd != "" && cwd != "~" {
		lines = append(lines, remoteCDCommand(cwd)+" || exit")
	}
	lines = append(lines,
		"PS1= PS2= PS4=",
		"set -m 2>/dev/null || true",
		"stty -echo 2>/dev/null || true",
		sshColorPrelude(),
		"__vmsh_control_path="+shellQuote(controlPath),
		"exec 3>\"$__vmsh_control_path\" || exit",
		"__vmsh_report() {",
		"  printf '%s\\t%s\\t%s\\n' \"$1\" \"$2\" \"$PWD\" >&3",
		"}",
		"__vmsh_run() {",
		"  stty echo 2>/dev/null || true",
		"  eval \"$1\" 2>&1",
		"  __vmsh_status=$?",
		"  stty -echo 2>/dev/null || true",
		"  __vmsh_report done \"$__vmsh_status\"",
		"}",
		"__vmsh_report ready 0",
		"while IFS= read -r __vmsh_line; do eval \"$__vmsh_line\"; done",
	)
	return strings.Join(lines, "\n")
}

func sshColorPrelude() string {
	return strings.Join([]string{
		"if command ls --color=always -C -w ${COLUMNS:-80} >/dev/null 2>&1; then",
		"  ls() { command ls --color=always -C -w \"${COLUMNS:-80}\" \"$@\"; }",
		"elif command ls -G -C >/dev/null 2>&1; then",
		"  ls() { command ls -G -C \"$@\"; }",
		"fi",
	}, "\n")
}

func sshPersistentShellEnded(err error) bool {
	return errors.Is(err, io.EOF)
}

func (p *persistentSSHShell) waitReady() error {
	ready := make(chan error, 1)
	go func() {
		ready <- p.waitReadySideband()
	}()
	timer := time.NewTimer(defaultGuestShellReadyTimeout)
	defer timer.Stop()
	select {
	case err := <-ready:
		return err
	case err := <-p.done:
		if err != nil {
			return err
		}
		return fmt.Errorf("persistent ssh shell exited before ready")
	case <-timer.C:
		return fmt.Errorf("persistent ssh shell did not become ready")
	}
}

func (p *persistentSSHShell) waitReadySideband() error {
	for {
		record, err := readPersistentControlRecord(p.control)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("persistent ssh control sideband closed before ready")
			}
			return err
		}
		if record.kind == "ready" {
			p.lastCWD = record.cwd
			return nil
		}
	}
}

func (p *persistentSSHShell) run(line string, stdout, stderr io.Writer, onInterrupt ...func()) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runSideband(line, stdout, stderr, onInterrupt...)
}

func (p *persistentSSHShell) runSideband(line string, stdout, stderr io.Writer, onInterrupt ...func()) error {
	p.setOutput(stdout)
	stopForwarding := func() {}
	defer func() {
		p.clearOutputSoon()
		stopForwarding()
	}()
	stop, err := startSSHPTYForwarding(p, true, stdout, stderr, onInterrupt...)
	if err != nil {
		return err
	}
	if stop != nil {
		stopForwarding = stop
	}
	if _, err := fmt.Fprintln(p.stdin, "__vmsh_run "+shellQuote(line)); err != nil {
		return err
	}
	for {
		record, err := readPersistentControlRecord(p.control)
		if err != nil {
			return err
		}
		if record.kind != "done" {
			continue
		}
		p.lastCWD = record.cwd
		p.ctx.CWD = record.cwd
		if record.code != 0 {
			return persistentShellExit{code: record.code}
		}
		return nil
	}
}

func (p *persistentSSHShell) forwardOutput() {
	buf := make([]byte, 4096)
	for {
		n, err := p.stdout.Read(buf)
		if n > 0 {
			p.outputMu.Lock()
			output := p.output
			if output != nil {
				writePTYOutput(output, buf[:n])
			}
			p.outputMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *persistentSSHShell) setOutput(output io.Writer) {
	p.outputMu.Lock()
	p.output = output
	p.outputMu.Unlock()
}

func (p *persistentSSHShell) clearOutputSoon() {
	time.Sleep(20 * time.Millisecond)
	p.setOutput(nil)
}

func startSSHPTYForwarding(p *persistentSSHShell, tty bool, stdout, stderr io.Writer, onInterrupt ...func()) (func(), error) {
	done := make(chan struct{})
	restore := func() {}
	cancelRead := func() {}
	var producers sync.WaitGroup
	if tty {
		resizeSSHPTY(p, stdout)
	}
	if tty {
		file, ok := terminalWriterFile(stdout)
		if ok && terminal.IsTerminalFD(int(file.Fd())) && terminal.IsTerminalFD(int(os.Stdin.Fd())) {
			terminalRestore, err := terminal.MakeAttachedRaw(os.Stdin)
			if err != nil {
				return nil, err
			}
			inputCancel, err := newPTYInputCanceller(os.Stdin)
			if err != nil {
				terminalRestore()
				return nil, err
			}
			restore = terminalRestore
			cancelRead = inputCancel.cancel
			producers.Add(1)
			go func() {
				defer producers.Done()
				defer inputCancel.close()
				streamSSHPTYStdin(os.Stdin, p.stdin, done, inputCancel, terminalWriterRecorder(stdout), onInterrupt...)
			}()
		}
	}

	producers.Add(1)
	go func() {
		defer producers.Done()
		forwardSSHPTYSignals(p, done, tty, stdout, stderr)
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			close(done)
			cancelRead()
			producers.Wait()
			restore()
		})
	}
	return stop, nil
}

func streamSSHPTYStdin(in *os.File, out io.Writer, done <-chan struct{}, inputCancel *ptyInputCanceller, recorder *asciinemaRecorder, onInterrupt ...func()) {
	var buf [4096]byte
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := readPTYInput(in, buf[:], done, inputCancel)
		if n > 0 {
			if recorder != nil {
				recorder.recordInput(buf[:n])
			}
			if bytes.Contains(buf[:n], []byte{0x03}) {
				for _, fn := range onInterrupt {
					if fn != nil {
						fn()
					}
				}
			}
			if !writeSSHPTYInput(out, done, buf[:n]) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func writeSSHPTYInput(out io.Writer, done <-chan struct{}, data []byte) bool {
	for len(data) > 0 {
		select {
		case <-done:
			return false
		default:
		}
		n, err := out.Write(data)
		if err != nil {
			return false
		}
		if n <= 0 {
			return false
		}
		data = data[n:]
	}
	return true
}

func forwardSSHPTYSignals(p *persistentSSHShell, done <-chan struct{}, tty bool, stdout, stderr io.Writer) {
	signals := terminal.HostSignals(tty)
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, signals...)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-done:
			return
		case sig := <-sigCh:
			if sig == nil {
				continue
			}
			if terminal.IsResizeSignal(sig) {
				resizeSSHPTY(p, stdout)
				continue
			}
			name, ok := terminal.SignalName(sig)
			if !ok {
				continue
			}
			if name == "INT" {
				fmt.Fprintln(stderr)
			}
			writeSSHPTYSignal(p, name)
		}
	}
}

func resizeSSHPTY(p *persistentSSHShell, stdout io.Writer) {
	file, ok := terminalWriterFile(stdout)
	if !ok {
		return
	}
	cols, rows, err := terminal.Size(file)
	if err != nil || cols <= 0 || rows <= 0 {
		return
	}
	_ = p.session.WindowChange(rows, cols)
}

func writeSSHPTYSignal(p *persistentSSHShell, name string) {
	switch name {
	case "INT":
		_, _ = p.stdin.Write([]byte{0x03})
	case "QUIT":
		_, _ = p.stdin.Write([]byte{0x1c})
	}
}

func (p *persistentSSHShell) cwd() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastCWD
}

func (p *persistentSSHShell) close() {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.session != nil {
		_ = p.session.Close()
	}
	if p.controlSession != nil {
		_ = p.controlSession.Close()
	}
	if p.controlClient != nil {
		_ = p.controlClient.Close()
	}
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
	if p.controlDone != nil {
		select {
		case <-p.controlDone:
		case <-time.After(2 * time.Second):
		}
	}
}

func sshSessionExitCode(err error) int {
	if err == nil {
		return 0
	}
	var persistentExit persistentShellExit
	if errors.As(err, &persistentExit) {
		return persistentExit.code
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus()
	}
	return -1
}

func (s *shellState) sshClientFor(ctx commandContext) (*persistentSSHClient, error) {
	return s.sshClientForContext(context.Background(), ctx)
}

func (s *shellState) sshClientForContext(runCtx context.Context, ctx commandContext) (*persistentSSHClient, error) {
	cfg, err := resolveSSHConfig(ctx)
	if err != nil {
		return nil, err
	}
	key := cfg.cacheKey()
	s.sshMu.Lock()
	if s.sshClients != nil {
		if client := s.sshClients[key]; client != nil {
			s.sshMu.Unlock()
			return client, nil
		}
	}
	s.sshMu.Unlock()

	client, err := s.dialSSHConfigContext(runCtx, cfg)
	if err != nil {
		return nil, err
	}
	persistent := &persistentSSHClient{key: key, config: cfg, client: client}
	s.sshMu.Lock()
	if s.sshClients == nil {
		s.sshClients = map[string]*persistentSSHClient{}
	}
	if existing := s.sshClients[key]; existing != nil {
		s.sshMu.Unlock()
		_ = client.Close()
		return existing, nil
	}
	s.sshClients[key] = persistent
	s.sshMu.Unlock()
	return persistent, nil
}

func (s *shellState) hasSSHClient(ctx commandContext) bool {
	cfg, err := resolveSSHConfig(ctx)
	if err != nil {
		return false
	}
	key := cfg.cacheKey()
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	return s.sshClients != nil && s.sshClients[key] != nil
}

func (s *shellState) sshShellForContext(ctx commandContext) *persistentSSHShell {
	cfg, err := resolveSSHConfig(ctx)
	if err != nil {
		return nil
	}
	return s.sshShellForKey(persistentSSHShellKey(cfg.cacheKey(), ctx))
}

func (s *shellState) sshShellForKey(key string) *persistentSSHShell {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	if s.sshShells == nil {
		return nil
	}
	return s.sshShells[key]
}

func (s *shellState) sshSessionNames() []string {
	shells := s.sshShellList()
	names := make([]string, 0, len(shells))
	for _, shell := range shells {
		names = append(names, shell.name)
	}
	sort.Strings(names)
	return uniqueStrings(names)
}

func (s *shellState) sshSessionContext(name string) (commandContext, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return commandContext{}, false
	}
	s.sshMu.Lock()
	var match *persistentSSHShell
	for _, shell := range s.sshShells {
		if shell.name == name || shell.ctx.SSHHost == name {
			match = shell
			break
		}
	}
	s.sshMu.Unlock()
	if match == nil {
		return commandContext{}, false
	}
	ctx := match.ctx
	if cwd := match.cwd(); cwd != "" {
		ctx.CWD = cwd
	}
	return ctx, true
}

type sshSessionState struct {
	Name string
	User string
	CWD  string
}

func (s *shellState) sshSessionStates() []sshSessionState {
	shells := s.sshShellList()
	states := make([]sshSessionState, 0, len(shells))
	for _, shell := range shells {
		states = append(states, sshSessionState{
			Name: shell.name,
			User: shell.client.config.User,
			CWD:  shell.cwd(),
		})
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	return states
}

func (s *shellState) sshShellList() []*persistentSSHShell {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	if len(s.sshShells) == 0 {
		return nil
	}
	shells := make([]*persistentSSHShell, 0, len(s.sshShells))
	for _, shell := range s.sshShells {
		shells = append(shells, shell)
	}
	return shells
}

func (s *shellState) dialSSHConfig(cfg resolvedSSHConfig) (*ssh.Client, error) {
	return s.dialSSHConfigContext(context.Background(), cfg)
}

func (s *shellState) dialSSHConfigContext(ctx context.Context, cfg resolvedSSHConfig) (*ssh.Client, error) {
	clientConfig, closers, err := s.sshClientConfig(cfg)
	if err != nil {
		return nil, err
	}
	defer closeAll(closers)
	addr := net.JoinHostPort(cfg.HostName, cfg.Port)
	if cfg.ProxyJump != "" {
		jump, err := s.sshClientForContext(ctx, commandContext{Mode: modeSSH, SSHHost: cfg.ProxyJump})
		if err != nil {
			return nil, err
		}
		conn, err := jump.client.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return ssh.NewClient(clientConn, chans, reqs), nil
	}
	dialer := net.Dialer{Timeout: cfg.ConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(clientConn, chans, reqs), nil
}

func (s *shellState) dropSSHClient(key string) {
	s.closeSSHSessionKey(key)
}

func (s *shellState) closeSSHClients() {
	s.sshMu.Lock()
	shells := s.sshShells
	s.sshShells = nil
	clients := s.sshClients
	s.sshClients = nil
	s.sshMu.Unlock()
	for _, shell := range shells {
		shell.close()
	}
	for _, client := range clients {
		_ = client.client.Close()
	}
}

func (s *shellState) stopSSHSessionForContext(ctx commandContext) bool {
	cfg, err := resolveSSHConfig(ctx)
	if err != nil {
		return false
	}
	return s.closeSSHSessionKey(cfg.cacheKey())
}

func (s *shellState) stopSSHSession(name string) bool {
	key, ok := s.sshSessionKeyForName(name)
	if !ok {
		return false
	}
	return s.closeSSHSessionKey(key)
}

func (s *shellState) sshSessionKeyForName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if cfg, err := resolveSSHConfig(commandContext{Mode: modeSSH, SSHHost: name}); err == nil {
		key := cfg.cacheKey()
		if s.hasSSHSessionKey(key) {
			return key, true
		}
	}
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	for key, shell := range s.sshShells {
		if shell.name == name || shell.ctx.SSHHost == name {
			return key, true
		}
	}
	for key, client := range s.sshClients {
		cfg := client.config
		if cfg.Alias == name || cfg.HostName == name || sshSessionDisplayName(commandContext{Mode: modeSSH, SSHHost: cfg.Alias}, cfg) == name {
			return key, true
		}
	}
	return "", false
}

func (s *shellState) hasSSHSessionKey(key string) bool {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	return (s.sshShells != nil && s.sshShells[key] != nil) || (s.sshClients != nil && s.sshClients[key] != nil)
}

func (s *shellState) closeSSHSessionKey(key string) bool {
	s.sshMu.Lock()
	var shell *persistentSSHShell
	if s.sshShells != nil {
		shell = s.sshShells[key]
		delete(s.sshShells, key)
	}
	var client *persistentSSHClient
	if s.sshClients != nil {
		client = s.sshClients[key]
		delete(s.sshClients, key)
	}
	s.sshMu.Unlock()
	if shell != nil {
		shell.close()
	}
	if client != nil {
		_ = client.client.Close()
	}
	return shell != nil || client != nil
}

func parseExplicitSSHStopTarget(name string) (bool, string) {
	name = strings.TrimSpace(name)
	for _, prefix := range []string{"@ssh:", "ssh:"} {
		if strings.HasPrefix(name, prefix) {
			return true, strings.TrimSpace(strings.TrimPrefix(name, prefix))
		}
	}
	return false, name
}

func sshSessionDisplayName(ctx commandContext, cfg resolvedSSHConfig) string {
	if strings.TrimSpace(ctx.SSHHost) != "" {
		return strings.TrimSpace(ctx.SSHHost)
	}
	if cfg.Alias != "" {
		return cfg.Alias
	}
	if cfg.User != "" && cfg.HostName != "" {
		return cfg.User + "@" + cfg.HostName
	}
	return cfg.HostName
}

func (s *shellState) sshClientConfig(cfg resolvedSSHConfig) (*ssh.ClientConfig, []io.Closer, error) {
	callback, err := s.sshHostKeyCallback(cfg)
	if err != nil {
		return nil, nil, err
	}
	auth, closers := sshAuthMethods(cfg, s.sshPassword, s.sshKeyboardAuth)
	config := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: callback,
		Timeout:         cfg.ConnectTimeout,
	}
	if s.sshBanner != nil {
		config.BannerCallback = func(message string) error {
			return s.sshBanner(cfg, message)
		}
	}
	return config, closers, nil
}

func (s *shellState) sshHostKeyCallback(cfg resolvedSSHConfig) (ssh.HostKeyCallback, error) {
	switch strings.ToLower(cfg.StrictHostKeyChecking) {
	case "no", "off":
		return ssh.InsecureIgnoreHostKey(), nil
	}
	var files []string
	for _, raw := range sshKnownHosts {
		file := expandUserPath(raw)
		if _, err := os.Stat(file); err == nil {
			files = append(files, file)
		}
	}
	callback, err := knownhosts.New(files...)
	if err != nil {
		return nil, err
	}
	strict := strings.ToLower(cfg.StrictHostKeyChecking)
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := callback(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) || len(keyErr.Want) != 0 {
			return err
		}
		switch strict {
		case "yes", "on":
			return err
		case "accept-new":
			return addSSHKnownHost(hostname, key)
		}
		if s.confirmSSHHost == nil {
			return err
		}
		ok, confirmErr := s.confirmSSHHost(cfg, hostname, remote, key)
		if confirmErr != nil {
			return confirmErr
		}
		if !ok {
			return fmt.Errorf("ssh host key rejected for %s", hostname)
		}
		return addSSHKnownHost(hostname, key)
	}, nil
}

func addSSHKnownHost(hostname string, key ssh.PublicKey) error {
	file := sshUserKnownHostsPath()
	if file == "" {
		return fmt.Errorf("no writable SSH known_hosts path configured")
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, knownhosts.Line([]string{hostname}, key))
	return err
}

func sshUserKnownHostsPath() string {
	for _, raw := range sshKnownHosts {
		file := expandUserPath(raw)
		if file == "" || strings.HasPrefix(file, "/etc/") {
			continue
		}
		return file
	}
	return ""
}

func sshAuthMethods(cfg resolvedSSHConfig, password func(resolvedSSHConfig) (string, error), keyboard func(resolvedSSHConfig, string, string, []string, []bool) ([]string, error)) ([]ssh.AuthMethod, []io.Closer) {
	var methods []ssh.AuthMethod
	var closers []io.Closer
	agentPath := firstNonEmpty(cfg.IdentityAgent, os.Getenv("SSH_AUTH_SOCK"))
	if agentPath != "" && strings.ToLower(agentPath) != "none" {
		agentPath = expandSSHConfigValue(agentPath, cfg)
		if conn, err := net.Dial("unix", agentPath); err == nil {
			client := agent.NewClient(conn)
			if signers, err := client.Signers(); err == nil && len(signers) > 0 {
				closers = append(closers, conn)
				methods = append(methods, ssh.PublicKeys(signers...))
			} else {
				_ = conn.Close()
			}
		}
	}
	for _, identity := range cfg.IdentityFiles {
		key, err := os.ReadFile(identity)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if password != nil {
		getPassword := sshCachedPassword(cfg, password)
		if keyboard == nil {
			keyboard = func(cfg resolvedSSHConfig, name, instruction string, questions []string, echos []bool) ([]string, error) {
				return sshPasswordKeyboardAnswers(questions, echos, getPassword)
			}
		}
	}
	if keyboard != nil {
		methods = append(methods, ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
			return keyboard(cfg, name, instruction, questions, echos)
		}))
	}
	if password != nil {
		methods = append(methods, ssh.PasswordCallback(sshCachedPassword(cfg, password)))
	}
	return methods, closers
}

func sshPasswordKeyboardAnswers(questions []string, echos []bool, getPassword func() (string, error)) ([]string, error) {
	answers := make([]string, 0, len(questions))
	for i := range questions {
		echo := false
		if i < len(echos) {
			echo = echos[i]
		}
		if echo {
			return nil, fmt.Errorf("unsupported keyboard-interactive SSH challenge")
		}
		answer, err := getPassword()
		if err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	return answers, nil
}

func sshCachedPassword(cfg resolvedSSHConfig, password func(resolvedSSHConfig) (string, error)) func() (string, error) {
	var mu sync.Mutex
	var cached string
	var ok bool
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if ok {
			return cached, nil
		}
		value, err := password(cfg)
		if err != nil {
			return "", err
		}
		cached = value
		ok = true
		return cached, nil
	}
}

func closeAll(closers []io.Closer) {
	for _, closer := range closers {
		_ = closer.Close()
	}
}

func (cfg resolvedSSHConfig) cacheKey() string {
	return strings.Join([]string{cfg.User, cfg.HostName, cfg.Port, cfg.ProxyJump}, "\x00")
}

func resolveSSHConfig(ctx commandContext) (resolvedSSHConfig, error) {
	rawHost := strings.TrimSpace(ctx.SSHHost)
	if rawHost == "" {
		return resolvedSSHConfig{}, fmt.Errorf("ssh host is required")
	}
	inlineUser, alias := splitSSHUserHost(rawHost)
	cfg := resolvedSSHConfig{
		Alias:          alias,
		HostName:       alias,
		Port:           "22",
		ConnectTimeout: 15 * time.Second,
	}
	for _, raw := range sshConfigPaths {
		_ = applySSHConfigFile(expandUserPath(raw), alias, &cfg, map[string]bool{})
	}
	if cfg.HostName == "" {
		cfg.HostName = alias
	}
	switch {
	case strings.TrimSpace(ctx.User) != "":
		cfg.User = strings.TrimSpace(ctx.User)
	case inlineUser != "":
		cfg.User = inlineUser
	case cfg.User == "":
		cfg.User = defaultSSHUser()
	}
	if cfg.Port == "" {
		cfg.Port = "22"
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	for i, identity := range cfg.IdentityFiles {
		cfg.IdentityFiles[i] = expandSSHConfigValue(identity, cfg)
	}
	if len(cfg.IdentityFiles) == 0 && !cfg.IdentitiesOnly {
		cfg.IdentityFiles = defaultSSHIdentityFiles()
	}
	cfg.HostName = expandSSHConfigValue(cfg.HostName, cfg)
	cfg.ProxyJump = expandSSHConfigValue(cfg.ProxyJump, cfg)
	return cfg, nil
}

func sshHostAliases() []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range sshConfigPaths {
		for _, alias := range sshHostAliasesFromConfig(expandUserPath(raw), map[string]bool{}) {
			if alias == "" || strings.ContainsAny(alias, "*?!") || seen[alias] {
				continue
			}
			seen[alias] = true
			out = append(out, alias)
		}
	}
	sort.Strings(out)
	return out
}

func sshHostAliasesFromConfig(file string, seen map[string]bool) []string {
	if seen[file] {
		return nil
	}
	seen[file] = true
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var aliases []string
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(trimSSHConfigComment(rawLine))
		if line == "" {
			continue
		}
		tokens, err := lexShellTokens(line)
		if err != nil || len(tokens) == 0 {
			continue
		}
		key := strings.ToLower(tokens[0].Value)
		values := tokenValues(tokens[1:])
		switch key {
		case "host":
			aliases = append(aliases, values...)
		case "include":
			for _, include := range values {
				for _, includeFile := range expandSSHInclude(include) {
					aliases = append(aliases, sshHostAliasesFromConfig(includeFile, seen)...)
				}
			}
		}
	}
	return aliases
}

func splitSSHUserHost(raw string) (string, string) {
	if userPart, hostPart, ok := strings.Cut(raw, "@"); ok && userPart != "" && hostPart != "" {
		return userPart, hostPart
	}
	return "", raw
}

func defaultSSHUser() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		if idx := strings.LastIndexAny(current.Username, `\`); idx >= 0 {
			return current.Username[idx+1:]
		}
		return current.Username
	}
	return os.Getenv("USER")
}

func defaultSSHIdentityFiles() []string {
	var out []string
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		path := filepath.Join(expandUserPath("~/.ssh"), name)
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

func applySSHConfigFile(file, alias string, cfg *resolvedSSHConfig, seen map[string]bool) error {
	if seen[file] {
		return nil
	}
	seen[file] = true
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	active := true
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(trimSSHConfigComment(rawLine))
		if line == "" {
			continue
		}
		tokens, err := lexShellTokens(line)
		if err != nil || len(tokens) == 0 {
			continue
		}
		key := strings.ToLower(tokens[0].Value)
		values := tokenValues(tokens[1:])
		switch key {
		case "host":
			active = sshHostPatternsMatch(values, alias)
			continue
		case "include":
			if active {
				for _, include := range values {
					for _, includeFile := range expandSSHInclude(include) {
						_ = applySSHConfigFile(includeFile, alias, cfg, seen)
					}
				}
			}
			continue
		}
		if !active || len(values) == 0 {
			continue
		}
		applySSHConfigOption(cfg, key, values)
	}
	return nil
}

func applySSHConfigOption(cfg *resolvedSSHConfig, key string, values []string) {
	value := strings.Join(values, " ")
	switch key {
	case "hostname":
		if cfg.HostName == "" || cfg.HostName == cfg.Alias {
			cfg.HostName = value
		}
	case "user":
		if cfg.User == "" {
			cfg.User = value
		}
	case "port":
		if cfg.Port == "" || cfg.Port == "22" {
			cfg.Port = value
		}
	case "identityfile":
		cfg.IdentityFiles = append(cfg.IdentityFiles, values...)
	case "identityagent":
		if cfg.IdentityAgent == "" {
			cfg.IdentityAgent = value
		}
	case "identitiesonly":
		if parseSSHBool(value) {
			cfg.IdentitiesOnly = true
		}
	case "proxyjump":
		if cfg.ProxyJump == "" && strings.ToLower(value) != "none" {
			cfg.ProxyJump = strings.Split(value, ",")[0]
		}
	case "stricthostkeychecking":
		if cfg.StrictHostKeyChecking == "" {
			cfg.StrictHostKeyChecking = value
		}
	case "connecttimeout":
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			cfg.ConnectTimeout = time.Duration(seconds) * time.Second
		}
	}
}

func tokenValues(tokens []shellToken) []string {
	values := make([]string, 0, len(tokens))
	for _, token := range tokens {
		values = append(values, token.Value)
	}
	return values
}

func trimSSHConfigComment(line string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '#' && !inSingle && !inDouble:
			return line[:i]
		}
	}
	return line
}

func sshHostPatternsMatch(patterns []string, host string) bool {
	host = strings.ToLower(host)
	matched := false
	for _, pattern := range patterns {
		negated := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		ok := sshHostPatternMatch(pattern, host)
		if negated && ok {
			return false
		}
		if ok {
			matched = true
		}
	}
	return matched
}

func sshHostPatternMatch(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	if ok, err := path.Match(pattern, host); err == nil && ok {
		return true
	}
	return pattern == host
}

func expandSSHInclude(pattern string) []string {
	pattern = expandUserPath(pattern)
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return []string{pattern}
	}
	return matches
}

func expandUserPath(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if value == "~" {
				return home
			}
			return filepath.Join(home, value[2:])
		}
	}
	return value
}

func expandSSHConfigValue(value string, cfg resolvedSSHConfig) string {
	value = strings.ReplaceAll(value, "%n", cfg.Alias)
	value = strings.ReplaceAll(value, "%h", cfg.HostName)
	value = strings.ReplaceAll(value, "%p", cfg.Port)
	value = strings.ReplaceAll(value, "%r", cfg.User)
	return expandUserPath(value)
}

func parseSSHBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "yes", "true", "on":
		return true
	default:
		return false
	}
}

func (s *shellState) copyLocalToSSH(src string, ctx commandContext, dst copyTargetPath, stderr io.Writer) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	remoteDir := dst.path
	rootName := filepath.Base(src)
	if !dst.directory {
		remoteDir = path.Dir(dst.path)
		rootName = path.Base(dst.path)
	}
	if remoteDir == "." {
		remoteDir = s.currentSSHCWD(ctx)
	}
	pr, pw := io.Pipe()
	go func() {
		err := writePathTar(pw, src, rootName)
		_ = pw.CloseWithError(err)
	}()
	command := remoteMkdirCommand(remoteDir) + " && " + remoteCDCommand(remoteDir) + " && tar -xf -"
	err := s.runSSHCommand(ctx, command, pr, io.Discard, stderr, false, false)
	if err != nil {
		return err
	}
	return nil
}

func (s *shellState) copySSHToLocal(ctx commandContext, src copyTargetPath, dst string, stderr io.Writer) error {
	remoteDir := path.Dir(src.path)
	base := path.Base(src.path)
	if remoteDir == "." {
		remoteDir = s.currentSSHCWD(ctx)
	}
	var archive bytes.Buffer
	command := remoteCDCommand(remoteDir) + " && tar -cf - " + shellQuote(base)
	if err := s.runSSHCommand(ctx, command, nil, &archive, stderr, false, false); err != nil {
		return err
	}
	return extractTarToHost(bytes.NewReader(archive.Bytes()), dst, false)
}

func remoteMkdirCommand(dir string) string {
	dir = strings.TrimSpace(dir)
	switch {
	case dir == "" || dir == "~" || dir == ".":
		return ":"
	case strings.HasPrefix(dir, "~/"):
		return "mkdir -p \"$HOME\"/" + shellQuote(strings.TrimPrefix(dir, "~/"))
	default:
		return "mkdir -p " + shellQuote(dir)
	}
}
