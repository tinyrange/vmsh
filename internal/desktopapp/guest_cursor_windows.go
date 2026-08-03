//go:build windows

package desktopapp

import (
	"fmt"
	"syscall"
	"unsafe"

	"j5.nz/cc/display"
)

const (
	guestCursorClassIndex = -12 // GCLP_HCURSOR
	guestCursorArrow      = 32512
)

type guestCursorBitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type guestCursorBitmapInfo struct {
	Header guestCursorBitmapInfoHeader
	Color  uint32
}

type guestCursorIconInfo struct {
	Icon  int32
	HotX  uint32
	HotY  uint32
	Mask  syscall.Handle
	Color syscall.Handle
}

type windowsGuestCursorHost struct {
	hwnd       syscall.Handle
	cursor     syscall.Handle
	generation uint64
	visible    bool
	desktop    bool
}

var (
	guestCursorUser32 = syscall.NewLazyDLL("user32.dll")
	guestCursorGDI32  = syscall.NewLazyDLL("gdi32.dll")

	guestCursorGetActiveWindow    = guestCursorUser32.NewProc("GetActiveWindow")
	guestCursorLoadCursor         = guestCursorUser32.NewProc("LoadCursorW")
	guestCursorSetCursor          = guestCursorUser32.NewProc("SetCursor")
	guestCursorSetClassLongPtr    = guestCursorUser32.NewProc("SetClassLongPtrW")
	guestCursorCreateIconIndirect = guestCursorUser32.NewProc("CreateIconIndirect")
	guestCursorDestroyCursor      = guestCursorUser32.NewProc("DestroyCursor")
	guestCursorCreateDIBSection   = guestCursorGDI32.NewProc("CreateDIBSection")
	guestCursorCreateBitmap       = guestCursorGDI32.NewProc("CreateBitmap")
	guestCursorDeleteObject       = guestCursorGDI32.NewProc("DeleteObject")
)

func newGuestCursorHost() guestCursorHost { return &windowsGuestCursorHost{} }

func (h *windowsGuestCursorHost) Apply(update display.CursorUpdate, desktopVisible bool) error {
	if h.hwnd == 0 {
		active, _, _ := guestCursorGetActiveWindow.Call()
		h.hwnd = syscall.Handle(active)
	}
	if h.hwnd == 0 {
		return nil
	}
	if !desktopVisible {
		if h.desktop || h.cursor != 0 {
			h.restoreArrow()
		}
		return nil
	}
	enteringDesktop := !h.desktop
	h.desktop = true
	if !update.Visible {
		if enteringDesktop || h.visible || h.cursor != 0 {
			h.replace(0)
		}
		h.visible = false
		h.generation = update.Generation
		return nil
	}
	if h.visible && h.generation == update.Generation {
		return nil
	}
	cursor, err := createWindowsGuestCursor(update)
	if err != nil {
		return err
	}
	h.replace(cursor)
	h.visible = true
	h.generation = update.Generation
	return nil
}

func (h *windowsGuestCursorHost) Close() { h.restoreArrow() }

func (h *windowsGuestCursorHost) restoreArrow() {
	arrow, _, _ := guestCursorLoadCursor.Call(0, guestCursorArrow)
	h.replace(syscall.Handle(arrow))
	// LoadCursor returns a shared system cursor which must not be destroyed.
	h.cursor = 0
	h.desktop = false
	h.visible = false
	h.generation = 0
}

func (h *windowsGuestCursorHost) replace(cursor syscall.Handle) {
	old := h.cursor
	classIndex := int32(guestCursorClassIndex)
	guestCursorSetClassLongPtr.Call(uintptr(h.hwnd), uintptr(classIndex), uintptr(cursor))
	guestCursorSetCursor.Call(uintptr(cursor))
	h.cursor = cursor
	if old != 0 {
		guestCursorDestroyCursor.Call(uintptr(old))
	}
}

func createWindowsGuestCursor(update display.CursorUpdate) (syscall.Handle, error) {
	if update.Width <= 0 || update.Height <= 0 || update.Width > 256 || update.Height > 256 ||
		update.HotX < 0 || update.HotY < 0 || update.HotX >= update.Width || update.HotY >= update.Height {
		return 0, fmt.Errorf("invalid guest cursor geometry %dx%d hotspot %d,%d", update.Width, update.Height, update.HotX, update.HotY)
	}
	pixelBytes := update.Width * update.Height * 4
	if len(update.Pixels) < pixelBytes {
		return 0, fmt.Errorf("guest cursor needs %d pixel bytes, has %d", pixelBytes, len(update.Pixels))
	}
	bitmapInfo := guestCursorBitmapInfo{Header: guestCursorBitmapInfoHeader{
		Size: uint32(unsafe.Sizeof(guestCursorBitmapInfoHeader{})), Width: int32(update.Width),
		Height: -int32(update.Height), Planes: 1, BitCount: 32,
	}}
	var bits uintptr
	colorBitmap, _, _ := guestCursorCreateDIBSection.Call(
		0, uintptr(unsafe.Pointer(&bitmapInfo)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0,
	)
	if colorBitmap == 0 || bits == 0 {
		return 0, fmt.Errorf("create guest cursor color bitmap")
	}
	defer guestCursorDeleteObject.Call(colorBitmap)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(bits)), pixelBytes), update.Pixels[:pixelBytes])

	maskBitmap, _, _ := guestCursorCreateBitmap.Call(uintptr(update.Width), uintptr(update.Height), 1, 1, 0)
	if maskBitmap == 0 {
		return 0, fmt.Errorf("create guest cursor mask bitmap")
	}
	defer guestCursorDeleteObject.Call(maskBitmap)
	iconInfo := guestCursorIconInfo{
		HotX: uint32(update.HotX), HotY: uint32(update.HotY),
		Mask: syscall.Handle(maskBitmap), Color: syscall.Handle(colorBitmap),
	}
	cursor, _, _ := guestCursorCreateIconIndirect.Call(uintptr(unsafe.Pointer(&iconInfo)))
	if cursor == 0 {
		return 0, fmt.Errorf("create native guest cursor")
	}
	return syscall.Handle(cursor), nil
}
