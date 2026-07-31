package main

import (
	"github.com/tinyrange/vmsh/internal/hostcompat"
	"golang.org/x/sys/unix"
)

func platformCompatibilityError() error {
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return nil
	}
	return hostcompat.DesktopMacOSRequirement(version)
}
