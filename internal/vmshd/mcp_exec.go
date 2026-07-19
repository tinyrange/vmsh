package vmshd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"j5.nz/cc/client"
)

const (
	mcpMaxCommandStreamBytes = 4 << 20
	mcpDefaultOutputChunk    = 1 << 20
	mcpMaxOutputChunk        = 4 << 20
	mcpMaxCombinedOutput     = 64 << 10
	mcpDefaultWaitSeconds    = 20
	mcpMaxWaitSeconds        = 30
	// Managed input is base64-encoded in JSON before crossing guest vsock. Keep
	// the encoded frame below the guest receive window.
	mcpStdinChunkBytes = 16 << 10
	mcpMaxStdinBytes   = 4 << 20
	mcpMaxCommandBytes = 256 << 10
	mcpMaxEnvBytes     = 256 << 10
	mcpMaxPathBytes    = 16 << 10
)

type mcpRunVMInput struct {
	VMID           string   `json:"vm_id" jsonschema:"ID returned by vm_create"`
	Command        []string `json:"command" jsonschema:"command and arguments to execute without a shell; total encoded argument bytes are capped at 256 KiB"`
	Env            []string `json:"env,omitempty" jsonschema:"environment entries in NAME=value form; total bytes are capped at 256 KiB"`
	WorkDir        string   `json:"workdir,omitempty" jsonschema:"working directory inside the guest; defaults to / for built-in BSD guests and /home/cc otherwise"`
	User           string   `json:"user,omitempty" jsonschema:"guest user name or uid[:gid]; built-in BSD guests currently support only root; defaults to 1000:1000 otherwise; use root for package installation"`
	Stdin          string   `json:"stdin,omitempty" jsonschema:"UTF-8 standard input sent to the command; capped at 4 MiB"`
	StdinBase64    string   `json:"stdin_base64,omitempty" jsonschema:"base64-encoded binary standard input capped at 4 MiB decoded; mutually exclusive with stdin"`
	TimeoutSeconds float64  `json:"timeout_seconds,omitempty" jsonschema:"guest command deadline in seconds; zero means no command deadline"`
}

