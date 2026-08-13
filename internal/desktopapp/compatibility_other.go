//go:build !darwin

package desktopapp

func platformCompatibilityError() error {
	return nil
}

func platformExperimentalGPUAccelerationAvailable() bool {
	return false
}

func platformNativeGPUScanoutAvailable() bool {
	return false
}
