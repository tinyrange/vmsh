//go:build windows

package group

import "os"

func fileAccessibleByOthers(info os.FileInfo) bool {
	return false
}
