//go:build !darwin && !windows

package desktopapp

func toggleActiveWindowMaximized() bool { return false }

func platformWindowControlsAvailable() bool { return false }

func activatePlatformWindowControl(chromeWindowControl) bool { return false }
