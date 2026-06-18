package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

type demoOptions struct {
	out      string
	raw      string
	gif      string
	keepRaw  bool
	noGIF    bool
	live     bool
	timeout  time.Duration
	vmImage  string
	vmMemory string
}

func runDemo(p paths, args []string) error {
	opts, err := parseDemoArgs(p, args)
	if err != nil {
		if errors.Is(err, errDemoHelp) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.out), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.raw), 0o755); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workRoot, err := makeDemoWorkRoot()
	if err != nil {
		return err
	}
	defer os.RemoveAll(workRoot)
	hostWork := filepath.Join(workRoot, "h")
	if err := os.MkdirAll(hostWork, 0o755); err != nil {
		return err
	}

	sshSrv, err := startDemoSSHServer(ctx, workRoot)
	if err != nil {
		return err
	}
	defer sshSrv.Close()
	if err := writeDemoSSHConfig(workRoot, sshSrv.Addr()); err != nil {
		return err
	}

	logf("demo: recording raw session to %s", opts.raw)
	if err := driveDemoSession(p, opts, workRoot, hostWork); err != nil {
		return err
	}
	if err := redactDemoCast(opts.raw, opts.out, p, sshSrv.Addr()); err != nil {
		return err
	}
	if !opts.keepRaw {
		_ = os.Remove(opts.raw)
	}
	logf("demo: wrote cast %s", opts.out)

	if !opts.noGIF {
		if err := renderDemoGIF(opts.out, opts.gif); err != nil {
			logf("demo: skipped gif render: %v", err)
		}
	}
	return nil
}

func makeDemoWorkRoot() (string, error) {
	for _, base := range []string{"/tmp", os.TempDir()} {
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			continue
		}
		dir, err := os.MkdirTemp(base, "d")
		if err == nil {
			return dir, nil
		}
	}
	return os.MkdirTemp("", "d")
}

func parseDemoArgs(p paths, args []string) (demoOptions, error) {
	opts := demoOptions{
		out:      filepath.Join(p.build, "demo.cast"),
		raw:      filepath.Join(p.build, "demo.raw.cast"),
		gif:      filepath.Join(p.build, "demo.gif"),
		timeout:  8 * time.Minute,
		vmImage:  "alpine",
		vmMemory: "768m",
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch {
		case arg == "--out":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.out = resolveDemoPath(p.root, v)
		case strings.HasPrefix(arg, "--out="):
			opts.out = resolveDemoPath(p.root, strings.TrimPrefix(arg, "--out="))
		case arg == "--raw":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.raw = resolveDemoPath(p.root, v)
		case strings.HasPrefix(arg, "--raw="):
			opts.raw = resolveDemoPath(p.root, strings.TrimPrefix(arg, "--raw="))
		case arg == "--gif":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.gif = resolveDemoPath(p.root, v)
		case strings.HasPrefix(arg, "--gif="):
			opts.gif = resolveDemoPath(p.root, strings.TrimPrefix(arg, "--gif="))
		case arg == "--vm-image":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.vmImage = strings.TrimSpace(v)
		case strings.HasPrefix(arg, "--vm-image="):
			opts.vmImage = strings.TrimSpace(strings.TrimPrefix(arg, "--vm-image="))
		case arg == "--memory":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.vmMemory = strings.TrimSpace(v)
		case strings.HasPrefix(arg, "--memory="):
			opts.vmMemory = strings.TrimSpace(strings.TrimPrefix(arg, "--memory="))
		case arg == "--timeout":
			v, err := next()
			if err != nil {
				return opts, err
			}
			d, err := time.ParseDuration(v)
			if err != nil {
				return opts, fmt.Errorf("invalid --timeout %q: %w", v, err)
			}
			opts.timeout = d
		case strings.HasPrefix(arg, "--timeout="):
			v := strings.TrimPrefix(arg, "--timeout=")
			d, err := time.ParseDuration(v)
			if err != nil {
				return opts, fmt.Errorf("invalid --timeout %q: %w", v, err)
			}
			opts.timeout = d
		case arg == "--keep-raw":
			opts.keepRaw = true
		case arg == "--no-gif":
			opts.noGIF = true
		case arg == "--live":
			opts.live = true
		case arg == "-h" || arg == "--help":
			printDemoUsage(os.Stderr)
			return opts, errDemoHelp
		default:
			return opts, fmt.Errorf("unknown demo argument %q", arg)
		}
	}
	if strings.TrimSpace(opts.vmImage) == "" {
		return opts, fmt.Errorf("--vm-image must not be empty")
	}
	if strings.TrimSpace(opts.vmMemory) == "" {
		return opts, fmt.Errorf("--memory must not be empty")
	}
	return opts, nil
}

