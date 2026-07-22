//go:build !windows

package shell

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

func openPersistentHostControl() (*os.File, string, func(), error) {
	dir, err := os.MkdirTemp("", "vmsh-host-shell-")
	if err != nil {
		return nil, "", nil, err
	}
	cleanupDir := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "control")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		cleanupDir()
		return nil, "", nil, err
	}
	reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		cleanupDir()
		return nil, "", nil, err
	}
	keeper, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		_ = reader.Close()
		cleanupDir()
		return nil, "", nil, err
	}
	if err := syscall.SetNonblock(int(reader.Fd()), false); err != nil {
		_ = keeper.Close()
		_ = reader.Close()
		cleanupDir()
		return nil, "", nil, err
	}
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			// The distinct writer is a lifecycle signal: closing it wakes a
			// blocked reader with EOF on Linux, BSD, and Darwin.
			_ = keeper.Close()
			cleanupDir()
		})
	}
	return reader, path, cleanup, nil
}
