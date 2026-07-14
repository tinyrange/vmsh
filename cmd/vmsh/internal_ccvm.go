package main

import (
	"os"

	"github.com/tinyrange/vmsh/internal/backend"
	"github.com/tinyrange/vmsh/internal/vmshd"
	"github.com/tinyrange/vmsh/internal/vmshdprotocol"
)

func bundledCCVMAvailable() bool {
	return true
}

func runInternalCCVMFromEnv() bool {
	if os.Getenv(backend.InternalVMSHDEnv) == "1" || vmshdprotocol.IsDaemonExecutableName(os.Args[0]) {
		_ = os.Setenv(backend.InternalCCVMSidecarModeEnv, backend.InternalCCVMSidecarMode)
		vmshd.Main(os.Args[1:])
		return true
	}
	return false
}
