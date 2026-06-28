//go:build embed_ccvm

package shell

func bundledCCVMAvailable() bool {
	return true
}

func defaultDaemonIdentity() string {
	return "ccprod"
}
