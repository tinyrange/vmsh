//go:build darwin

package desktopapp

import "github.com/ebitengine/purego/objc"

func platformWindowControlsAvailable() bool { return false }

func activatePlatformWindowControl(chromeWindowControl) bool { return false }

func toggleActiveWindowMaximized() bool {
	application := objc.ID(objc.GetClass("NSApplication")).Send(objc.RegisterName("sharedApplication"))
	if application == 0 {
		return false
	}
	window := application.Send(objc.RegisterName("keyWindow"))
	if window == 0 {
		return false
	}
	window.Send(objc.RegisterName("performZoom:"), objc.ID(0))
	return true
}
