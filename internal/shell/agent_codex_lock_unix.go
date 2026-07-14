//go:build !windows

package shell

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func acquireCodexFileLock(path string, timeout time.Duration) (*codexFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			lock, lockErr := newCodexFileLock(file, func() error {
				return unix.Flock(int(file.Fd()), unix.LOCK_UN)
			})
			if lockErr != nil {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
				return nil, lockErr
			}
			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, errCodexLockTimeout
		}
		time.Sleep(100 * time.Millisecond)
	}
}
