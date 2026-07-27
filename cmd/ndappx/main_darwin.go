package main

import (
	"os"
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
	if os.Getenv("TERM") != "" {
		return []string{"--help"}
	}
	return []string{defaultNeurodesktopImage}
}
