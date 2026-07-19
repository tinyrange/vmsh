package vmshd

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	pathpkg "path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"j5.nz/cc/client"
)

const (
	mcpShellEventBuffer   = 256
	mcpMaxContextCaptures = 8
)

type mcpGuestContext struct {
	id          string
	vmID        string
	user        string
	workDir     string
	env         []string
	controlPath string
	captureDir  string
	inputs      chan client.ExecInput
	events      chan client.ExecEvent
	cancel      context.CancelFunc
	done        chan struct{}
	control     *client.Client

	runMu    sync.Mutex
	stopOnce sync.Once
	mu       sync.Mutex
	err      string
	carry    []byte
	captures []mcpContextCapture
}

type mcpContextCapture struct {
	stdoutPath   string
	stderrPath   string
	stdoutOffset int64
	stderrOffset int64
}

type mcpContextOpenInput struct {
	VMID    string   `json:"vm_id" jsonschema:"ID returned by vm_create"`
	WorkDir string   `json:"workdir,omitempty" jsonschema:"initial working directory; defaults to / for built-in BSD guests and /home/cc otherwise"`
	User    string   `json:"user,omitempty" jsonschema:"guest user name or uid[:gid]; built-in BSD guests currently support only root; defaults to 1000:1000 otherwise"`
	Env     []string `json:"env,omitempty" jsonschema:"initial exported environment entries in NAME=value form"`
}

type mcpContextInfo struct {
	ContextID string `json:"context_id"`
	VMID      string `json:"vm_id"`
	Status    string `json:"status"`
	User      string `json:"user"`
	WorkDir   string `json:"initial_workdir"`
	Error     string `json:"error,omitempty"`
}

func (e *mcpEndpoint) openGuestContext(ctx context.Context, _ *mcp.CallToolRequest, in mcpContextOpenInput) (*mcp.CallToolResult, mcpContextInfo, error) {
	if err := validateMCPStringBytes("environment", in.Env, mcpMaxEnvBytes); err != nil {
		return nil, mcpContextInfo{}, err
	}
	if len(in.WorkDir) > mcpMaxPathBytes {
		return nil, mcpContextInfo{}, fmt.Errorf("workdir exceeds the %d-byte limit", mcpMaxPathBytes)
	}
	vm, err := e.ownedVM(in.VMID)
	if err != nil {
		return nil, mcpContextInfo{}, err
	}
	user, err := mcpGuestUser(vm, in.User)
	if err != nil {
		return nil, mcpContextInfo{}, err
	}
	workDir := mcpGuestWorkDir(vm, in.WorkDir)
	if err := e.reserveGuestContext(); err != nil {
		return nil, mcpContextInfo{}, err
	}
	reserved := true
	defer func() {
		if reserved {
			e.releaseGuestContextReservation()
		}
	}()
	id, err := randomMCPID("context")
	if err != nil {
		return nil, mcpContextInfo{}, err
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	captureDir := "/tmp/.vmsh-" + id
	guest := &mcpGuestContext{
		id: id, vmID: vm.ID, user: user, workDir: workDir, env: append([]string(nil), in.Env...), captureDir: captureDir, controlPath: captureDir + "/control",
		inputs: make(chan client.ExecInput, 16), events: make(chan client.ExecEvent, mcpShellEventBuffer), cancel: cancel, done: make(chan struct{}), control: e.control,
	}
	e.mu.Lock()
	e.openingContexts--
	reserved = false
	if e.closed {
		e.mu.Unlock()
		cancel()
		return nil, mcpContextInfo{}, fmt.Errorf("MCP endpoint is stopped")
	}
	e.contexts[id] = guest
	e.mu.Unlock()
	go guest.serve(streamCtx, e.control)
	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	if _, err := guest.runLine(probeCtx, ":"); err != nil {
		guest.stop()
		e.mu.Lock()
		delete(e.contexts, id)
		e.mu.Unlock()
		return nil, mcpContextInfo{}, fmt.Errorf("open guest context: %w", err)
	}
	return nil, guest.info(), nil
}

func (e *mcpEndpoint) reserveGuestContext() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("MCP endpoint is stopped")
	}
	for id, guest := range e.contexts {
		select {
		case <-guest.done:
			delete(e.contexts, id)
		default:
		}
	}
	e.openingContexts++
	return nil
}

