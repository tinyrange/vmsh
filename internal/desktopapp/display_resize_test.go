package desktopapp

import (
	"image"
	"testing"
	"time"

	"github.com/tinyrange/gowin/gl"
	"github.com/tinyrange/gowin/window"
	"j5.nz/cc/display"
)

type resizeTestWindow struct {
	width, height int
	scale         float32
}

func (*resizeTestWindow) GL() (gl.OpenGL, error)                 { return nil, nil }
func (*resizeTestWindow) Close()                                 {}
func (*resizeTestWindow) Poll() bool                             { return true }
func (*resizeTestWindow) Swap()                                  {}
func (w *resizeTestWindow) BackingSize() (int, int)              { return w.width, w.height }
func (*resizeTestWindow) Cursor() (float32, float32)             { return 0, 0 }
func (w *resizeTestWindow) Scale() float32                       { return w.scale }
func (*resizeTestWindow) GetKeyState(window.Key) window.KeyState { return window.KeyStateUp }
func (*resizeTestWindow) GetButtonState(window.Button) window.ButtonState {
	return window.ButtonStateUp
}
func (*resizeTestWindow) DrainInputEvents() []window.InputEvent { return nil }
func (*resizeTestWindow) TextInput() string                     { return "" }

type resizeTestSession struct {
	width, height int
	resizes       []image.Point
	changed       chan struct{}
}

func (s *resizeTestSession) Size() (int, int) { return s.width, s.height }
func (*resizeTestSession) Snapshot(image.Rectangle, uint64, bool) display.FramebufferUpdate {
	return display.FramebufferUpdate{}
}
func (s *resizeTestSession) Changed() <-chan struct{} { return s.changed }
func (s *resizeTestSession) Resize(width, height int) error {
	s.width, s.height = width, height
	s.resizes = append(s.resizes, image.Pt(width, height))
	return nil
}
func (*resizeTestSession) Key(uint16, bool) error                     { return nil }
func (*resizeTestSession) Pointer(uint32, uint32, uint8, uint8) error { return nil }
func (*resizeTestSession) SetClipboard(string)                        {}
func (*resizeTestSession) GuestClipboard() (string, uint64)           { return "", 0 }

func TestNativeDisplayDoesNotResizeGuestForInitialWindowAdjustment(t *testing.T) {
	win := &resizeTestWindow{width: 2880, height: 1782, scale: 2}
	session := &resizeTestSession{width: 1440, height: 900, changed: make(chan struct{})}
	viewer := &displayViewer{window: win, session: session}

	if err := viewer.handleResize(); err != nil {
		t.Fatal(err)
	}
	if len(session.resizes) != 0 {
		t.Fatalf("initial platform-adjusted window resized guest to %v", session.resizes)
	}

	win.width, win.height = 2800, 1700
	if err := viewer.handleResize(); err != nil {
		t.Fatal(err)
	}
	viewer.resizeChangedAt = time.Now().Add(-time.Second)
	if err := viewer.handleResize(); err != nil {
		t.Fatal(err)
	}
	if len(session.resizes) != 1 || session.resizes[0] != image.Pt(1400, 850) {
		t.Fatalf("user resize requests = %v, want [(1400,850)]", session.resizes)
	}
}
