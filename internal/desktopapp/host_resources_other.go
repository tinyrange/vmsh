//go:build !darwin && !linux && !windows

package desktopapp

func hostMemoryMB() uint64 {
	return 0
}
