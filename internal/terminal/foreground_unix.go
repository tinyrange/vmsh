//go:build darwin || linux

package terminal

import (
	"os"
	"os/signal"
	"sync"

	"golang.org/x/sys/unix"
)

var ignoreTerminalOutputStop sync.Once

type ForegroundProcessGroup struct {
	fd       int
	original int
}

func NewForegroundProcessGroup(file *os.File) (*ForegroundProcessGroup, error) {
	if file == nil {
		return nil, os.ErrInvalid
	}
	fd := int(file.Fd())
	if !IsTerminalFD(fd) {
		return nil, os.ErrInvalid
	}
	ignoreTerminalOutputStop.Do(func() {
		signal.Ignore(unix.SIGTTOU)
	})
	original, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return nil, err
	}
	return &ForegroundProcessGroup{fd: fd, original: original}, nil
}

func (g *ForegroundProcessGroup) Set(pgid int) error {
	if g == nil || pgid <= 0 {
		return nil
	}
	return unix.IoctlSetPointerInt(g.fd, unix.TIOCSPGRP, pgid)
}

func (g *ForegroundProcessGroup) Restore() error {
	if g == nil || g.original <= 0 {
		return nil
	}
	return unix.IoctlSetPointerInt(g.fd, unix.TIOCSPGRP, g.original)
}
