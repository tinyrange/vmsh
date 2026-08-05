//go:build windows

package desktopapp

import "syscall"

const (
	windowsShowMaximized = 3
	windowsShowRestored  = 9
)

var (
	windowsUser32              = syscall.NewLazyDLL("user32.dll")
	windowsGetForegroundWindow = windowsUser32.NewProc("GetForegroundWindow")
	windowsIsZoomed            = windowsUser32.NewProc("IsZoomed")
	windowsShowWindow          = windowsUser32.NewProc("ShowWindow")
)

func toggleActiveWindowMaximized() bool {
	window, _, _ := windowsGetForegroundWindow.Call()
	if window == 0 {
		return false
	}
	command := uintptr(windowsShowMaximized)
	if maximized, _, _ := windowsIsZoomed.Call(window); maximized != 0 {
		command = windowsShowRestored
	}
	windowsShowWindow.Call(window, command)
	return true
}
