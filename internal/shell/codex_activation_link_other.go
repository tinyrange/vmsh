//go:build !windows

package shell

import "os"

func replaceCodexActivationLink(src, dst string) error {
	return os.Rename(src, dst)
}