type mcpOutputChunk struct {
	Text       string `json:"text,omitempty"`
	Base64     string `json:"base64,omitempty"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	TotalBytes int64  `json:"total_bytes"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type mcpCommandOutput struct {
	CommandID         string          `json:"command_id"`
	VMID              string          `json:"vm_id"`
	ContextID         string          `json:"context_id,omitempty"`
	Status            string          `json:"status"`
	ExitCode          *int            `json:"exit_code,omitempty"`
	Stdout            mcpOutputChunk  `json:"stdout"`
	Stderr            mcpOutputChunk  `json:"stderr"`
	AsyncStdout       *mcpOutputChunk `json:"async_stdout,omitempty"`
	AsyncStderr       *mcpOutputChunk `json:"async_stderr,omitempty"`
	Output            string          `json:"output,omitempty"`
	OutputBase64      string          `json:"output_base64,omitempty"`
	Error             string          `json:"error,omitempty"`
	ContainmentAction string          `json:"containment_action,omitempty"`
	ContainmentError  string          `json:"containment_error,omitempty"`
	StartedAt         time.Time       `json:"started_at"`
	FinishedAt        *time.Time      `json:"finished_at,omitempty"`
}

type mcpCommand struct {
	id        string
	vmID      string
	contextID string
	request   client.RunRequest
	stdin     []byte
	inputs    chan client.ExecInput
	cancel    context.CancelFunc
	done      chan struct{}

	mu                   sync.Mutex
	status               string
	exitCode             *int
	stdout               []byte
	stderr               []byte
	stdoutTotal          int64
	stderrTotal          int64
	stdoutTruncated      bool
	stderrTruncated      bool
	timedOut             bool
	asyncStdout          []byte
	asyncStderr          []byte
	asyncStdoutTotal     int64
	asyncStderrTotal     int64
	asyncStdoutTruncated bool
	asyncStderrTruncated bool
	err                  string
	startedAt            time.Time
	finishedAt           *time.Time
	cancelRequested      bool
	cancelOnce           sync.Once
	terminateOverride    func()
	containmentAction    string
	containmentError     string
}

func (e *mcpEndpoint) runVM(ctx context.Context, _ *mcp.CallToolRequest, in mcpRunVMInput) (*mcp.CallToolResult, mcpCommandOutput, error) {
	command, err := e.startCommand(in)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	select {
	case <-command.done:
		return nil, command.snapshot(0, 0, mcpMaxOutputChunk, true), nil
	case <-ctx.Done():
		command.requestCancel()
		select {
		case <-command.done:
		case <-time.After(3 * time.Second):
		}
		return nil, command.snapshot(0, 0, mcpMaxOutputChunk, true), ctx.Err()
	}
}

func (e *mcpEndpoint) startVMCommand(_ context.Context, _ *mcp.CallToolRequest, in mcpRunVMInput) (*mcp.CallToolResult, mcpCommandOutput, error) {
	command, err := e.startCommand(in)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	return nil, command.snapshot(0, 0, mcpDefaultOutputChunk, false), nil
}

type mcpCommandStatusInput struct {
	CommandID    string `json:"command_id" jsonschema:"ID returned by vm_exec_start, vm_run, or vm_context_exec_start"`
	StdoutOffset int64  `json:"stdout_offset,omitempty" jsonschema:"next stdout byte offset previously returned"`
	StderrOffset int64  `json:"stderr_offset,omitempty" jsonschema:"next stderr byte offset previously returned"`
	MaxBytes     int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes returned per stream"`
}

func (e *mcpEndpoint) statusVMCommand(_ context.Context, _ *mcp.CallToolRequest, in mcpCommandStatusInput) (*mcp.CallToolResult, mcpCommandOutput, error) {
	command, err := e.command(in.CommandID)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	maxBytes, err := mcpOutputChunkSize(in.MaxBytes)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	if in.StdoutOffset < 0 || in.StderrOffset < 0 {
		return nil, mcpCommandOutput{}, fmt.Errorf("output offsets must be non-negative")
	}
	return nil, command.snapshot(in.StdoutOffset, in.StderrOffset, maxBytes, false), nil
}

type mcpCommandWaitInput struct {
	CommandID    string  `json:"command_id" jsonschema:"ID returned by vm_exec_start, vm_run, or vm_context_exec_start"`
	WaitSeconds  float64 `json:"wait_seconds,omitempty" jsonschema:"maximum seconds to wait in this call; defaults to 20 and is capped at 30"`
	StdoutOffset int64   `json:"stdout_offset,omitempty"`
	StderrOffset int64   `json:"stderr_offset,omitempty"`
	MaxBytes     int     `json:"max_bytes,omitempty"`
}

func (e *mcpEndpoint) waitVMCommand(ctx context.Context, _ *mcp.CallToolRequest, in mcpCommandWaitInput) (*mcp.CallToolResult, mcpCommandOutput, error) {
	command, err := e.command(in.CommandID)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	maxBytes, err := mcpOutputChunkSize(in.MaxBytes)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	if in.StdoutOffset < 0 || in.StderrOffset < 0 {
		return nil, mcpCommandOutput{}, fmt.Errorf("output offsets must be non-negative")
	}
	wait := in.WaitSeconds
	if wait == 0 {
		wait = mcpDefaultWaitSeconds
	}
	if wait < 0 || wait > mcpMaxWaitSeconds {
		return nil, mcpCommandOutput{}, fmt.Errorf("wait_seconds must be between 0 and %d", mcpMaxWaitSeconds)
	}
	timer := time.NewTimer(time.Duration(wait * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-command.done:
	case <-timer.C:
	case <-ctx.Done():
		return nil, mcpCommandOutput{}, ctx.Err()
	}
	return nil, command.snapshot(in.StdoutOffset, in.StderrOffset, maxBytes, false), nil
}

type mcpCommandCancelInput struct {
	CommandID    string `json:"command_id" jsonschema:"ID returned by vm_exec_start, vm_run, or vm_context_exec_start"`
	StdoutOffset int64  `json:"stdout_offset,omitempty" jsonschema:"next stdout byte offset previously returned"`
	StderrOffset int64  `json:"stderr_offset,omitempty" jsonschema:"next stderr byte offset previously returned"`
	MaxBytes     int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes returned per stream"`
}

func (e *mcpEndpoint) cancelVMCommand(ctx context.Context, _ *mcp.CallToolRequest, in mcpCommandCancelInput) (*mcp.CallToolResult, mcpCommandOutput, error) {
	command, err := e.command(in.CommandID)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	maxBytes, err := mcpOutputChunkSize(in.MaxBytes)
	if err != nil {
		return nil, mcpCommandOutput{}, err
	}
	if in.StdoutOffset < 0 || in.StderrOffset < 0 {
		return nil, mcpCommandOutput{}, fmt.Errorf("output offsets must be non-negative")
	}
	command.requestCancel()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-command.done:
	case <-timer.C:
	case <-ctx.Done():
		return nil, mcpCommandOutput{}, ctx.Err()
	}
	return nil, command.snapshot(in.StdoutOffset, in.StderrOffset, maxBytes, false), nil
}

func (e *mcpEndpoint) startCommand(in mcpRunVMInput) (*mcpCommand, error) {
	vm, err := e.ownedVM(in.VMID)
	if err != nil {
		return nil, err
	}
	id := vm.ID
	if len(in.Command) == 0 || strings.TrimSpace(in.Command[0]) == "" {
		return nil, fmt.Errorf("command is required")
	}
	if err := validateMCPStringBytes("command", in.Command, mcpMaxCommandBytes); err != nil {
		return nil, err
	}
	if err := validateMCPStringBytes("environment", in.Env, mcpMaxEnvBytes); err != nil {
		return nil, err
	}
	if len(in.WorkDir) > mcpMaxPathBytes {
		return nil, fmt.Errorf("workdir exceeds the %d-byte limit", mcpMaxPathBytes)
	}
	if err := validateMCPDurationSeconds("timeout_seconds", in.TimeoutSeconds); err != nil {
		return nil, err
	}
	stdin, err := mcpCommandStdin(in.Stdin, in.StdinBase64)
	if err != nil {
		return nil, err
	}
	user, err := mcpGuestUser(vm, in.User)
	if err != nil {
		return nil, err
	}
	workDir := mcpGuestWorkDir(vm, in.WorkDir)
	commandID, err := randomMCPID("cmd")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	command := &mcpCommand{
		id: commandID, vmID: id, cancel: cancel, done: make(chan struct{}), status: "running", startedAt: time.Now().UTC(),
		request: client.RunRequest{Command: append([]string(nil), in.Command...), Env: append([]string(nil), in.Env...), WorkDir: workDir, User: user, TimeoutSeconds: in.TimeoutSeconds},
		stdin:   stdin, inputs: make(chan client.ExecInput),
	}
	if _, err := e.registerMCPCommand(command); err != nil {
		cancel()
		return nil, err
	}
	go command.run(ctx, e.control)
	return command, nil
}

func (e *mcpEndpoint) registerMCPCommand(command *mcpCommand) (*mcpGuestContext, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("MCP endpoint is stopped")
	}
	var guest *mcpGuestContext
	if command.contextID != "" {
		guest = e.contexts[command.contextID]
		if guest == nil {
			return nil, fmt.Errorf("context %q is not owned by this MCP session", command.contextID)
		}
		command.vmID = guest.vmID
		select {
		case <-guest.done:
			return nil, fmt.Errorf("context %q is closed", command.contextID)
		default:
		}
		command.terminateOverride = guest.stop
	}
	if _, ok := e.vms[command.vmID]; !ok {
		return nil, fmt.Errorf("VM %q is not owned by this MCP session", command.vmID)
	}
	if _, ok := e.stopping[command.vmID]; ok {
		return nil, fmt.Errorf("VM %q is stopping", command.vmID)
	}
	e.commands[command.id] = command
	return guest, nil
}

