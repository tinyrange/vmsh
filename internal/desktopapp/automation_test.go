package desktopapp

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"j5.nz/cc/display"
)

const automationTestToken = "0123456789abcdef0123456789abcdef"

type automationTestSession struct {
	mu            sync.Mutex
	width         int
	height        int
	generation    uint64
	pixels        []byte
	changed       chan struct{}
	firstSnapshot chan struct{}
	snapshotOnce  sync.Once
	keys          []automationTestKey
	pointers      []automationTestPointer
	scrolls       []image.Point
}

type automationTestKey struct {
	code uint16
	down bool
}

type automationTestPointer struct {
	x, y              uint32
	buttons, previous uint8
}

func newAutomationTestSession() *automationTestSession {
	return &automationTestSession{
		width: 2, height: 1, generation: 1,
		pixels:        []byte{0, 0, 0, 0, 0, 0, 0, 0},
		changed:       make(chan struct{}, 8),
		firstSnapshot: make(chan struct{}),
	}
}

func (s *automationTestSession) Size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.width, s.height
}

func (s *automationTestSession) Snapshot(image.Rectangle, uint64, bool) display.FramebufferUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotOnce.Do(func() { close(s.firstSnapshot) })
	return display.FramebufferUpdate{
		Width: s.width, Height: s.height, Generation: s.generation,
		Rect: image.Rect(0, 0, s.width, s.height), Pixels: append([]byte(nil), s.pixels...),
	}
}

func (s *automationTestSession) Changed() <-chan struct{} { return s.changed }
func (*automationTestSession) Resize(int, int) error      { return nil }
func (s *automationTestSession) Key(code uint16, down bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, automationTestKey{code: code, down: down})
	return nil
}
func (s *automationTestSession) Pointer(x, y uint32, buttons, previous uint8) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pointers = append(s.pointers, automationTestPointer{x: x, y: y, buttons: buttons, previous: previous})
	return nil
}
func (s *automationTestSession) Scroll(x, y int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scrolls = append(s.scrolls, image.Pt(int(x), int(y)))
	return nil
}
func (*automationTestSession) SetClipboard(string)              {}
func (*automationTestSession) GuestClipboard() (string, uint64) { return "", 0 }

func (s *automationTestSession) publish(pixels []byte) {
	s.mu.Lock()
	s.generation++
	s.pixels = append(s.pixels[:0], pixels...)
	s.mu.Unlock()
	s.changed <- struct{}{}
}

