//go:build windows

package shell

import (
	"os"

	"golang.org/x/sys/windows"
)

func withSSHKnownHostsLock(path string, fn func() error) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		return err
	}
	defer windows.UnlockFileEx(windows.Handle(lock.Fd()), 0, 1, 0, overlapped)
	return fn()
}
