//go:build !linux && !darwin && !windows

package terminal

import (
	"errors"
	"os"
)

func IsTerminalFD(int) bool { return false }

func Size(*os.File) (int, int, error) {
	return 0, 0, errors.New("terminal size unsupported")
}

func MakeRaw(*os.File) (func(), error) {
	return nil, errors.New("raw terminal unsupported")
}

func PrepareOutput(*os.File) func() { return DiscardRestore() }

func InterruptRead(*os.File) {}
