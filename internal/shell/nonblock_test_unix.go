//go:build !windows

package shell

import (
	"os"
	"syscall"
)

func setNonblockForTest(file *os.File) error {
	return syscall.SetNonblock(int(file.Fd()), true)
}