func (e *mcpEndpoint) releaseGuestContextReservation() {
	e.mu.Lock()
	e.openingContexts--
	e.mu.Unlock()
}

type mcpContextRunInput struct {
	ContextID      string   `json:"context_id" jsonschema:"ID returned by vm_context_open"`
	CommandLine    string   `json:"command_line,omitempty" jsonschema:"shell command line; cwd, exports, functions, and aliases persist in this context"`
	Command        []string `json:"command,omitempty" jsonschema:"command and arguments safely quoted for the guest shell; mutually exclusive with command_line"`
	TimeoutSeconds float64  `json:"timeout_seconds,omitempty" jsonschema:"deadline in seconds; a timeout closes the context to guarantee termination"`
}

type mcpContextRunOutput struct {
	ContextID             string `json:"context_id"`
	VMID                  string `json:"vm_id"`
	ContextStatus         string `json:"context_status"`
	CommandStatus         string `json:"command_status"`
	ExitCode              int    `json:"exit_code"`
	Stdout                string `json:"stdout,omitempty"`
	StdoutBase64          string `json:"stdout_base64,omitempty"`
	StdoutTotalBytes      int64  `json:"stdout_total_bytes"`
	StdoutTruncated       bool   `json:"stdout_truncated,omitempty"`
	Stderr                string `json:"stderr,omitempty"`
	StderrBase64          string `json:"stderr_base64,omitempty"`
	StderrTotalBytes      int64  `json:"stderr_total_bytes"`
	StderrTruncated       bool   `json:"stderr_truncated,omitempty"`
	AsyncStdout           string `json:"async_stdout,omitempty"`
	AsyncStdoutBase64     string `json:"async_stdout_base64,omitempty"`
	AsyncStdoutTotalBytes int64  `json:"async_stdout_total_bytes"`
	AsyncStdoutTruncated  bool   `json:"async_stdout_truncated,omitempty"`
	AsyncStderr           string `json:"async_stderr,omitempty"`
	AsyncStderrBase64     string `json:"async_stderr_base64,omitempty"`
	AsyncStderrTotalBytes int64  `json:"async_stderr_total_bytes"`
	AsyncStderrTruncated  bool   `json:"async_stderr_truncated,omitempty"`
	Error                 string `json:"error,omitempty"`
}

func (e *mcpEndpoint) runGuestContext(ctx context.Context, _ *mcp.CallToolRequest, in mcpContextRunInput) (*mcp.CallToolResult, mcpContextRunOutput, error) {
	guest, err := e.guestContext(in.ContextID)
	if err != nil {
		return nil, mcpContextRunOutput{}, err
	}
	line, err := validateGuestContextCommand(in)
	if err != nil {
		return nil, mcpContextRunOutput{}, err
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if in.TimeoutSeconds > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(in.TimeoutSeconds*float64(time.Second)))
		defer cancel()
	}
	result, err := guest.runLine(runCtx, line)
	out := mcpContextRunOutput{ContextID: guest.id, VMID: guest.vmID}
	applyContextResult(&out, result)
	if err != nil {
		if runCtx.Err() != nil {
			closed := guest.stopAndWait(3 * time.Second)
			status := "canceled"
			exitCode := 130
			if runCtx.Err() == context.DeadlineExceeded {
				status = "timed_out"
				exitCode = 124
			}
			out.ContextStatus = "closed"
			if !closed {
				out.ContextStatus = "closing"
				out.Error = "persistent context termination was not confirmed; VM retained with its filesystem intact and the command may still be running"
			}
			out.CommandStatus = status
			out.ExitCode = exitCode
			return nil, out, nil
		}
		return nil, mcpContextRunOutput{}, err
	}
	out.ContextStatus = "running"
	out.CommandStatus = "exited"
	out.ExitCode = result.exitCode
	return nil, out, nil
}

