//go:build darwin

package desktopapp

import "golang.org/x/sys/unix"

func hostMemoryMB() uint64 {
	value, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return value >> 20
}
