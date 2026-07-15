//go:build !windows

package vmshd

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func configureHostShellCommand(cmd *exec.Cmd) {
	// pty.StartWithSize creates a new session and controlling terminal, making
	// the shell the leader of an isolated process group.
}

func terminateHostShellProcess(cmd *exec.Cmd, exited <-chan struct{}, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGHUP); err != nil && !errors.Is(err, syscall.ESRCH) {
		return
	}
	softGrace := grace / 4
	if softGrace <= 0 {
		softGrace = time.Millisecond
	}
	timer := time.NewTimer(softGrace)
	defer timer.Stop()
	select {
	case <-exited:
	case <-timer.C:
	}
	// A login shell may exit on SIGHUP before all descendants do. Address the
	// group again so children cannot escape merely because the leader reaped.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer.Reset(softGrace)
	<-timer.C
	if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-exited:
	case <-time.After(grace):
	}
}
