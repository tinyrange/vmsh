//go:build !windows

package ptyterm

import (
	"os/exec"

	"github.com/creack/pty"
)

func startPTY(cmd *exec.Cmd, size Size) (*ptyProcess, error) {
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(size.Rows), Cols: uint16(size.Cols)})
	if err != nil {
		return nil, err
	}
	return &ptyProcess{
		io: file,
		wait: func() Result {
			err := cmd.Wait()
			return Result{ExitCode: exitCode(err), Err: err}
		},
		resize: func(size Size) error {
			return pty.Setsize(file, &pty.Winsize{Rows: uint16(size.Rows), Cols: uint16(size.Cols)})
		},
		kill: func() error {
			if cmd.Process == nil {
				return nil
			}
			return cmd.Process.Kill()
		},
	}, nil
}
