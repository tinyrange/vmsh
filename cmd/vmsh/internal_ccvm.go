//go:build embed_ccvm

package main

import (
	"os"

	"github.com/tinyrange/vmsh/internal/backend"
	"github.com/tinyrange/vmsh/internal/vmshd"
)

func bundledCCVMAvailable() bool {
	return true
}

func runInternalCCVMFromEnv() bool {
	if os.Getenv(backend.InternalVMSHDEnv) == "1" {
		_ = os.Setenv(backend.InternalCCVMSidecarModeEnv, backend.InternalCCVMSidecarMode)
		vmshd.Main(os.Args[1:])
		return true
	}
	return false
}
