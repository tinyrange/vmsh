//go:build embed_ccvm

package main

import (
	"os"

	"github.com/tinyrange/vmsh/internal/backend"
	"j5.nz/cc/ccvmd"
)

func bundledCCVMAvailable() bool {
	return true
}

func runInternalCCVMFromEnv() bool {
	if os.Getenv(backend.InternalCCVMEnv) != "1" {
		return false
	}
	_ = os.Setenv(backend.InternalCCVMSidecarModeEnv, backend.InternalCCVMSidecarMode)
	ccvmd.Main(os.Args[1:])
	return true
}