func TestAutomationConfigurationIsDisabledUnlessExplicitAndLoopback(t *testing.T) {
	config, err := loadDesktopAutomationConfig("", "", "")
	if err != nil || config != nil {
		t.Fatalf("default automation config = %+v, %v", config, err)
	}
	if _, err := loadDesktopAutomationConfig("127.0.0.1:0", "", ""); err == nil {
		t.Fatal("incomplete automation configuration was accepted")
	}

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(automationTestToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDesktopAutomationConfig("0.0.0.0:8080", tokenFile, filepath.Join(dir, "captures")); err == nil {
		t.Fatal("non-loopback automation listener was accepted")
	}
	config, err = loadDesktopAutomationConfig("127.0.0.1:0", tokenFile, filepath.Join(dir, "captures"))
	if err != nil || config == nil {
		t.Fatalf("loopback automation config = %+v, %v", config, err)
	}
}

func TestAuthenticatedAutomationRoutesGuestInput(t *testing.T) {
	automation := &desktopAutomation{token: []byte(automationTestToken), captureRoot: t.TempDir()}
	session := newAutomationTestSession()
	automation.setSession(session)
	handler := automation.handler()

	unauthorized := automationHTTP(t, handler, http.MethodGet, "/v1/status", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	for _, body := range []string{
		`{"x":1,"y":0,"buttons":1}`,
		`{"x":1,"y":0,"buttons":0}`,
	} {
		response := automationHTTP(t, handler, http.MethodPost, "/v1/input/pointer", []byte(body), automationTestToken)
		if response.Code != http.StatusOK {
			t.Fatalf("pointer response = %d: %s", response.Code, response.Body.String())
		}
	}
	if response := automationHTTP(t, handler, http.MethodPost, "/v1/input/key", []byte(`{"code":30,"down":true}`), automationTestToken); response.Code != http.StatusOK {
		t.Fatalf("key response = %d: %s", response.Code, response.Body.String())
	}
	if response := automationHTTP(t, handler, http.MethodPost, "/v1/input/scroll", []byte(`{"delta_x_120":0,"delta_y_120":120}`), automationTestToken); response.Code != http.StatusOK {
		t.Fatalf("scroll response = %d: %s", response.Code, response.Body.String())
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	wantPointers := []automationTestPointer{
		{x: 1, y: 0, buttons: 1, previous: 0},
		{x: 1, y: 0, buttons: 0, previous: 1},
	}
	if len(session.pointers) != len(wantPointers) || session.pointers[0] != wantPointers[0] || session.pointers[1] != wantPointers[1] {
		t.Fatalf("pointer transitions = %+v, want %+v", session.pointers, wantPointers)
	}
	if len(session.keys) != 1 || session.keys[0] != (automationTestKey{code: 30, down: true}) {
		t.Fatalf("key transitions = %+v", session.keys)
	}
	if len(session.scrolls) != 1 || session.scrolls[0] != image.Pt(0, 120) {
		t.Fatalf("scroll transitions = %+v", session.scrolls)
	}
}

func TestAutomationCapturesSubsequentDesktopFramesAsPNG(t *testing.T) {
	root := t.TempDir()
	automation := &desktopAutomation{token: []byte(automationTestToken), captureRoot: root}
	session := newAutomationTestSession()
	automation.setSession(session)
	handler := automation.handler()

	invalid := automationHTTP(t, handler, http.MethodPost, "/v1/captures", []byte(`{"frames":1,"subdir":"../escape"}`), automationTestToken)
	if invalid.Code < 400 || invalid.Code >= 500 {
		t.Fatalf("traversal capture response = %d: %s", invalid.Code, invalid.Body.String())
	}
	response := automationHTTP(t, handler, http.MethodPost, "/v1/captures", []byte(`{"frames":2,"subdir":"firefox-av1"}`), automationTestToken)
	if response.Code != http.StatusAccepted {
		t.Fatalf("start capture response = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-session.firstSnapshot:
	case <-time.After(time.Second):
		t.Fatal("capture did not establish its initial framebuffer generation")
	}

	// The framebuffer is little-endian XRGB: blue, green, red, unused.
	session.publish([]byte{3, 2, 1, 0, 30, 20, 10, 0})
	waitForAutomationCaptureCount(t, automation, 1)
	session.publish([]byte{6, 5, 4, 0, 60, 50, 40, 0})
	waitForAutomationCaptureCount(t, automation, 2)

	files, err := filepath.Glob(filepath.Join(root, "firefox-av1", "*.png"))
	if err != nil || len(files) != 2 {
		t.Fatalf("captured files = %v, %v", files, err)
	}
	file, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, a := decoded.At(0, 0).RGBA()
	if r>>8 != 1 || g>>8 != 2 || b>>8 != 3 || a>>8 != 255 {
		t.Fatalf("captured first pixel = rgba(%d,%d,%d,%d)", r>>8, g>>8, b>>8, a>>8)
	}
}

func TestAutomationCapturesComposedPresentationFrames(t *testing.T) {
	root := t.TempDir()
	automation := &desktopAutomation{token: []byte(automationTestToken), captureRoot: root}
	automation.setSession(newAutomationTestSession())
	status, err := automation.startCapture(1, "presentation", automationSourcePresentation)
	if err != nil {
		t.Fatal(err)
	}
	job, ok := automation.beginPresentationFrame()
	if !ok {
		t.Fatal("presentation capture did not request a frame")
	}
	// OpenGL returns the lower row first. The encoded PNG should be top-down.
	automation.submitPresentationFrame(job, 2, 2, 42, []byte{
		0, 0, 255, 0, 0, 0, 255, 0,
		255, 0, 0, 0, 255, 0, 0, 0,
	})
	waitForAutomationCaptureCount(t, automation, 1)
	files, err := filepath.Glob(filepath.Join(status.Directory, "*.png"))
	if err != nil || len(files) != 1 {
		t.Fatalf("presentation files = %v, %v", files, err)
	}
	file, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	topR, _, topB, topA := decoded.At(0, 0).RGBA()
	bottomR, _, bottomB, bottomA := decoded.At(0, 1).RGBA()
	if topR>>8 != 255 || topB != 0 || topA>>8 != 255 || bottomR != 0 || bottomB>>8 != 255 || bottomA>>8 != 255 {
		t.Fatalf("presentation rows were not converted from OpenGL coordinates")
	}
}

func automationHTTP(t *testing.T, handler http.Handler, method, path string, body []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:54321"
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func waitForAutomationCaptureCount(t *testing.T, automation *desktopAutomation, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		automation.mu.Lock()
		status := automation.capture.status
		automation.mu.Unlock()
		if status.Captured == count {
			return
		}
		if status.State == "failed" {
			encoded, _ := json.Marshal(status)
			t.Fatalf("capture failed: %s", encoded)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("capture did not reach %d frames", count)
}
