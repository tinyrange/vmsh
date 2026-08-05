//go:build windows

package desktopapp

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type desktopMemoryStatus struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func hostMemoryMB() uint64 {
	var status desktopMemoryStatus
	status.length = uint32(unsafe.Sizeof(status))
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	ok, _, _ := procedure.Call(uintptr(unsafe.Pointer(&status)))
	if ok == 0 {
		return 0
	}
	return status.totalPhys >> 20
}
