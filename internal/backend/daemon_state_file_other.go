//go:build !windows

package backend

import "os"

func replaceDaemonStateFile(src, dst string) error {
	return os.Rename(src, dst)
}

func syncDaemonStateDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
