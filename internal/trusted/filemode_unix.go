//go:build !windows

package trusted

import "os"

func fileAccessibleByOthers(info os.FileInfo) bool { return info.Mode().Perm()&0o077 != 0 }

func ownerOnlyFilesSupported() bool { return true }

func fileIsExecutable(_ string, info os.FileInfo) bool { return info.Mode().Perm()&0o111 != 0 }
