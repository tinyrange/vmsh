//go:build windows

package shell

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
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
	overlapped := new(windows.Overlapped)
	deadline := time.Now().Add(timeout)
	for {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if err == nil {
			lock, lockErr := newCodexFileLock(file, func() error {
				return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
			})
			if lockErr != nil {
				_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
				_ = file.Close()
				return nil, lockErr
			}
			return lock, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
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
