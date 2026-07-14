//go:build linux || darwin

package shell

import (
	"errors"
	"os/exec"
	"syscall"
)

func externalInterrupted(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGINT
}
