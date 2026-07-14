//go:build windows

package vmshd

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func configureHostShellCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func terminateHostShellProcess(cmd *exec.Cmd, exited <-chan struct{}, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	_ = exec.CommandContext(ctx, "taskkill", "/PID", pid, "/T").Run()
	cancel()
	select {
	case <-exited:
		return
	case <-time.After(grace):
	}
	ctx, cancel = context.WithTimeout(context.Background(), grace)
	_ = exec.CommandContext(ctx, "taskkill", "/PID", pid, "/T", "/F").Run()
	cancel()
}
