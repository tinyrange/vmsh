//go:build embed_ccvm

package main

import (
	"os"

	"j5.nz/cc/ccvmd"
)

func bundledCCVMAvailable() bool {
	return true
}

func runInternalCCVMFromEnv() bool {
	if os.Getenv(internalCCVMEnv) != "1" {
		return false
	}
	ccvmd.Main(os.Args[1:])
	return true
}
