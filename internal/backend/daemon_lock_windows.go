//go:build windows

package backend

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockDaemonFile(file *os.File) (func() error, bool, error) {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err == nil {
		return func() error {
			return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		}, true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil, false, nil
	}
	return nil, false, err
}
