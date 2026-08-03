//go:build !darwin

package desktopapp

func platformCompatibilityError() error {
	return nil
}