var errDemoHelp = errors.New("demo help requested")

func demoWantsHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help")
}

func printDemoUsage(w io.Writer) {
	fmt.Fprint(w, `usage:
  ./tools/build.go demo [--out FILE] [--gif FILE] [--no-gif] [--live]

Options:
  --out FILE          write redacted asciinema cast (default build/vmsh/demo.cast)
  --raw FILE          write temporary unredacted cast (default build/vmsh/demo.raw.cast)
  --gif FILE          render GIF with agg when available (default build/vmsh/demo.gif)
  --no-gif            skip optional GIF rendering
  --keep-raw          keep the unredacted raw cast for local debugging
  --live              mirror the driven vmsh session to this terminal
  --vm-image IMAGE    VM image to use for the demo (default alpine)
  --memory SIZE       VM memory size for the demo VM (default 768m)
  --timeout DURATION  whole demo timeout (default 8m)
`)
}

func resolveDemoPath(root, value string) string {
	value = os.ExpandEnv(strings.TrimSpace(value))
	if value == "" {
		return value
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(root, value)
}

func defaultDemoCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return filepath.Join(os.TempDir(), "ccx3")
	}
	return filepath.Join(dir, "ccx3")
}

func driveDemoSession(p paths, opts demoOptions, workRoot, hostWork string) error {
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	args := []string{"-ccvm", p.ccvm, "-cache-dir", defaultDemoCacheDir(), "-record", opts.raw}
	cmd := exec.CommandContext(ctx, p.vmsh, args...)
	cmd.Dir = p.root
	cmd.Env = append(os.Environ(),
		"HOME="+workRoot,
		"TERM=xterm-256color",
		"USER=demo",
		"LOGNAME=demo",
		"SHELL=/bin/sh",
		"VMSH_DEMO=1",
	)

	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 18, Cols: 110})
	if err != nil {
		return err
	}
	defer tty.Close()

	driver := newDemoPTYDriver(tty, opts.live)
	defer driver.Close()

	send := func(line, waitFor string, timeout time.Duration) error {
		lineStart := driver.Len()
		if err := driver.TypeLine(line); err != nil {
			return err
		}
		start, err := driver.WaitForLineFeedAfter(lineStart, 2*time.Second)
		if err != nil {
			return err
		}
		if waitFor == "" {
			time.Sleep(timeout)
			return nil
		}
		return driver.WaitForAfterHandlingPrompts(waitFor, start, timeout)
	}

	if err := driver.WaitFor("vmsh", 20*time.Second); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)

	commands := []struct {
		line    string
		expect  string
		timeout time.Duration
	}{
		{fmt.Sprintf("@demo --from %s --memory %s", opts.vmImage, opts.vmMemory), "vm:", 2 * time.Minute},
		{"sh -lc 'printf \"guest: \"; uname -srm'", "Linux", 30 * time.Second},
		{"@host sh -lc 'mkdir -p " + shellSingleQuote(hostWork) + " && printf \"hello-from-host\\n\" > " + shellSingleQuote(filepath.Join(hostWork, "note.txt")) + "'", "vmsh", 20 * time.Second},
		{"@copy @host:" + hostWork + "/note.txt @:/tmp/note.txt", "vmsh", 20 * time.Second},
		{"sh -lc 'printf \"vm saw: \"; cat /tmp/note.txt'", "hello-from-host", 30 * time.Second},
		{"sh -c 'printf \"hello-from-vm\\n\" > /tmp/reply.txt'", "vmsh", 20 * time.Second},
		{"@copy @:/tmp/reply.txt @host:" + hostWork + "/reply.txt", "vmsh", 20 * time.Second},
		{"@host sh -c 'printf \"host saw: \"; cat " + hostWork + "/reply.txt'", "hello-from-vm", 20 * time.Second},
		{"@ssh demo-ssh sh -lc 'printf \"ssh: \"; hostname; printf \"hello-from-ssh\\n\" > ssh.txt'", "ssh:", 20 * time.Second},
		{"@copy @ssh:demo-ssh:ssh.txt @host:" + hostWork + "/ssh.txt", "vmsh", 20 * time.Second},
		{"@host sh -c 'printf \"host copied: \"; cat " + hostWork + "/ssh.txt'", "hello-from-ssh", 20 * time.Second},
		{"@ps", "running", 20 * time.Second},
		{"@stop ssh:demo-ssh", "vmsh", 20 * time.Second},
		{"@stop demo", "vmsh", 30 * time.Second},
		{"@demo-freebsd --from freebsd --memory 1024 --cpus 1", "vm:", 3 * time.Minute},
		{"sh -lc 'printf \"one more guest: \"; uname -srm'", "FreeBSD", 30 * time.Second},
		{"@stop demo-freebsd", "vmsh", 30 * time.Second},
	}
	for _, item := range commands {
		if err := send(item.line, item.expect, item.timeout); err != nil {
			return fmt.Errorf("demo command %q: %w", item.line, err)
		}
		time.Sleep(1500 * time.Millisecond)
	}

	if err := driver.TypeLine("exit"); err != nil {
		return err
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 0 {
			return nil
		}
		return waitErr
	}
	return nil
}

