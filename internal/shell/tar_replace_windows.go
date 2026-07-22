//go:build windows

package shell

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func replaceHostRootFile(root *os.Root, source, destination string) error {
	from, err := windows.UTF16PtrFromString(filepath.Join(root.Name(), source))
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(filepath.Join(root.Name(), destination))
	if err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if err == nil {
			return nil
		}
		if (!errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION)) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
