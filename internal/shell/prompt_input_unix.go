//go:build !windows

package shell

import (
	"os"

	"github.com/tinyrange/vmsh/internal/terminal"
)

func openPromptInput(fallback *os.File) (*os.File, func()) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err == nil && terminal.IsTerminalFD(int(tty.Fd())) {
		return tty, func() { _ = tty.Close() }
	}
	if tty != nil {
		_ = tty.Close()
	}
	return fallback, func() {}
}
