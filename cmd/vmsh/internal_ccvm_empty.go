//go:build !embed_ccvm

package main

func bundledCCVMAvailable() bool {
	return false
}

func runInternalCCVMFromEnv() bool {
	return false
}
