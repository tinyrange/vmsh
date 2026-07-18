package vmshd

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"j5.nz/cc/client"
)

const mcpShellEventBuffer = 256

type mcpGuestContext struct {
	id      string
	vmID    string
	user    string
	workDir string
	env     []string
	inputs  chan client.ExecInput
	events  chan client.ExecEvent
	cancel  context.CancelFunc
	done    chan struct{}

	runMu    sync.Mutex
	stopOnce sync.Once
	mu       sync.Mutex
	err      string
	carry    []byte
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
	if err := validateGuestContextStart(ctx, e.control, vm.ID, user, workDir, in.Env); err != nil {
		return nil, mcpContextInfo{}, fmt.Errorf("open guest context: %w", err)
	}
	id, err := randomMCPID("context")
	if err != nil {
		return nil, mcpContextInfo{}, err
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	guest := &mcpGuestContext{
		id: id, vmID: vm.ID, user: user, workDir: workDir, env: append([]string(nil), in.Env...),
		inputs: make(chan client.ExecInput, 16), events: make(chan client.ExecEvent, mcpShellEventBuffer), cancel: cancel, done: make(chan struct{}),
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
	if len(e.contexts)+e.openingContexts >= mcpMaxContexts {
		return fmt.Errorf("MCP session context limit of %d reached", mcpMaxContexts)
	}
	e.openingContexts++
	return nil
}

func (e *mcpEndpoint) releaseGuestContextReservation() {
	e.mu.Lock()
	e.openingContexts--
	e.mu.Unlock()
}

func validateGuestContextStart(ctx context.Context, control *client.Client, vmID, user, workDir string, env []string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	events, err := control.RunEventsInContext(probeCtx, vmID, client.RunRequest{
		Command: []string{"/bin/sh", "-c", ":"}, Env: append([]string(nil), env...), WorkDir: workDir, User: user,
	})
	if err != nil {
		return errors.New(conciseCommandError(err))
	}
	exitCode := -1
	var diagnostic strings.Builder
	for _, event := range events {
		switch event.Kind {
		case "stderr", "error":
			data := event.Data
			if len(data) == 0 {
				data = []byte(firstNonEmpty(event.Error, event.Output))
			}
			diagnostic.Write(data)
		case "exit":
			exitCode = event.ExitCode
		}
	}
	if exitCode == 0 {
		return nil
	}
	message := strings.TrimSpace(diagnostic.String())
	if message != "" {
		return fmt.Errorf("%s", message)
	}
	if exitCode >= 0 {
		return fmt.Errorf("workdir validation exited with status %d", exitCode)
	}
	return fmt.Errorf("workdir validation ended without an exit status")
}

type mcpContextRunInput struct {
	ContextID      string   `json:"context_id" jsonschema:"ID returned by vm_context_open"`
	CommandLine    string   `json:"command_line,omitempty" jsonschema:"shell command line; cwd, exports, functions, and aliases persist in this context"`
	Command        []string `json:"command,omitempty" jsonschema:"command and arguments safely quoted for the guest shell; mutually exclusive with command_line"`
	TimeoutSeconds float64  `json:"timeout_seconds,omitempty" jsonschema:"deadline in seconds; a timeout closes the context to guarantee termination"`
}

type mcpContextRunOutput struct {
	ContextID        string `json:"context_id"`
	VMID             string `json:"vm_id"`
	ContextStatus    string `json:"context_status"`
	CommandStatus    string `json:"command_status"`
	ExitCode         int    `json:"exit_code"`
	Stdout           string `json:"stdout,omitempty"`
	StdoutBase64     string `json:"stdout_base64,omitempty"`
	StdoutTotalBytes int64  `json:"stdout_total_bytes"`
	StdoutTruncated  bool   `json:"stdout_truncated,omitempty"`
	Stderr           string `json:"stderr,omitempty"`
	StderrBase64     string `json:"stderr_base64,omitempty"`
	StderrTotalBytes int64  `json:"stderr_total_bytes"`
	StderrTruncated  bool   `json:"stderr_truncated,omitempty"`
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
			guest.stopAndWait(3 * time.Second)
			status := "canceled"
			exitCode := 130
			if runCtx.Err() == context.DeadlineExceeded {
				status = "timed_out"
				exitCode = 124
			}
			out.ContextStatus = "closed"
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
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > mcpMaxTimeoutSeconds {
		return "", fmt.Errorf("timeout_seconds must be between 0 and %d", mcpMaxTimeoutSeconds)
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
	exitCode        int
	stdout          []byte
	stderr          []byte
	stdoutTotal     int64
	stderrTotal     int64
	stdoutTruncated bool
	stderrTruncated bool
}

func (g *mcpGuestContext) serve(ctx context.Context, control *client.Client) {
	err := control.RunInteractiveStreamInContext(ctx, g.vmID, client.RunRequest{
		Command: []string{"/bin/sh"}, Env: append([]string(nil), g.env...), WorkDir: g.workDir, User: g.user, ControlFD: true,
	}, g.inputs, func(event client.ExecEvent) error {
		select {
		case g.events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
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
	token, err := randomMCPID("marker")
	if err != nil {
		return mcpContextResult{}, err
	}
	marker := []byte("\x1e" + token + ":")
	script := mcpContextCommandScript(token, line)
	select {
	case g.inputs <- client.ExecInput{Kind: "stdin", Data: []byte(script)}:
	case <-g.done:
		return mcpContextResult{}, g.closedError()
	case <-ctx.Done():
		return mcpContextResult{}, ctx.Err()
	}
	g.mu.Lock()
	initial := append([]byte(nil), g.carry...)
	g.carry = nil
	g.mu.Unlock()
	scan := append([]byte(nil), initial...)
	var result mcpContextResult
	for {
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
		select {
		case event := <-g.events:
			data := event.Data
			if len(data) == 0 && event.Output != "" {
				data = []byte(event.Output)
			}
			switch event.Kind {
			case "stdout":
				result.stdout, result.stdoutTotal, result.stdoutTruncated = appendCommandOutput(result.stdout, result.stdoutTotal, result.stdoutTruncated, data)
			case "stderr":
				result.stderr, result.stderrTotal, result.stderrTruncated = appendCommandOutput(result.stderr, result.stderrTotal, result.stderrTruncated, data)
			case "output":
				if event.Stream == "stderr" {
					result.stderr, result.stderrTotal, result.stderrTruncated = appendCommandOutput(result.stderr, result.stderrTotal, result.stderrTruncated, data)
				} else {
					result.stdout, result.stdoutTotal, result.stdoutTruncated = appendCommandOutput(result.stdout, result.stdoutTotal, result.stdoutTruncated, data)
				}
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

func mcpContextCommandScript(token, line string) string {
	statusVar := "__vmsh_mcp_status_" + strings.TrimPrefix(token, "marker_")
	// Preserve the protocol descriptor before evaluating user shell state.
	// Commands may legitimately close or redirect fd 3; the saved descriptor
	// carries the status frame and then restores fd 3 before the next prompt.
	// Hide fd 9 while the user command runs so it cannot overwrite the backup.
	return "exec 9>&3\nif {\n" + line + "\n} 9>&-; then\n" + statusVar + "=0\nelse\n" + statusVar + "=$?\nfi\n/usr/bin/printf '\\036" + token + ":%s\\037\\n' \"$" + statusVar + "\" >&9\nexec 3>&9 9>&-\n"
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

func (g *mcpGuestContext) stopAndWait(timeout time.Duration) {
	g.stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-g.done:
	case <-timer.C:
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
	if utf8.Valid(data) {
		*text = string(data)
	} else if len(data) != 0 {
		*encoded = base64.StdEncoding.EncodeToString(data)
	}
}
