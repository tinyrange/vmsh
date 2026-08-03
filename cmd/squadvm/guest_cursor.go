package main

import "j5.nz/cc/display"

type guestCursorHost interface {
	Apply(update display.CursorUpdate, desktopVisible bool) error
	Close()
}
