//go:build windows

package shell

import "os"

func setNonblockForTest(file *os.File) error {
	return nil
}