type demoPTYDriver struct {
	f    *os.File
	live bool

	mu     sync.Mutex
	buffer bytes.Buffer
	done   chan error
	closed sync.Once
}

func newDemoPTYDriver(f *os.File, live bool) *demoPTYDriver {
	d := &demoPTYDriver{f: f, live: live, done: make(chan error, 1)}
	go d.readLoop()
	return d
}

func (d *demoPTYDriver) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := d.f.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if d.live {
				_, _ = os.Stdout.Write(chunk)
			}
			d.mu.Lock()
			_, _ = d.buffer.Write(chunk)
			if d.buffer.Len() > 512*1024 {
				data := d.buffer.Bytes()
				keep := append([]byte(nil), data[len(data)-256*1024:]...)
				d.buffer.Reset()
				_, _ = d.buffer.Write(keep)
			}
			d.mu.Unlock()
		}
		if err != nil {
			d.done <- err
			return
		}
	}
}

func (d *demoPTYDriver) Close() {
	d.closed.Do(func() {
		_ = d.f.Close()
	})
}

func (d *demoPTYDriver) TypeLine(line string) error {
	for _, r := range line {
		if _, err := io.WriteString(d.f, string(r)); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	_, err := io.WriteString(d.f, "\r")
	return err
}

func (d *demoPTYDriver) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buffer.Len()
}

func (d *demoPTYDriver) WaitFor(substr string, timeout time.Duration) error {
	return d.WaitForAfter(substr, 0, timeout)
}

func (d *demoPTYDriver) WaitForLineFeedAfter(offset int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		d.mu.Lock()
		text := d.buffer.String()
		if offset < 0 {
			offset = 0
		}
		if offset > len(text) {
			offset = len(text)
		}
		relative := strings.IndexByte(text[offset:], '\n')
		if relative >= 0 {
			d.mu.Unlock()
			return offset + relative + 1, nil
		}
		tail := text
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		d.mu.Unlock()
		if time.Now().After(deadline) {
			return offset, fmt.Errorf("timed out waiting for submitted line to run; output tail:\n%s", tail)
		}
		select {
		case err := <-d.done:
			return offset, fmt.Errorf("vmsh exited while waiting for submitted line: %w", err)
		case <-tick.C:
		}
	}
}

func (d *demoPTYDriver) WaitForAfter(substr string, offset int, timeout time.Duration) error {
	return d.waitForAfter(substr, offset, timeout, false)
}

func (d *demoPTYDriver) WaitForAfterHandlingPrompts(substr string, offset int, timeout time.Duration) error {
	return d.waitForAfter(substr, offset, timeout, true)
}

