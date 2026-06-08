//go:build !windows

package backend

import (
	"os/exec"
	"syscall"
)

func detachDaemonCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
