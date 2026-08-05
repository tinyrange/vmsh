//go:build !darwin && !windows

package desktopapp

func toggleActiveWindowMaximized() bool { return false }
