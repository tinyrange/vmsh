//go:build !windows

package backend

import "os"

func platformReplaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
