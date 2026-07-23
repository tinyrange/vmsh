//go:build !windows

package shell

import "syscall"

func execProcess(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
