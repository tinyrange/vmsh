//go:build !windows

package desktopapp

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func hostFreeBytes(path string) (int64, error) {
	current := filepath.Clean(path)
	for {
		var stat unix.Statfs_t
		if err := unix.Statfs(current, &stat); err == nil {
			return int64(stat.Bavail) * int64(stat.Bsize), nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			var stat unix.Statfs_t
			if err := unix.Statfs(parent, &stat); err != nil {
				return 0, err
			}
			return int64(stat.Bavail) * int64(stat.Bsize), nil
		}
		current = parent
	}
}
