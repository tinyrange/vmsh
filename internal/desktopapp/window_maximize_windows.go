//go:build windows

package desktopapp

import "syscall"

const (
	windowsShowMaximized = 3
	windowsShowMinimized = 6
	windowsShowRestored  = 9
	windowsMessageClose  = 0x0010
)

var (
	windowsUser32              = syscall.NewLazyDLL("user32.dll")
	windowsGetForegroundWindow = windowsUser32.NewProc("GetForegroundWindow")
	windowsIsZoomed            = windowsUser32.NewProc("IsZoomed")
	windowsShowWindow          = windowsUser32.NewProc("ShowWindow")
	windowsPostMessage         = windowsUser32.NewProc("PostMessageW")
)

func platformWindowControlsAvailable() bool { return true }

func activatePlatformWindowControl(control chromeWindowControl) bool {
	window, _, _ := windowsGetForegroundWindow.Call()
	if window == 0 {
		return false
	}
	switch control {
	case chromeWindowControlMinimize:
		windowsShowWindow.Call(window, windowsShowMinimized)
	case chromeWindowControlMaximize:
		return toggleActiveWindowMaximized()
	case chromeWindowControlClose:
		windowsPostMessage.Call(window, windowsMessageClose, 0, 0)
	default:
		return false
	}
	return true
}

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