func (d *demoPTYDriver) waitForAfter(substr string, offset int, timeout time.Duration, handlePrompts bool) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	answeredPrompts := map[string]bool{}
	for {
		d.mu.Lock()
		text := d.buffer.String()
		if offset < 0 {
			offset = 0
		}
		if offset > len(text) {
			offset = len(text)
		}
		have := strings.Contains(text[offset:], substr)
		tail := text
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		d.mu.Unlock()
		if have {
			return nil
		}
		if handlePrompts {
			current := text[offset:]
			for _, prompt := range []string{"(y/n) [n]:", "Trust this host and add it to known_hosts? (yes/no) [no]:"} {
				if strings.Contains(current, prompt) && !answeredPrompts[prompt] {
					answeredPrompts[prompt] = true
					answer := "y"
					if strings.Contains(prompt, "known_hosts") {
						answer = "yes"
					}
					if err := d.TypeLine(answer); err != nil {
						return err
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %q; output tail:\n%s", substr, tail)
		}
		select {
		case err := <-d.done:
			return fmt.Errorf("vmsh exited while waiting for %q: %w", substr, err)
		case <-tick.C:
		}
	}
}

type demoSSHServer struct {
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
}

func startDemoSSHServer(parent context.Context, workRoot string) (*demoSSHServer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &demoSSHServer{listener: ln, cancel: cancel, done: make(chan struct{})}
	remoteRoot := filepath.Join(workRoot, "remote")
	if err := os.MkdirAll(remoteRoot, 0o755); err != nil {
		_ = ln.Close()
		cancel()
		return nil, err
	}
	go s.acceptLoop(ctx, cfg, remoteRoot)
	return s, nil
}

func (s *demoSSHServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *demoSSHServer) Close() {
	s.cancel()
	_ = s.listener.Close()
	<-s.done
}

func (s *demoSSHServer) acceptLoop(ctx context.Context, cfg *ssh.ServerConfig, remoteRoot string) {
	defer close(s.done)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		go handleDemoSSHConn(conn, cfg, remoteRoot)
	}
}

func handleDemoSSHConn(conn net.Conn, cfg *ssh.ServerConfig, remoteRoot string) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "session channels only")
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go handleDemoSSHSession(channel, requests, remoteRoot)
	}
}

