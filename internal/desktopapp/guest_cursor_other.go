//go:build !windows

package desktopapp

import "j5.nz/cc/display"

type noopGuestCursorHost struct{}

func newGuestCursorHost() guestCursorHost { return noopGuestCursorHost{} }

func (noopGuestCursorHost) Apply(display.CursorUpdate, bool) error { return nil }
func (noopGuestCursorHost) Close()                                 {}
