package desktopapp

import (
	"runtime"

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

func platformExperimentalGPUAccelerationAvailable() bool {
	return runtime.GOARCH == "arm64"
}

func platformNativeGPUScanoutAvailable() bool {
	// NSOpenGL shared textures can expose partially composed X11 frames through
	// Apple's GL-on-Metal translation. Keep VirGL enabled in the guest, but use
	// the reliable readback/upload presentation path until that handoff is safe.
	return false
}
