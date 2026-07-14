//go:build windows

package shell

import (
	"os"

	"github.com/tinyrange/vmsh/internal/terminal"
)

func openPromptInput(fallback *os.File) (*os.File, func()) {
	conin, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err == nil && terminal.IsTerminalFD(int(conin.Fd())) {
		return conin, func() { _ = conin.Close() }
	}
	if conin != nil {
		_ = conin.Close()
	}
	return fallback, func() {}
}
