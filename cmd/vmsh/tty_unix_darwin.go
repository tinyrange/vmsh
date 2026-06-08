//go:build darwin

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func isTerminalFD(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	return err == nil
}

func terminalSize(file *os.File) (int, int, error) {
	ws, err := unix.IoctlGetWinsize(int(file.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

func makeRawTerminal(file *os.File) (func(), error) {
	fd := int(file.Fd())
	termios, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, err
	}
	originalTermios := *termios
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, err
	}
	raw := *termios
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &raw); err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.IoctlSetTermios(fd, unix.TIOCSETA, &originalTermios)
		return nil, err
	}
	return func() {
		_ = unix.SetNonblock(fd, flags&unix.O_NONBLOCK != 0)
		_ = unix.IoctlSetTermios(fd, unix.TIOCSETA, &originalTermios)
	}, nil
}

func interruptTerminalRead(*os.File) {}

func prepareTerminalOutput(*os.File) func() {
	return func() {}
}

func hostSignals(tty bool) []os.Signal {
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP}
	if tty {
		signals = append(signals, syscall.SIGWINCH)
	}
	return signals
}

func isResizeSignal(sig os.Signal) bool {
	return sig == syscall.SIGWINCH
}

func signalName(sig os.Signal) (string, bool) {
	switch sig {
	case os.Interrupt:
		return "INT", true
	case syscall.SIGHUP:
		return "HUP", true
	case syscall.SIGQUIT:
		return "QUIT", true
	case syscall.SIGTERM:
		return "TERM", true
	default:
		return "", false
	}
}
