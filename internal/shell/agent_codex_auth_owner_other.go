//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package shell

import "os"

func preserveCodexAuthOwnership(_ string, _ os.FileInfo) error {
	return nil
}
