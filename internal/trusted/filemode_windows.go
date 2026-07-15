//go:build windows

package trusted

import (
	"os"
	"path/filepath"
	"strings"
)

func fileAccessibleByOthers(info os.FileInfo) bool { return true }

func ownerOnlyFilesSupported() bool { return false }

func fileIsExecutable(path string, _ os.FileInfo) bool {
	extension := strings.ToLower(filepath.Ext(path))
	return extension == ".exe" || extension == ".com"
}
