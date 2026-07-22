//go:build !windows

package shell

import "os"

func replaceHostRootFile(root *os.Root, source, destination string) error {
	return root.Rename(source, destination)
}