func applyContextResult(out *mcpContextRunOutput, result mcpContextResult) {
	encodeContextOutput(result.stdout, &out.Stdout, &out.StdoutBase64)
	encodeContextOutput(result.stderr, &out.Stderr, &out.StderrBase64)
	out.StdoutTotalBytes = result.stdoutTotal
	out.StdoutTruncated = result.stdoutTruncated
	out.StderrTotalBytes = result.stderrTotal
	out.StderrTruncated = result.stderrTruncated
	encodeContextOutput(result.asyncStdout, &out.AsyncStdout, &out.AsyncStdoutBase64)
	encodeContextOutput(result.asyncStderr, &out.AsyncStderr, &out.AsyncStderrBase64)
	out.AsyncStdoutTotalBytes = result.asyncStdoutTotal
	out.AsyncStdoutTruncated = result.asyncStdoutTruncated
	out.AsyncStderrTotalBytes = result.asyncStderrTotal
	out.AsyncStderrTruncated = result.asyncStderrTruncated
}

func validateGuestContextCommand(in mcpContextRunInput) (string, error) {
	if in.CommandLine != "" && len(in.Command) != 0 {
		return "", fmt.Errorf("command_line and command are mutually exclusive")
	}
	line := in.CommandLine
	if len(in.Command) != 0 {
		line = shellJoin(in.Command)
	}
	if strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("command_line or command is required")
	}
	if len(line) > mcpMaxCommandBytes {
		return "", fmt.Errorf("context command exceeds the %d-byte limit", mcpMaxCommandBytes)
	}
	if err := validateMCPDurationSeconds("timeout_seconds", in.TimeoutSeconds); err != nil {
		return "", err
	}
	return line, nil
}

func (e *mcpEndpoint) startGuestContextCommand(_ context.Context, _ *mcp.CallToolRequest, in mcpContextRunInput) (*mcp.CallToolResult, mcpCommandOutput, error) {
	line, err := validateGuestContextCommand(in)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	commandID, err := randomMCPID("cmd")
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	command := &mcpCommand{
		id: commandID, contextID: strings.TrimSpace(in.ContextID), cancel: cancel, done: make(chan struct{}),
		status: "running", startedAt: time.Now().UTC(),
	}
	guest, err := e.registerMCPCommand(command)
	if err != nil {
		cancel()
		return nil, mcpCommandOutput{}, err
	}
	go command.runGuestContext(ctx, guest, line, in.TimeoutSeconds)
	return nil, command.snapshot(0, 0, mcpDefaultOutputChunk, false), nil
}

func (c *mcpCommand) runGuestContext(ctx context.Context, guest *mcpGuestContext, line string, timeoutSeconds float64) {
	runCtx := ctx
	var timeoutCancel context.CancelFunc
	if timeoutSeconds > 0 {
		runCtx, timeoutCancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds*float64(time.Second)))
		defer timeoutCancel()
	}
	result, err := guest.runLine(runCtx, line)
	if runCtx.Err() != nil {
		guest.stopAndWait(3 * time.Second)
	}
	guestClosed := guest.info().Status == "closed"
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	defer close(c.done)
	c.finishedAt = &now
	c.stdout = append(c.stdout[:0], result.stdout...)
	c.stderr = append(c.stderr[:0], result.stderr...)
	c.stdoutTotal = result.stdoutTotal
	c.stderrTotal = result.stderrTotal
	c.stdoutTruncated = result.stdoutTruncated
	c.stderrTruncated = result.stderrTruncated
	c.asyncStdout = append(c.asyncStdout[:0], result.asyncStdout...)
	c.asyncStderr = append(c.asyncStderr[:0], result.asyncStderr...)
	c.asyncStdoutTotal = result.asyncStdoutTotal
	c.asyncStderrTotal = result.asyncStderrTotal
	c.asyncStdoutTruncated = result.asyncStdoutTruncated
	c.asyncStderrTruncated = result.asyncStderrTruncated
	if runCtx.Err() != nil && c.cancellationUnverifiable && c.containmentError == "" {
		c.markPrivilegedCancellationUnverifiableLocked()
	}
	if c.containmentError != "" {
		c.status = "termination_unconfirmed"
		c.exitCode = nil
		return
	}
	if runCtx.Err() == context.DeadlineExceeded {
		c.status = "timed_out"
		code := 124
		c.exitCode = &code
		return
	}
	if c.cancelRequested || runCtx.Err() != nil || guestClosed {
		c.status = "canceled"
		code := 130
		c.exitCode = &code
		return
	}
	if err != nil {
		c.status = "failed"
		c.err = err.Error()
		return
	}
	c.status = "exited"
	code := result.exitCode
	c.exitCode = &code
}

