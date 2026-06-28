//go:build windows

package terminal

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	enableEchoInput             = 0x0004
	enableLineInput             = 0x0002
	enableProcessedInput        = 0x0001
	enableExtendedFlags         = 0x0080
	enableVirtualTerminalInput  = 0x0200
	enableProcessedOutput       = 0x0001
	enableVirtualTerminalOutput = 0x0004
)

type coord struct {
	X int16
	Y int16
}

type smallRect struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procCancelIoEx                 = kernel32.NewProc("CancelIoEx")
)

func getConsoleMode(handle syscall.Handle, mode *uint32) error {
	r1, _, e1 := procGetConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(mode)))
	if r1 == 0 {
		return e1
	}
	return nil
}

func setConsoleMode(handle syscall.Handle, mode uint32) error {
	r1, _, e1 := procSetConsoleMode.Call(uintptr(handle), uintptr(mode))
	if r1 == 0 {
		return e1
	}
	return nil
}

func IsTerminalFD(fd int) bool {
	var mode uint32
	return getConsoleMode(syscall.Handle(fd), &mode) == nil
}

func Size(file *os.File) (int, int, error) {
	var info consoleScreenBufferInfo
	r1, _, e1 := procGetConsoleScreenBufferInfo.Call(uintptr(syscall.Handle(file.Fd())), uintptr(unsafe.Pointer(&info)))
	if r1 == 0 {
		return 0, 0, e1
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
	handle := syscall.Handle(file.Fd())
	var original uint32
	if err := getConsoleMode(handle, &original); err != nil {
		return nil, err
	}
	raw := original
	raw &^= enableEchoInput | enableLineInput | enableProcessedInput
	raw |= enableExtendedFlags | enableVirtualTerminalInput
	if err := setConsoleMode(handle, raw); err != nil {
		return nil, err
	}
	return func() {
		_ = setConsoleMode(handle, original)
	}, nil
}

func PrepareOutput(file *os.File) func() {
	handle := syscall.Handle(file.Fd())
	var original uint32
	if err := getConsoleMode(handle, &original); err != nil {
		return DiscardRestore()
	}
	next := original | enableProcessedOutput | enableVirtualTerminalOutput
	if err := setConsoleMode(handle, next); err != nil {
		return DiscardRestore()
	}
	return func() {
		_ = setConsoleMode(handle, original)
	}
}

func InterruptRead(file *os.File) {
	if file == nil {
		return
	}
	procCancelIoEx.Call(uintptr(syscall.Handle(file.Fd())), 0)
}
