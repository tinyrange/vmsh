//go:build !windows

package vmshd

import "os"

func replaceMCPHostFile(source, destination string) error {
	return os.Rename(source, destination)
}