type mcpContextStatusInput struct {
	ContextID string `json:"context_id" jsonschema:"ID returned by vm_context_open"`
}

func (e *mcpEndpoint) statusGuestContext(_ context.Context, _ *mcp.CallToolRequest, in mcpContextStatusInput) (*mcp.CallToolResult, mcpContextInfo, error) {
	guest, err := e.guestContext(in.ContextID)
	if err != nil {
		return nil, mcpContextInfo{}, err
	}
	return nil, guest.info(), nil
}

type mcpContextCloseOutput struct {
	Closed bool `json:"closed"`
}

func (e *mcpEndpoint) closeGuestContext(ctx context.Context, _ *mcp.CallToolRequest, in mcpContextStatusInput) (*mcp.CallToolResult, mcpContextCloseOutput, error) {
	guest, err := e.guestContext(in.ContextID)
	if err != nil {
		return nil, mcpContextCloseOutput{}, err
	}
	e.mu.Lock()
	commands := make([]*mcpCommand, 0)
	for _, command := range e.commands {
		if command.contextID == guest.id {
			commands = append(commands, command)
		}
	}
	delete(e.contexts, guest.id)
	e.mu.Unlock()
	if err := cancelAndWaitMCPWork(ctx, e.workCleanupTimeout(), commands, []*mcpGuestContext{guest}); err != nil {
		return nil, mcpContextCloseOutput{}, fmt.Errorf("close guest context: %w", err)
	}
	return nil, mcpContextCloseOutput{Closed: true}, nil
}

type mcpContextResult struct {
	exitCode             int
	stdout               []byte
	stderr               []byte
	stdoutTotal          int64
	stderrTotal          int64
	stdoutTruncated      bool
	stderrTruncated      bool
	asyncStdout          []byte
	asyncStderr          []byte
	asyncStdoutTotal     int64
	asyncStderrTotal     int64
	asyncStdoutTruncated bool
	asyncStderrTruncated bool
}

