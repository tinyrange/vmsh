//go:build !windows

package desktopapp

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