func (e *mcpEndpoint) command(id string) (*mcpCommand, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	command := e.commands[id]
	e.mu.Unlock()
	if command == nil {
		return nil, fmt.Errorf("command %q is not owned by this MCP session", id)
	}
	return command, nil
}

func (c *mcpCommand) run(ctx context.Context, control *client.Client) {
	onEvent := func(event client.ExecEvent) error {
		c.accept(event)
		return nil
	}
	go streamMCPCommandStdin(ctx, c.inputs, c.stdin)
	err := control.RunInteractiveStreamInContext(ctx, c.vmID, c.request, c.inputs, onEvent)
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	defer close(c.done)
	c.finishedAt = &now
	if c.cancelRequested || (ctx.Err() != nil && c.status == "running") {
		c.status = "canceled"
		code := 130
		c.exitCode = &code
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			c.err = conciseCommandError(err)
		}
		return
	}
	if err != nil {
		c.status = "failed"
		c.err = conciseCommandError(err)
		return
	}
	if c.exitCode == nil {
		c.status = "failed"
		c.err = "command stream ended without an exit status"
		return
	}
	if c.timedOut {
		c.status = "timed_out"
		return
	}
	c.status = "exited"
}

func streamMCPCommandStdin(ctx context.Context, inputs chan<- client.ExecInput, stdin []byte) {
	for len(stdin) > 0 {
		n := len(stdin)
		if n > mcpStdinChunkBytes {
			n = mcpStdinChunkBytes
		}
		chunk := append([]byte(nil), stdin[:n]...)
		select {
		case inputs <- client.ExecInput{Kind: "stdin", Data: chunk}:
			stdin = stdin[n:]
		case <-ctx.Done():
			return
		}
	}
	select {
	case inputs <- client.ExecInput{Kind: "stdin_close"}:
	case <-ctx.Done():
	}
}

