package shell

import (
	"fmt"
	"io"

	"j5.nz/cc/client"
)

type shellTarget interface {
	Context() commandContext
	Mode() shellMode
	Run(line string, stdout, stderr io.Writer) error
	RunWithInput(line string, stdin io.Reader, stdout, stderr io.Writer) error
	PrepareRunWithInput(line string, stderr io.Writer) (preparedTargetCommand, error)
	CurrentCWD() string
	Chdir(target string) error
	ResolveCopyPath(value string) string
	LocalPath(targetPath string) (string, bool)
	CopyFromLocal(src string, dst copyTargetPath, stderr io.Writer) error
	CopyToLocal(src copyTargetPath, dst string, stderr io.Writer) error
}

type copyTargetPath struct {
	path      string
	directory bool
}

type preparedTargetCommand interface {
	RunWithInput(stdin io.Reader, stdout, stderr io.Writer) error
}

type hostShellTarget struct {
	shell *shellState
	ctx   commandContext
}

func (t hostShellTarget) Context() commandContext { return t.ctx }

func (t hostShellTarget) Mode() shellMode { return modeHost }

func (t hostShellTarget) Run(line string, stdout, stderr io.Writer) error {
	return t.shell.runHost(line, stdout, stderr)
}

func (t hostShellTarget) RunWithInput(line string, stdin io.Reader, stdout, stderr io.Writer) error {
	return t.shell.runHostWithInput(line, stdin, stdout, stderr)
}

func (t hostShellTarget) PrepareRunWithInput(line string, stderr io.Writer) (preparedTargetCommand, error) {
	return hostPreparedCommand{target: t, line: line}, nil
}

func (t hostShellTarget) CurrentCWD() string { return t.shell.hostCWD }

func (t hostShellTarget) Chdir(target string) error { return t.shell.chdirHost(target) }

func (t hostShellTarget) ResolveCopyPath(value string) string {
	return t.shell.resolveHostCopyPath(value)
}

func (t hostShellTarget) LocalPath(targetPath string) (string, bool) { return targetPath, true }

func (t hostShellTarget) CopyFromLocal(src string, dst copyTargetPath, stderr io.Writer) error {
	return copyHostPath(src, dst.path)
}

func (t hostShellTarget) CopyToLocal(src copyTargetPath, dst string, stderr io.Writer) error {
	return copyHostPath(src.path, dst)
}

type hostPreparedCommand struct {
	target hostShellTarget
	line   string
}

func (c hostPreparedCommand) RunWithInput(stdin io.Reader, stdout, stderr io.Writer) error {
	return c.target.RunWithInput(c.line, stdin, stdout, stderr)
}

type guestShellTarget struct {
	shell *shellState
	ctx   commandContext
}

func (t guestShellTarget) Context() commandContext { return t.ctx }

func (t guestShellTarget) Mode() shellMode { return modeVM }

func (t guestShellTarget) Run(line string, stdout, stderr io.Writer) error {
	return t.shell.runGuest(t.ctx, line, stdout, stderr)
}

func (t guestShellTarget) RunWithInput(line string, stdin io.Reader, stdout, stderr io.Writer) error {
	req, err := t.shell.prepareGuestRunRequest(t.ctx, line, false, 0, 0, stderr)
	if err != nil {
		return err
	}
	return t.shell.streamGuestRunWithInput(backendVMID(t.ctx), req, stdin, stdout, stderr)
}

func (t guestShellTarget) PrepareRunWithInput(line string, stderr io.Writer) (preparedTargetCommand, error) {
	req, err := t.shell.prepareGuestRunRequest(t.ctx, line, false, 0, 0, stderr)
	if err != nil {
		return nil, err
	}
	return guestPreparedCommand{shell: t.shell, ctx: t.ctx, req: req}, nil
}

func (t guestShellTarget) CurrentCWD() string { return t.shell.currentGuestCWD(t.ctx) }

func (t guestShellTarget) Chdir(target string) error {
	return t.shell.chdirGuestContext(t.ctx, target)
}

func (t guestShellTarget) ResolveCopyPath(value string) string {
	return t.shell.resolveGuestCopyPath(t.ctx, value)
}

func (t guestShellTarget) LocalPath(targetPath string) (string, bool) {
	if t.ctx.Isolated {
		return "", false
	}
	return guestHostPathToHost(t.shell.hostCWD, targetPath)
}

func (t guestShellTarget) CopyFromLocal(src string, dst copyTargetPath, stderr io.Writer) error {
	return t.shell.copyLocalToGuest(src, t.ctx, dst, stderr)
}

func (t guestShellTarget) CopyToLocal(src copyTargetPath, dst string, stderr io.Writer) error {
	return t.shell.copyGuestToLocal(t.ctx, src, dst, stderr)
}

type sshShellTarget struct {
	shell *shellState
	ctx   commandContext
}

func (t sshShellTarget) Context() commandContext { return t.ctx }

func (t sshShellTarget) Mode() shellMode { return modeSSH }

func (t sshShellTarget) Run(line string, stdout, stderr io.Writer) error {
	return t.shell.runSSH(t.ctx, line, stdout, stderr)
}

func (t sshShellTarget) RunWithInput(line string, stdin io.Reader, stdout, stderr io.Writer) error {
	return t.shell.runSSHWithInput(t.ctx, line, stdin, stdout, stderr)
}

func (t sshShellTarget) PrepareRunWithInput(line string, stderr io.Writer) (preparedTargetCommand, error) {
	return sshPreparedCommand{target: t, line: line}, nil
}

func (t sshShellTarget) CurrentCWD() string { return t.shell.currentSSHCWD(t.ctx) }

func (t sshShellTarget) Chdir(target string) error {
	return t.shell.chdirSSHContext(t.ctx, target)
}

func (t sshShellTarget) ResolveCopyPath(value string) string {
	return t.shell.resolveSSHCopyPath(t.ctx, value)
}

func (t sshShellTarget) LocalPath(targetPath string) (string, bool) { return "", false }

func (t sshShellTarget) CopyFromLocal(src string, dst copyTargetPath, stderr io.Writer) error {
	return t.shell.copyLocalToSSH(src, t.ctx, dst, stderr)
}

func (t sshShellTarget) CopyToLocal(src copyTargetPath, dst string, stderr io.Writer) error {
	return t.shell.copySSHToLocal(t.ctx, src, dst, stderr)
}

type sshPreparedCommand struct {
	target sshShellTarget
	line   string
}

func (c sshPreparedCommand) RunWithInput(stdin io.Reader, stdout, stderr io.Writer) error {
	return c.target.RunWithInput(c.line, stdin, stdout, stderr)
}

type guestPreparedCommand struct {
	shell *shellState
	ctx   commandContext
	req   client.RunRequest
}

func (c guestPreparedCommand) RunWithInput(stdin io.Reader, stdout, stderr io.Writer) error {
	return c.shell.streamGuestRunWithInput(backendVMID(c.ctx), c.req, stdin, stdout, stderr)
}

func (s *shellState) targetFor(ctx commandContext) (shellTarget, error) {
	switch ctx.Mode {
	case modeHost:
		return hostShellTarget{shell: s, ctx: ctx}, nil
	case modeVM:
		return guestShellTarget{shell: s, ctx: ctx}, nil
	case modeSSH:
		return sshShellTarget{shell: s, ctx: ctx}, nil
	default:
		return nil, fmt.Errorf("unknown shell mode %q", ctx.Mode)
	}
}
