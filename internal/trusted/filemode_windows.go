//go:build windows

package trusted

import "os"

func fileAccessibleByOthers(info os.FileInfo) bool { return false }
