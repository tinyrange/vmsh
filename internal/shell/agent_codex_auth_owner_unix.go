//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package shell

import (
	"os"
	"syscall"
)

func preserveCodexAuthOwnership(path string, previous os.FileInfo) error {
	previousStat, ok := previous.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	current, err := os.Stat(path)
	if err != nil {
		return err
	}
	currentStat, ok := current.Sys().(*syscall.Stat_t)
	if ok && currentStat.Uid == previousStat.Uid && currentStat.Gid == previousStat.Gid {
		return nil
	}
	return os.Chown(path, int(previousStat.Uid), int(previousStat.Gid))
}
