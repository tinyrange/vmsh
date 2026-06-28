package terminal

import (
	"io"
	"os"
)

type Mode int

const (
	ModeNonInteractive Mode = iota
	ModePlainInteractive
	ModeDynamicInteractive
)

type Capabilities struct {
	Mode          Mode
	Width         int
	Height        int
	Color         bool
	ReducedMotion bool
}

func Detect(in, out *os.File) Capabilities {
	c := Capabilities{Mode: ModeNonInteractive, Width: 80, Height: 24}
	if out != nil {
		if cols, rows, err := Size(out); err == nil {
			if cols > 0 {
				c.Width = cols
			}
			if rows > 0 {
				c.Height = rows
			}
		}
	}
	interactive := in != nil && out != nil && IsTerminal(in) && IsTerminal(out)
	term := os.Getenv("TERM")
	if interactive && term != "" && term != "dumb" {
		c.Mode = ModeDynamicInteractive
	} else if interactive {
		c.Mode = ModePlainInteractive
	}
	c.Color = c.Mode != ModeNonInteractive && os.Getenv("NO_COLOR") == "" && term != "dumb"
	c.ReducedMotion = os.Getenv("TERMUI_REDUCED_MOTION") != "" || os.Getenv("NO_MOTION") != ""
	return c
}

func IsTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	return IsTerminalFD(int(file.Fd()))
}

func DiscardRestore() func() {
	return func() {}
}

func WriteString(w io.Writer, s string) {
	if w != nil {
		_, _ = io.WriteString(w, s)
	}
}
