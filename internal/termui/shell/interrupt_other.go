//go:build !linux && !darwin

package shell

func externalInterrupted(error) bool {
	return false
}
