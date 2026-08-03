//go:build windows

package desktopapp

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func hostFreeBytes(path string) (int64, error) {
	current := filepath.Clean(path)
	for {
		pointer, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return 0, err
		}
		var available uint64
		if err := windows.GetDiskFreeSpaceEx(pointer, &available, nil, nil); err == nil {
			return int64(available), nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return 0, fs.ErrNotExist
		}
		current = parent
	}
}