func handleDemoSSHSession(channel ssh.Channel, requests <-chan *ssh.Request, remoteRoot string) {
	defer channel.Close()
	for req := range requests {
		switch req.Type {
		case "env", "pty-req":
			req.Reply(true, nil)
		case "exec":
			command, err := parseSSHExecPayload(req.Payload)
			if err != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			code := runDemoSSHCommand(remoteRoot, command, channel, channel.Stderr(), channel)
			var payload [4]byte
			binary.BigEndian.PutUint32(payload[:], uint32(code))
			_, _ = channel.SendRequest("exit-status", false, payload[:])
			return
		case "shell":
			req.Reply(true, nil)
			code := runDemoSSHCommand(remoteRoot, "exec sh -i", channel, channel.Stderr(), channel)
			var payload [4]byte
			binary.BigEndian.PutUint32(payload[:], uint32(code))
			_, _ = channel.SendRequest("exit-status", false, payload[:])
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func parseSSHExecPayload(payload []byte) (string, error) {
	if len(payload) < 4 {
		return "", io.ErrUnexpectedEOF
	}
	n := int(binary.BigEndian.Uint32(payload[:4]))
	if n < 0 || len(payload[4:]) < n {
		return "", io.ErrUnexpectedEOF
	}
	return string(payload[4 : 4+n]), nil
}

func runDemoSSHCommand(remoteRoot, command string, stdout, stderr io.Writer, stdin io.Reader) int {
	cmd := exec.Command("sh", "-lc", command)
	cmd.Dir = remoteRoot
	cmd.Env = append(os.Environ(),
		"HOME="+remoteRoot,
		"USER=demo",
		"LOGNAME=demo",
		"HOSTNAME=demo-ssh",
		"TERM=xterm-256color",
	)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "demo ssh: %v\n", err)
		return 1
	}
	return 0
}

func writeDemoSSHConfig(workRoot, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	sshDir := filepath.Join(workRoot, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	config := strings.Join([]string{
		"Host demo-ssh",
		"  HostName " + host,
		"  Port " + port,
		"  User demo",
		"  StrictHostKeyChecking no",
		"  UserKnownHostsFile /dev/null",
		"  PreferredAuthentications none",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sshDir, "known_hosts"), nil, 0o600)
}

func redactDemoCast(rawPath, outPath string, p paths, sshAddr string) error {
	data, err := os.ReadFile(rawPath)
	if err != nil {
		return err
	}
	text := string(data)
	replacements := []demoRedaction{
		{old: p.root, new: "/work/vmsh"},
	}
	if realRoot, err := filepath.EvalSymlinks(p.root); err == nil && realRoot != "" {
		replacements = append(replacements, demoRedaction{old: realRoot, new: "/work/vmsh"})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		replacements = append(replacements, demoRedaction{old: home, new: "~"})
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		replacements = append(replacements, demoRedaction{old: host, new: "demo-host"})
	}
	if u, err := user.Current(); err == nil {
		if u.Username != "" {
			replacements = append(replacements, demoRedaction{old: u.Username, new: "demo"})
		}
		if u.Name != "" {
			replacements = append(replacements, demoRedaction{old: u.Name, new: "Demo User"})
		}
	}
	if sshAddr != "" {
		replacements = append(replacements, demoRedaction{old: sshAddr, new: "127.0.0.1:2222"})
	}
	sort.SliceStable(replacements, func(i, j int) bool {
		return len(replacements[i].old) > len(replacements[j].old)
	})
	for _, replacement := range replacements {
		if replacement.old == "" || replacement.old == replacement.new {
			continue
		}
		text = strings.ReplaceAll(text, replacement.old, replacement.new)
	}
	text = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`).ReplaceAllString(text, "127.0.0.1:2222")
	text = normalizeDemoCastTimeline(text)
	return os.WriteFile(outPath, []byte(text), 0o644)
}

type demoRedaction struct {
	old string
	new string
}

func normalizeDemoCastTimeline(text string) string {
	lines := strings.SplitAfter(text, "\n")
	var firstEvent *float64
	var out []string
	var pending *demoCastEvent
	var timeShift float64
	var spinnerAnchorRaw float64
	var spinnerAnchorOut float64
	var spinnerEvents int
	inBootSpinner := false
	flushPending := func() {
		if pending == nil {
			return
		}
		out = append(out, pending.String())
		pending = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "{") {
			flushPending()
			out = append(out, line)
			continue
		}
		event, err := parseDemoCastEvent(trimmed)
		if err != nil {
			flushPending()
			out = append(out, line)
			continue
		}
		if firstEvent == nil {
			first := event.Time
			firstEvent = &first
		}
		event.Time -= *firstEvent
		if event.Time < 0 {
			event.Time = 0
		}
		if isDemoBootSpinnerEvent(event) {
			if !inBootSpinner {
				inBootSpinner = true
				spinnerEvents = 0
				spinnerAnchorRaw = event.Time
				spinnerAnchorOut = event.Time - timeShift
			}
			spinnerEvents++
			if spinnerEvents > 6 {
				continue
			}
			event.Time -= timeShift
		} else {
			if inBootSpinner {
				rawDuration := event.Time - spinnerAnchorRaw
				displayDuration := rawDuration
				if displayDuration > 1.2 {
					displayDuration = 1.2
				}
				timeShift = event.Time - (spinnerAnchorOut + displayDuration)
				inBootSpinner = false
				spinnerEvents = 0
			}
			event.Time -= timeShift
		}
		if event.Time < 0 {
			event.Time = 0
		}
		if pending != nil && pending.canMerge(event) {
			pending.Data += event.Data
			continue
		}
		flushPending()
		pending = &event
	}
	flushPending()
	return strings.Join(out, "")
}

func isDemoBootSpinnerEvent(event demoCastEvent) bool {
	return event.Kind == "o" && strings.Contains(event.Data, "Boot: starting VM")
}

type demoCastEvent struct {
	Time float64
	Kind string
	Data string
}

func parseDemoCastEvent(line string) (demoCastEvent, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return demoCastEvent{}, err
	}
	if len(raw) < 3 {
		return demoCastEvent{}, fmt.Errorf("short cast event")
	}
	var event demoCastEvent
	if err := json.Unmarshal(raw[0], &event.Time); err != nil {
		return demoCastEvent{}, err
	}
	if err := json.Unmarshal(raw[1], &event.Kind); err != nil {
		return demoCastEvent{}, err
	}
	if err := json.Unmarshal(raw[2], &event.Data); err != nil {
		return demoCastEvent{}, err
	}
	return event, nil
}

func (e demoCastEvent) canMerge(next demoCastEvent) bool {
	if e.Kind != "o" || next.Kind != "o" {
		return false
	}
	if next.Time < e.Time {
		return false
	}
	return next.Time-e.Time <= 0.025
}

func (e demoCastEvent) String() string {
	raw := []any{e.Time, e.Kind, e.Data}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(encoded) + "\n"
}

func renderDemoGIF(castPath, gifPath string) error {
	agg, err := exec.LookPath("agg")
	if err != nil {
		return fmt.Errorf("agg is not installed")
	}
	if err := os.MkdirAll(filepath.Dir(gifPath), 0o755); err != nil {
		return err
	}
	logf("demo: rendering gif %s", gifPath)
	cmd := exec.Command(agg, castPath, gifPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("agg %s %s: %w", castPath, gifPath, err)
	}
	logf("demo: wrote gif %s", gifPath)
	return nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