func (c *mcpCommand) accept(event client.ExecEvent) {
	data := event.Data
	if len(data) == 0 && event.Output != "" {
		data = []byte(event.Output)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch event.Kind {
	case "stdout":
		c.stdout, c.stdoutTotal, c.stdoutTruncated = appendCommandOutput(c.stdout, c.stdoutTotal, c.stdoutTruncated, data)
	case "stderr":
		c.stderr, c.stderrTotal, c.stderrTruncated = appendCommandOutput(c.stderr, c.stderrTotal, c.stderrTruncated, data)
	case "output":
		if event.Stream == "stderr" {
			c.stderr, c.stderrTotal, c.stderrTruncated = appendCommandOutput(c.stderr, c.stderrTotal, c.stderrTruncated, data)
		} else {
			c.stdout, c.stdoutTotal, c.stdoutTruncated = appendCommandOutput(c.stdout, c.stdoutTotal, c.stdoutTruncated, data)
		}
	case "exit":
		code := event.ExitCode
		c.exitCode = &code
	case "timeout":
		c.timedOut = true
	case "error":
		c.err = firstNonEmpty(event.Error, event.Output, "guest command failed")
	}
}

func (c *mcpCommand) requestCancel() {
	c.mu.Lock()
	if c.status == "running" {
		c.cancelRequested = true
		c.cancelOnce.Do(func() { go c.terminate() })
	}
	c.mu.Unlock()
}

func (c *mcpCommand) terminate() {
	if c.terminateOverride != nil {
		c.terminateOverride()
		if !c.waitDone(3 * time.Second) {
			c.markTerminationUnconfirmed("persistent context did not close after cancellation; VM was retained with its filesystem intact")
			c.cancel()
		}
		return
	}
	if !c.sendInput(client.ExecInput{Kind: "signal", Signal: "TERM"}, 500*time.Millisecond) {
		c.markTerminationUnconfirmed("guest command stream did not accept TERM; VM was retained with its filesystem intact and the command may still be running")
		c.cancel()
		return
	}
	if c.waitDone(500 * time.Millisecond) {
		return
	}
	if !c.sendInput(client.ExecInput{Kind: "signal", Signal: "KILL"}, 500*time.Millisecond) {
		c.markTerminationUnconfirmed("guest command stream did not accept KILL; VM was retained with its filesystem intact and the command may still be running")
		c.cancel()
		return
	}
	if c.waitDone(2500 * time.Millisecond) {
		return
	}
	c.markTerminationUnconfirmed("guest did not confirm command termination after TERM and KILL; VM was retained with its filesystem intact and the command may still be running")
	c.cancel()
}

func (c *mcpCommand) markTerminationUnconfirmed(message string) {
	c.mu.Lock()
	if c.status != "running" {
		c.mu.Unlock()
		return
	}
	c.containmentAction = "VM retained after unconfirmed command cancellation"
	c.containmentError = message
	c.err = message
	c.mu.Unlock()
}

func (c *mcpCommand) sendInput(input client.ExecInput, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case c.inputs <- input:
		return true
	case <-c.done:
		return false
	case <-timer.C:
		return false
	}
}

func (c *mcpCommand) waitDone(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.done:
		return true
	case <-timer.C:
		return false
	}
}

