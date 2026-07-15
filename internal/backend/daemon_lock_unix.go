//go:build !windows

package backend

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockDaemonFile(file *os.File) (func() error, bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return func() error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }, true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return nil, false, nil
	}
	return nil, false, err
}
