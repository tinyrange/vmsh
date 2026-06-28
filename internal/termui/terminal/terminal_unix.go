//go:build linux || darwin

package terminal

import (
	"os"
	"syscall"
	"unsafe"
)

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func ioctl(fd int, req uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func fcntl(fd int, cmd int, arg int) (int, error) {
	r1, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(cmd), uintptr(arg))
	if errno != 0 {
		return 0, errno
	}
	return int(r1), nil
}

func IsTerminalFD(fd int) bool {
	var t syscall.Termios
	return ioctl(fd, ioctlGetTermios, uintptr(unsafe.Pointer(&t))) == nil
}

func Size(file *os.File) (int, int, error) {
	var ws winsize
	if err := ioctl(int(file.Fd()), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

func MakeRaw(file *os.File) (func(), error) {
	fd := int(file.Fd())
	var original syscall.Termios
	if err := ioctl(fd, ioctlGetTermios, uintptr(unsafe.Pointer(&original))); err != nil {
		return nil, err
	}
	flags, err := fcntl(fd, syscall.F_GETFL, 0)
	if err != nil {
		return nil, err
	}
	raw := original
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Cflag |= syscall.CS8
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, ioctlSetTermios, uintptr(unsafe.Pointer(&raw))); err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = ioctl(fd, ioctlSetTermios, uintptr(unsafe.Pointer(&original)))
		return nil, err
	}
	return func() {
		_ = syscall.SetNonblock(fd, flags&syscall.O_NONBLOCK != 0)
		_ = ioctl(fd, ioctlSetTermios, uintptr(unsafe.Pointer(&original)))
	}, nil
}

func PrepareOutput(*os.File) func() {
	return DiscardRestore()
}

func InterruptRead(*os.File) {}