func (g *mcpGuestContext) serve(ctx context.Context, control *client.Client) {
	err := control.RunInteractiveStreamInContext(ctx, g.vmID, client.RunRequest{
		Command: []string{"/bin/sh", "-c", mcpContextShellScript(g.controlPath)}, Env: append([]string(nil), g.env...), WorkDir: g.workDir, User: g.user, ControlFD: true,
	}, g.inputs, func(event client.ExecEvent) error {
		select {
		case g.events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = control.RunEventsInContext(cleanupCtx, g.vmID, client.RunRequest{
		Command: []string{"/bin/sh", "-c", `rm -rf "$1"`, "sh", g.captureDir},
		WorkDir: "/", User: g.user,
	})
	cleanupCancel()
	g.mu.Lock()
	if err != nil && ctx.Err() == nil {
		g.err = conciseCommandError(err)
	}
	g.mu.Unlock()
	close(g.done)
}

func (g *mcpGuestContext) runLine(ctx context.Context, line string) (mcpContextResult, error) {
	g.runMu.Lock()
	defer g.runMu.Unlock()
	var result mcpContextResult
	token, err := randomMCPID("marker")
	if err != nil {
		return mcpContextResult{}, err
	}
	marker := []byte("\x1e" + token + ":")
	beginMarker := []byte("\x1e" + token + ":begin\x1f")
	capture := mcpContextCapture{
		stdoutPath: g.captureDir + "/" + token + ".stdout",
		stderrPath: g.captureDir + "/" + token + ".stderr",
	}
	if err := g.collectContextCaptures(ctx, &result); err != nil {
		return result, err
	}
	script := mcpContextCommandCaptureScript(token, line, g.controlPath, capture.stdoutPath, capture.stderrPath)
	g.mu.Lock()
	initial := append([]byte(nil), g.carry...)
	g.carry = nil
	g.mu.Unlock()
	scan := append([]byte(nil), initial...)
	// Anything already delivered before this command is submitted belongs to
	// the persistent context, not to the new command. Drain it explicitly so a
	// background writer cannot be charged to the next command.
	for {
		select {
		case event := <-g.events:
			data := contextEventData(event)
			switch event.Kind {
			case "stdout", "stderr", "output":
				result.appendOutputEvent(event, data, true)
			case "control":
				scan = append(scan, data...)
			case "error":
				return result, fmt.Errorf("guest context: %s", firstNonEmpty(event.Error, event.Output, "shell failed"))
			case "exit":
				return result, fmt.Errorf("guest context shell exited with status %d", event.ExitCode)
			}
		default:
			goto drained
		}
	}
drained:
	select {
	case g.inputs <- client.ExecInput{Kind: "stdin", Data: []byte(script)}:
	case <-g.done:
		return result, g.closedError()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	begun := false
	for {
		if !begun {
			if start := bytes.Index(scan, beginMarker); start >= 0 {
				carryStart := start + len(beginMarker)
				if carryStart < len(scan) && scan[carryStart] == '\n' {
					carryStart++
				}
				scan = append(scan[:0], scan[carryStart:]...)
				begun = true
			} else if keep := len(beginMarker) - 1; len(scan) > keep {
				scan = scan[len(scan)-keep:]
			}
		}
		if begun {
			if start := bytes.Index(scan, marker); start >= 0 {
				statusStart := start + len(marker)
				if endRel := bytes.IndexByte(scan[statusStart:], '\x1f'); endRel >= 0 {
					end := statusStart + endRel
					code, parseErr := strconv.Atoi(string(scan[statusStart:end]))
					if parseErr != nil {
						return mcpContextResult{}, fmt.Errorf("invalid guest context status: %w", parseErr)
					}
					carryStart := end + 1
					if carryStart < len(scan) && scan[carryStart] == '\n' {
						carryStart++
					}
					g.mu.Lock()
					g.carry = append(g.carry[:0], scan[carryStart:]...)
					g.mu.Unlock()
					if err := g.collectCurrentCapture(ctx, &result, &capture); err != nil {
						return result, err
					}
					g.mu.Lock()
					g.captures = append(g.captures, capture)
					retired := append([]mcpContextCapture(nil), g.captures[:max(0, len(g.captures)-mcpMaxContextCaptures)]...)
					if len(retired) != 0 {
						g.captures = append([]mcpContextCapture(nil), g.captures[len(retired):]...)
					}
					g.mu.Unlock()
					g.removeContextCaptures(retired)
					result.exitCode = code
					return result, nil
				}
				if start > 0 {
					scan = scan[start:]
				}
			} else if keep := len(marker) - 1; len(scan) > keep {
				drop := len(scan) - keep
				scan = scan[drop:]
			}
		}
		select {
		case event := <-g.events:
			data := contextEventData(event)
			switch event.Kind {
			case "stdout", "stderr", "output":
				result.appendOutputEvent(event, data, false)
			case "control":
				scan = append(scan, data...)
			case "error":
				return result, fmt.Errorf("guest context: %s", firstNonEmpty(event.Error, event.Output, "shell failed"))
			case "exit":
				return result, fmt.Errorf("guest context shell exited with status %d", event.ExitCode)
			}
		case <-g.done:
			return result, g.closedError()
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
}

func (g *mcpGuestContext) collectContextCaptures(ctx context.Context, result *mcpContextResult) error {
	g.mu.Lock()
	captures := append([]mcpContextCapture(nil), g.captures...)
	g.mu.Unlock()
	for i := range captures {
		stdout, stdoutSize, err := g.readContextCapture(ctx, captures[i].stdoutPath, captures[i].stdoutOffset, mcpMaxCommandStreamBytes-len(result.asyncStdout))
		if err != nil {
			return err
		}
		stderr, stderrSize, err := g.readContextCapture(ctx, captures[i].stderrPath, captures[i].stderrOffset, mcpMaxCommandStreamBytes-len(result.asyncStderr))
		if err != nil {
			return err
		}
		stdoutDelta := max(int64(0), stdoutSize-captures[i].stdoutOffset)
		stderrDelta := max(int64(0), stderrSize-captures[i].stderrOffset)
		result.asyncStdout, result.asyncStdoutTotal, result.asyncStdoutTruncated = appendCapturedOutput(result.asyncStdout, result.asyncStdoutTotal, result.asyncStdoutTruncated, stdout, stdoutDelta)
		result.asyncStderr, result.asyncStderrTotal, result.asyncStderrTruncated = appendCapturedOutput(result.asyncStderr, result.asyncStderrTotal, result.asyncStderrTruncated, stderr, stderrDelta)
		captures[i].stdoutOffset = stdoutSize
		captures[i].stderrOffset = stderrSize
	}
	g.mu.Lock()
	for i := range captures {
		if i < len(g.captures) && g.captures[i].stdoutPath == captures[i].stdoutPath {
			g.captures[i] = captures[i]
		}
	}
	g.mu.Unlock()
	return nil
}

func (g *mcpGuestContext) collectCurrentCapture(ctx context.Context, result *mcpContextResult, capture *mcpContextCapture) error {
	stdout, stdoutSize, err := g.readContextCapture(ctx, capture.stdoutPath, 0, mcpMaxCommandStreamBytes)
	if err != nil {
		return err
	}
	stderr, stderrSize, err := g.readContextCapture(ctx, capture.stderrPath, 0, mcpMaxCommandStreamBytes)
	if err != nil {
		return err
	}
	result.stdout, result.stdoutTotal, result.stdoutTruncated = appendCapturedOutput(result.stdout, result.stdoutTotal, result.stdoutTruncated, stdout, stdoutSize)
	result.stderr, result.stderrTotal, result.stderrTruncated = appendCapturedOutput(result.stderr, result.stderrTotal, result.stderrTruncated, stderr, stderrSize)
	capture.stdoutOffset = stdoutSize
	capture.stderrOffset = stderrSize
	return nil
}

func appendCapturedOutput(dst []byte, total int64, truncated bool, data []byte, observed int64) ([]byte, int64, bool) {
	dst, total, truncated = appendCommandOutput(dst, total, truncated, data)
	if observed > int64(len(data)) {
		total += observed - int64(len(data))
		truncated = true
	}
	return dst, total, truncated
}

func (g *mcpGuestContext) readContextCapture(ctx context.Context, capturePath string, offset int64, limit int) ([]byte, int64, error) {
	if g.control == nil {
		return nil, 0, fmt.Errorf("guest context control client is unavailable")
	}
	if limit < 0 {
		limit = 0
	}
	script := `if [ ! -f "$1" ]; then printf '0\n' >&2; exit; fi; size=$(wc -c <"$1") || exit; printf '%s\n' "$size" >&2; if [ "$3" -gt 0 ] && [ "$size" -gt "$2" ]; then tail -c "+$(( $2 + 1 ))" "$1" | head -c "$3"; fi`
	var output, diagnostic []byte
	err := g.control.ExecStreamInContext(ctx, g.vmID, client.ExecRequest{
		Command: []string{"/bin/sh", "-c", script, "sh", capturePath, strconv.FormatInt(offset, 10), strconv.Itoa(limit)},
		WorkDir: "/", User: g.user,
	}, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			if event.Stream != "stderr" && len(output) < limit {
				data := contextEventData(event)
				n := limit - len(output)
				if n > len(data) {
					n = len(data)
				}
				output = append(output, data[:n]...)
			}
		case "stderr":
			diagnostic = append(diagnostic, contextEventData(event)...)
		case "error":
			return fmt.Errorf("%s", firstNonEmpty(event.Error, event.Output, "command failed"))
		case "exit":
			if event.ExitCode != 0 {
				return fmt.Errorf("read context output exited with status %d", event.ExitCode)
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("read context output: %w", err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(diagnostic)), 10, 64)
	if err != nil || size < 0 {
		return nil, 0, fmt.Errorf("read context output returned invalid size")
	}
	return output, size, nil
}

func (g *mcpGuestContext) removeContextCaptures(captures []mcpContextCapture) {
	if len(captures) == 0 || g.control == nil {
		return
	}
	args := []string{"/bin/rm", "-f"}
	for _, capture := range captures {
		args = append(args, capture.stdoutPath, capture.stderrPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = g.control.RunEventsInContext(ctx, g.vmID, client.RunRequest{Command: args, WorkDir: "/", User: g.user})
}

func contextEventData(event client.ExecEvent) []byte {
	if len(event.Data) != 0 {
		return event.Data
	}
	return []byte(event.Output)
}

func (r *mcpContextResult) appendOutputEvent(event client.ExecEvent, data []byte, async bool) {
	stderr := event.Kind == "stderr" || event.Kind == "output" && event.Stream == "stderr"
	if async {
		if stderr {
			r.asyncStderr, r.asyncStderrTotal, r.asyncStderrTruncated = appendCommandOutput(r.asyncStderr, r.asyncStderrTotal, r.asyncStderrTruncated, data)
		} else {
			r.asyncStdout, r.asyncStdoutTotal, r.asyncStdoutTruncated = appendCommandOutput(r.asyncStdout, r.asyncStdoutTotal, r.asyncStdoutTruncated, data)
		}
		return
	}
	if stderr {
		r.stderr, r.stderrTotal, r.stderrTruncated = appendCommandOutput(r.stderr, r.stderrTotal, r.stderrTruncated, data)
	} else {
		r.stdout, r.stdoutTotal, r.stdoutTruncated = appendCommandOutput(r.stdout, r.stdoutTotal, r.stdoutTruncated, data)
	}
}

func mcpContextShellScript(controlPath string) string {
	return mcpContextShellScriptForShell(controlPath, "/bin/sh")
}

func mcpContextShellScriptForShell(controlPath, shellPath string) string {
	path := shellJoin([]string{controlPath})
	dir := shellJoin([]string{pathpkg.Dir(controlPath)})
	shell := shellJoin([]string{shellPath})
	return "__vmsh_mcp_fifo=" + path + "\n" +
		"__vmsh_mcp_relay_stop=__vmsh_mcp_relay_stop__\n" +
		"__vmsh_mcp_initial_umask=$(umask)\n" +
		"umask 077\n" +
		"rm -rf " + dir + "\n" +
		"mkdir -m 700 " + dir + " || exit 126\n" +
		"mkfifo \"$__vmsh_mcp_fifo\" || exit 126\n" +
		"(exec 8<>\"$__vmsh_mcp_fifo\"; while IFS= read -r __vmsh_mcp_frame <&8; do [ \"$__vmsh_mcp_frame\" = \"$__vmsh_mcp_relay_stop\" ] && break; command printf '%s\\n' \"$__vmsh_mcp_frame\"; done) >&3 &\n" +
		"__vmsh_mcp_relay=$!\n" +
		// Keep fd 3 occupied so shells such as BusyBox ash do not reuse it as
		// an internal script-reader descriptor. The user may freely close or
		// replace this harmless placeholder without affecting the relay.
		"exec 3</dev/null\n" +
		"__vmsh_mcp_cleanup() { (command printf '%s\\n' \"$__vmsh_mcp_relay_stop\" >\"$__vmsh_mcp_fifo\") & __vmsh_mcp_stop_writer=$!; wait \"$__vmsh_mcp_relay\" 2>/dev/null; kill \"$__vmsh_mcp_stop_writer\" 2>/dev/null; wait \"$__vmsh_mcp_stop_writer\" 2>/dev/null; rm -f \"$__vmsh_mcp_fifo\"; }\n" +
		"trap '__vmsh_mcp_cleanup; exit 143' HUP INT TERM\n" +
		"umask \"$__vmsh_mcp_initial_umask\"\n" +
		"unset __vmsh_mcp_initial_umask\n" +
		shell + "\n" +
		"__vmsh_mcp_shell_status=$?\n" +
		"trap - HUP INT TERM\n" +
		"__vmsh_mcp_cleanup\n" +
		"exit \"$__vmsh_mcp_shell_status\"\n"
}

func mcpContextCommandScript(token, line, controlPath string) string {
	statusVar := "__vmsh_mcp_status_" + strings.TrimPrefix(token, "marker_")
	path := shellJoin([]string{controlPath})
	return "/usr/bin/printf '\\036" + token + ":begin\\037\\n' >" + path + "\nif {\n" + line + "\n} </dev/null; then\n" + statusVar + "=0\nelse\n" + statusVar + "=$?\nfi\n/usr/bin/printf '\\036" + token + ":%s\\037\\n' \"$" + statusVar + "\" >" + path + "\nunset " + statusVar + "\n"
}

func mcpContextCommandCaptureScript(token, line, controlPath, stdoutPath, stderrPath string) string {
	statusVar := "__vmsh_mcp_status_" + strings.TrimPrefix(token, "marker_")
	umaskVar := "__vmsh_mcp_umask_" + strings.TrimPrefix(token, "marker_")
	path := shellJoin([]string{controlPath})
	stdout := shellJoin([]string{stdoutPath})
	stderr := shellJoin([]string{stderrPath})
	return umaskVar + "=$(umask)\numask 077\n: >" + stdout + "\n: >" + stderr + "\nchmod 600 " + stdout + " " + stderr + "\numask \"$" + umaskVar + "\"\n" +
		"/usr/bin/printf '\\036" + token + ":begin\\037\\n' >" + path + "\nif {\n" + line +
		"\n} </dev/null >" + stdout + " 2>" + stderr + "; then\n" + statusVar + "=0\nelse\n" + statusVar + "=$?\nfi\n" +
		"/usr/bin/printf '\\036" + token + ":%s\\037\\n' \"$" + statusVar + "\" >" + path + "\nunset " + statusVar + " " + umaskVar + "\n"
}

func (g *mcpGuestContext) info() mcpContextInfo {
	g.mu.Lock()
	err := g.err
	g.mu.Unlock()
	status := "running"
	select {
	case <-g.done:
		status = "closed"
	default:
	}
	return mcpContextInfo{ContextID: g.id, VMID: g.vmID, Status: status, User: g.user, WorkDir: g.workDir, Error: err}
}

func (g *mcpGuestContext) stop() {
	g.stopOnce.Do(g.cancel)
}

func (g *mcpGuestContext) stopAndWait(timeout time.Duration) bool {
	g.stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-g.done:
		return true
	case <-timer.C:
		return false
	}
}

func (g *mcpGuestContext) closedError() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fmt.Errorf("guest context is closed: %s", firstNonEmpty(g.err, "shell exited"))
}

func (e *mcpEndpoint) guestContext(id string) (*mcpGuestContext, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	guest := e.contexts[id]
	e.mu.Unlock()
	if guest == nil {
		return nil, fmt.Errorf("context %q is not owned by this MCP session", id)
	}
	return guest, nil
}

func shellJoin(command []string) string {
	quoted := make([]string, len(command))
	for i, arg := range command {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	return strings.Join(quoted, " ")
}

func encodeContextOutput(data []byte, text, encoded *string) {
	if efficientTextOutput(data) {
		*text = string(data)
	} else if len(data) != 0 {
		*encoded = base64.StdEncoding.EncodeToString(data)
	}
}
