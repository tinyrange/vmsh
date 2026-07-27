package main

import (
	"runtime"
)

func init() {
	// Cocoa windows must be created on the process's original main thread.
	// Pin it before VM preparation has a chance to migrate this goroutine.
	runtime.LockOSThread()
}

func platformArguments(args []string) []string {
	if len(args) != 0 {
		return args
	}
	return []string{defaultNeurodesktopImage}
}
