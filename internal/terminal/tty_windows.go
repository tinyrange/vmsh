//go:build windows

package terminal

import (
	"os"

	"golang.org/x/sys/windows"
)

func IsTerminalFD(fd int) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(fd), &mode) == nil
}

func Size(file *os.File) (int, int, error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(file.Fd()), &info); err != nil {
		return 0, 0, err
	}
	cols := int(info.Window.Right-info.Window.Left) + 1
	rows := int(info.Window.Bottom-info.Window.Top) + 1
	if cols <= 0 {
		cols = int(info.Size.X)
	}
	if rows <= 0 {
		rows = int(info.Size.Y)
	}
	return cols, rows, nil
}

func MakeRaw(file *os.File) (func(), error) {
	handle := windows.Handle(file.Fd())
	var original uint32
	if err := windows.GetConsoleMode(handle, &original); err != nil {
		return nil, err
	}
	raw := original
	raw &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
	raw |= windows.ENABLE_EXTENDED_FLAGS | windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(handle, raw); err != nil {
		return nil, err
	}
	return func() {
		_ = windows.SetConsoleMode(handle, original)
	}, nil
}

func InterruptRead(file *os.File) {
	if file == nil {
		return
	}
	_ = windows.CancelIoEx(windows.Handle(file.Fd()), nil)
}

func PrepareOutput(file *os.File) func() {
	handle := windows.Handle(file.Fd())
	var original uint32
	if err := windows.GetConsoleMode(handle, &original); err != nil {
		return func() {}
	}
	next := original | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	if err := windows.SetConsoleMode(handle, next); err != nil {
		return func() {}
	}
	return func() {
		_ = windows.SetConsoleMode(handle, original)
	}
}

func HostSignals(_ bool) []os.Signal {
	return []os.Signal{os.Interrupt}
}

func IsResizeSignal(os.Signal) bool {
	return false
}

func SignalName(sig os.Signal) (string, bool) {
	switch sig {
	case os.Interrupt:
		return "INT", true
	default:
		return "", false
	}
}
