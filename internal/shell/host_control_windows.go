//go:build windows

package shell

import (
	"fmt"
	"os"
)

func openPersistentHostControl() (*os.File, string, func(), error) {
	return nil, "", nil, fmt.Errorf("persistent host shells are unavailable on Windows")
}
