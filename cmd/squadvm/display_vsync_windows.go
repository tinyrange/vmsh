//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	openGL32DLL          = syscall.NewLazyDLL("opengl32.dll")
	wglGetProcAddressAPI = openGL32DLL.NewProc("wglGetProcAddress")
)

// enableDisplayVSync asks WGL to synchronize buffer swaps with the display.
// gowin's Windows backend otherwise calls SwapBuffers as fast as the render
// loop permits, which can expose partially presented frames.
func enableDisplayVSync() {
	name, err := syscall.BytePtrFromString("wglSwapIntervalEXT")
	if err != nil {
		return
	}
	address, _, _ := wglGetProcAddressAPI.Call(uintptr(unsafe.Pointer(name)))
	switch address {
	case 0, 1, 2, 3, ^uintptr(0):
		return
	}
	_, _, _ = syscall.SyscallN(address, 1)
}
