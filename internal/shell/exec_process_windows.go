//go:build windows

package shell

import (
	"fmt"
	"os"
	"os/exec"
)

func execProcess(path string, argv, env []string) error {
	if len(argv) == 0 {
		argv = []string{path}
	}
	cmd := exec.Command(path)
	cmd.Args = append([]string(nil), argv...)
	cmd.Env = append([]string(nil), env...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start replacement process: %w", err)
	}
	return cmd.Process.Release()
}