func (c *mcpCommand) snapshot(stdoutOffset, stderrOffset int64, maxBytes int, includeCombined bool) mcpCommandOutput {
	c.mu.Lock()
	defer c.mu.Unlock()
	stdout := commandOutputChunk(c.stdout, c.stdoutTotal, c.stdoutTruncated, stdoutOffset, maxBytes)
	stderr := commandOutputChunk(c.stderr, c.stderrTotal, c.stderrTruncated, stderrOffset, maxBytes)
	out := mcpCommandOutput{
		CommandID: c.id, VMID: c.vmID, ContextID: c.contextID, Status: c.status, ExitCode: cloneInt(c.exitCode), Stdout: stdout, Stderr: stderr,
		Error: c.err, ContainmentAction: c.containmentAction, ContainmentError: c.containmentError,
		StartedAt: c.startedAt, FinishedAt: cloneTime(c.finishedAt),
	}
	if c.asyncStdoutTotal != 0 {
		chunk := commandOutputChunk(c.asyncStdout, c.asyncStdoutTotal, c.asyncStdoutTruncated, 0, mcpMaxOutputChunk)
		out.AsyncStdout = &chunk
	}
	if c.asyncStderrTotal != 0 {
		chunk := commandOutputChunk(c.asyncStderr, c.asyncStderrTotal, c.asyncStderrTruncated, 0, mcpMaxOutputChunk)
		out.AsyncStderr = &chunk
	}
	if includeCombined {
		combined := make([]byte, 0, len(c.stdout)+len(c.stderr))
		combined = append(combined, c.stdout...)
		combined = append(combined, c.stderr...)
		if len(combined) > mcpMaxCombinedOutput {
			return out
		}
		if efficientTextOutput(combined) {
			out.Output = string(combined)
		} else if len(combined) > 0 {
			out.OutputBase64 = base64.StdEncoding.EncodeToString(combined)
		}
	}
	return out
}

func appendCommandOutput(dst []byte, total int64, truncated bool, data []byte) ([]byte, int64, bool) {
	total += int64(len(data))
	remaining := mcpMaxCommandStreamBytes - len(dst)
	if remaining <= 0 {
		return dst, total, truncated || len(data) > 0
	}
	if len(data) > remaining {
		dst = append(dst, data[:remaining]...)
		return dst, total, true
	}
	return append(dst, data...), total, truncated
}

func commandOutputChunk(data []byte, total int64, truncated bool, offset int64, maxBytes int) mcpOutputChunk {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	end := offset + int64(maxBytes)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	chunk := append([]byte(nil), data[offset:end]...)
	out := mcpOutputChunk{Offset: offset, NextOffset: end, TotalBytes: total, Truncated: truncated}
	if efficientTextOutput(chunk) {
		out.Text = string(chunk)
	} else if len(chunk) > 0 {
		out.Base64 = base64.StdEncoding.EncodeToString(chunk)
	}
	return out
}

func efficientTextOutput(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	encoded, err := json.Marshal(string(data))
	if err != nil {
		return false
	}
	return len(encoded) <= base64.StdEncoding.EncodedLen(len(data))+2
}

func mcpCommandStdin(text, encoded string) ([]byte, error) {
	if text != "" && encoded != "" {
		return nil, fmt.Errorf("stdin and stdin_base64 are mutually exclusive")
	}
	if encoded == "" {
		if len(text) > mcpMaxStdinBytes {
			return nil, fmt.Errorf("stdin exceeds the %d-byte limit", mcpMaxStdinBytes)
		}
		return []byte(text), nil
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > mcpMaxStdinBytes {
		return nil, fmt.Errorf("stdin_base64 exceeds the %d-byte decoded limit", mcpMaxStdinBytes)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode stdin_base64: %w", err)
	}
	if len(data) > mcpMaxStdinBytes {
		return nil, fmt.Errorf("stdin_base64 exceeds the %d-byte decoded limit", mcpMaxStdinBytes)
	}
	return data, nil
}

func validateMCPStringBytes(field string, values []string, limit int) error {
	total := 0
	for _, value := range values {
		if len(value) > limit-total {
			return fmt.Errorf("%s exceeds the %d-byte limit", field, limit)
		}
		total += len(value)
	}
	return nil
}

func mcpOutputChunkSize(value int) (int, error) {
	if value == 0 {
		return mcpDefaultOutputChunk, nil
	}
	if value < 0 || value > mcpMaxOutputChunk {
		return 0, fmt.Errorf("max_bytes must be between 1 and %d", mcpMaxOutputChunk)
	}
	return value, nil
}

func conciseCommandError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if idx := strings.Index(message, "Post \""); idx >= 0 {
		if colon := strings.LastIndex(message, ": "); colon >= 0 {
			message = message[colon+2:]
		}
	}
	return message
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
