//go:build !embed_ccvm

package shell

func bundledCCVMAvailable() bool {
	return false
}

func defaultDaemonIdentity() string {
	return "ccdev"
}
