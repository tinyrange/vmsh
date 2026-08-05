package desktopapp

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	imagedraw "image/draw"
	"image/png"
	"math"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tinyrange/gowin/gl"
	gowintext "github.com/tinyrange/gowin/text"
	"github.com/tinyrange/gowin/window"
	"github.com/tinyrange/vmsh/internal/ptyterm"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"
	"j5.nz/cc/client"
	"j5.nz/cc/display"
)

const (
	glBGRA          = 0x80e1
	appChromeHeight = float32(28)

	displayVertexShader = `#version 150
in vec2 position;
in vec2 texcoord;
out vec2 uv;
void main() {
	uv = texcoord;
	gl_Position = vec4(position, 0.0, 1.0);
}`
	displayFragmentShader = `#version 150
uniform sampler2D framebuffer;
in vec2 uv;
out vec4 color;
void main() {
	color = texture(framebuffer, uv);
}`
	roundedRectFragmentShader = `#version 150
uniform vec4 shapeColor;
uniform vec4 shapeGeometry;
in vec2 uv;
out vec4 color;
void main() {
	vec2 size = shapeGeometry.xy;
	float radius = min(shapeGeometry.z, min(size.x, size.y) * 0.5);
	vec2 point = uv * size - size * 0.5;
	vec2 distanceToCorner = abs(point) - (size * 0.5 - vec2(radius));
	float distanceToEdge = length(max(distanceToCorner, 0.0))
		+ min(max(distanceToCorner.x, distanceToCorner.y), 0.0) - radius;
	float coverage = smoothstep(0.0, shapeGeometry.w, -distanceToEdge);
	color = vec4(shapeColor.rgb, shapeColor.a * coverage);
}`
	backgroundFragmentShaderTemplate = `#version 150
uniform vec4 backgroundGeometry;
in vec2 uv;
out vec4 color;
void main() {
	vec2 size = backgroundGeometry.xy;
	vec2 point = uv * size;
	vec2 glowCenter = vec2(size.x * 0.92, -size.y * 0.08);
	float glowRadius = min(size.x, size.y) * 0.56;
	float glow = (1.0 - smoothstep(0.0, glowRadius, length(point - glowCenter))) * 0.22;
	vec3 base = vec3(%d.0, %d.0, %d.0) / 255.0;
	vec3 accent = vec3(%d.0, %d.0, %d.0) / 255.0;
	color = vec4(mix(base, accent, glow), 1.0);
}`
	checkmarkFragmentShader = `#version 150
uniform vec4 iconColor;
in vec2 uv;
out vec4 color;
float segmentDistance(vec2 point, vec2 start, vec2 end) {
	vec2 relative = point - start;
	vec2 segment = end - start;
	float position = clamp(dot(relative, segment) / dot(segment, segment), 0.0, 1.0);
	return length(relative - segment * position);
}
void main() {
	float distanceToMark = min(
		segmentDistance(uv, vec2(0.22, 0.52), vec2(0.43, 0.72)),
		segmentDistance(uv, vec2(0.43, 0.72), vec2(0.79, 0.29))
	);
	float coverage = 1.0 - smoothstep(0.055, 0.085, distanceToMark);
	color = vec4(iconColor.rgb, iconColor.a * coverage);
}`
)

type displayViewer struct {
	session             display.Session
	window              window.Window
	guestCursor         guestCursorHost
	gl                  gl.OpenGL
	text                *gowintext.Stash
	font                int
	fontBold            int
	fontMono            int
	fontMonoBold        int
	program             uint32
	roundedRectProgram  uint32
	roundedRectColor    int32
	roundedRectGeometry int32
	backgroundProgram   uint32
	backgroundGeometry  int32
	checkmarkProgram    uint32
	checkmarkColor      int32
	vertexArray         uint32
	vertexBuffer        uint32
	texture             uint32
	brandTexture        uint32
	brandWidth          int
	brandHeight         int
	textureWidth        int
	textureHeight       int
	generation          uint64
	buttons             uint8
	sentButtons         uint8
	scrollX120Remainder float64
	scrollY120Remainder float64
	legacyScrollY120    int64
	keysDown            map[window.Key]bool
	guestClipboardGen   uint64
	hostClipboard       string
	lastResize          image.Point
	pendingResize       image.Point
	resizeChangedAt     time.Time
	windowMinimized     bool
	startup             startupProgress
	startupChecklist    []startupChecklistItem
	checklistChangedAt  time.Time
	startupTerminal     *ptyterm.Emulator
	serialMu            sync.Mutex
	serialPending       []byte
	serialLastCR        bool
	startupDetailScroll int
	startupEvents       chan startupProgress
	startDone           chan displayStartResult
	cancelDone          chan struct{}
	preflight           startupPreflight
	preflightDone       chan displayPreflightResult
	preflightReady      bool
	preflightErr        error
	settings            startupOptions
	folderSelectionErr  string
	showSettings        bool
	showAdvanced        bool
	resourceDrag        startupControl
	updateShownAt       time.Time
	releaseDismissed    bool
	imageDismissed      bool
	releaseUpdateErr    error
	imageRestartReady   chan struct{}
	starting            bool
	returnToSettings    bool
	startCancel         context.CancelFunc
	attemptStopped      <-chan struct{}
	parentContext       context.Context
	start               displayStart
	presentation        desktopPresentationGate
	desktopVisible      bool
	startErr            error
	startupFocus        startupControl
	startupFocusVisible bool
	startupHover        startupControl
	updateFocus         int
	updateFocusActive   bool
	updateHover         int
	updateHoverActive   bool
	updateConsumedKeys  map[window.Key]bool
	chromeEnabled       bool
	chromeInsets        window.TitleBarInsets
	cvmfsStatus         client.CVMFSStatusResponse
	cvmfsActivity       cvmfsActivityPresentation
	cvmfsStatusEvents   chan client.CVMFSStatusResponse
	cvmfsExpanded       bool
	cvmfsMirrorMenuOpen bool
	cvmfsMirrorOffset   int
	lastChromeClickAt   time.Time
	lastChromeClick     image.Point
	chromeControlHover  chromeWindowControl
}

type chromeWindowControl uint8

const (
	chromeWindowControlNone chromeWindowControl = iota
	chromeWindowControlMinimize
	chromeWindowControlMaximize
	chromeWindowControlClose
)

type chromeWindowControlButton struct {
	control chromeWindowControl
	bounds  image.Rectangle
}

type displayStartResult struct {
	started displayStarted
	err     error
}

type displayPreflightResult struct {
	preflight startupPreflight
	err       error
}

type cvmfsStatusSource func(context.Context) (client.CVMFSStatusResponse, error)

const cvmfsActivityHold = 750 * time.Millisecond

// cvmfsActivityPresentation smooths the short gaps between demand-driven
// object fetches. It does not alter the daemon's active-transfer state; it only
// keeps the most recently observed activity visible long enough for a person to
// read it.
type cvmfsActivityPresentation struct {
	lastActive client.CVMFSStatusResponse
	activeAt   time.Time
}

func (p *cvmfsActivityPresentation) observe(status client.CVMFSStatusResponse, now time.Time) client.CVMFSStatusResponse {
	if status.State == "error" {
		p.lastActive = client.CVMFSStatusResponse{}
		p.activeAt = time.Time{}
		return status
	}
	if len(status.ActiveTransfers) != 0 {
		p.lastActive = status
		p.activeAt = now
		return status
	}
	if !p.activeAt.IsZero() && now.Sub(p.activeAt) < cvmfsActivityHold {
		presented := p.lastActive
		if status.SelectedMirror != "" {
			presented.SelectedMirror = status.SelectedMirror
		}
		return presented
	}
	p.lastActive = client.CVMFSStatusResponse{}
	p.activeAt = time.Time{}
	return status
}

func openDisplayWindow(
	ctx context.Context,
	title string,
	width, height int,
	settings startupOptions,
	preflight displayPreflight,
	start displayStart,
	cvmfsStatus cvmfsStatusSource,
) error {
	win, err := window.New(title, width, height, true)
	if err != nil {
		return fmt.Errorf("open graphics window: %w", err)
	}
	viewer := &displayViewer{
		window:             win,
		guestCursor:        newGuestCursorHost(),
		keysDown:           make(map[window.Key]bool),
		startup:            initialStartupProgress(),
		startupEvents:      make(chan startupProgress, 16),
		startDone:          make(chan displayStartResult, 1),
		cancelDone:         make(chan struct{}, 1),
		preflightDone:      make(chan displayPreflightResult, 1),
		imageRestartReady:  make(chan struct{}, 1),
		settings:           settings,
		showSettings:       true,
		updateConsumedKeys: make(map[window.Key]bool),
		parentContext:      ctx,
		start:              start,
		chromeEnabled:      cvmfsStatus != nil,
		cvmfsStatusEvents:  make(chan client.CVMFSStatusResponse, 1),
	}
	if viewer.chromeEnabled {
		if chrome, ok := win.(window.IntegratedTitleBarSupport); ok && chrome.SetIntegratedTitleBar(true) {
			viewer.chromeInsets = chrome.IntegratedTitleBarInsets()
		}
		go pollCVMFSStatus(ctx, cvmfsStatus, viewer.cvmfsStatusEvents)
	}
	viewer.setStartupProgress(initialStartupProgress())
	if err := viewer.init(); err != nil {
		win.Close()
		return err
	}
	defer viewer.close()
	viewer.beginPreflight(ctx, preflight)
	return viewer.loop(ctx)
}

func pollCVMFSStatus(ctx context.Context, source cvmfsStatusSource, updates chan client.CVMFSStatusResponse) {
	// CVMFS object fetches are commonly shorter than 200 ms. Poll often enough
	// to observe them, then let cvmfsActivityPresentation bridge the tiny gaps
	// between demand-driven reads.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := source(ctx)
		if err == nil {
			select {
			case updates <- status:
			default:
				select {
				case <-updates:
				default:
				}
				select {
				case updates <- status:
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (v *displayViewer) beginPreflight(ctx context.Context, preflight displayPreflight) {
	go func() {
		result, err := preflight(ctx)
		v.preflightDone <- displayPreflightResult{preflight: result, err: err}
	}()
}

func (v *displayViewer) beginStart(refreshImage bool) {
	if v.starting || !v.preflightReady || !v.preflight.canPrepareImage() {
		return
	}
	v.settings.RefreshImage = refreshImage
	backingWidth, backingHeight := v.window.BackingSize()
	displaySize := v.guestDisplaySize(backingWidth, backingHeight)
	v.settings.DisplayWidth = displaySize.X
	v.settings.DisplayHeight = displaySize.Y
	ctx, cancel := context.WithCancel(v.parentContext)
	v.startCancel = cancel
	v.starting = true
	v.returnToSettings = false
	v.showSettings = false
	v.startErr = nil
	v.startupChecklist = nil
	v.startupTerminal = ptyterm.NewEmulator(ptyterm.Size{Cols: 120, Rows: 12}, 256)
	v.serialMu.Lock()
	v.serialPending = nil
	v.serialMu.Unlock()
	v.serialLastCR = false
	v.startupDetailScroll = 0
	v.setStartupProgress(initialStartupProgress())
	publish := func(progress startupProgress) {
		if progress.Serial != "" {
			v.queueStartupSerial(progress.Serial)
			return
		}
		select {
		case v.startupEvents <- progress:
		default:
			select {
			case <-v.startupEvents:
			default:
			}
			select {
			case v.startupEvents <- progress:
			default:
			}
		}
	}
	go func() {
		started, err := v.start(ctx, v.settings, publish)
		v.startDone <- displayStartResult{started: started, err: err}
	}()
}

func (v *displayViewer) queueStartupSerial(data string) {
	if data == "" {
		return
	}
	v.serialMu.Lock()
	v.serialPending = append(v.serialPending, data...)
	v.serialMu.Unlock()
}

func (v *displayViewer) drainStartupSerial() {
	v.serialMu.Lock()
	pending := v.serialPending
	v.serialPending = nil
	v.serialMu.Unlock()
	if len(pending) != 0 {
		v.appendStartupSerial(string(pending))
	}
}

func (v *displayViewer) setStartupProgress(progress startupProgress) {
	if progress.Serial != "" {
		v.appendStartupSerial(progress.Serial)
		return
	}
	v.startup = progress
	v.startupDetailScroll = 0
	var appended bool
	v.startupChecklist, appended = updateStartupChecklist(v.startupChecklist, progress)
	if appended {
		v.checklistChangedAt = time.Now()
	}
}

func (v *displayViewer) appendStartupSerial(data string) {
	if data == "" {
		return
	}
	if v.startupTerminal == nil {
		v.startupTerminal = ptyterm.NewEmulator(ptyterm.Size{Cols: 120, Rows: 12}, 256)
	}
	raw := []byte(data)
	normalized := make([]byte, 0, len(raw)+8)
	for _, b := range raw {
		if b == '\n' && !v.serialLastCR {
			normalized = append(normalized, '\r')
		}
		normalized = append(normalized, b)
		v.serialLastCR = b == '\r'
	}
	_, _ = v.startupTerminal.Write(normalized)
}

func (v *displayViewer) init() error {
	var err error
	v.gl, err = v.window.GL()
	if err != nil {
		return fmt.Errorf("initialize graphics: %w", err)
	}
	// The low-level display path does not pass through gowin/graphics, which
	// normally establishes alpha blending for Fontstash glyph textures.
	v.gl.Enable(gl.Blend)
	v.gl.BlendFunc(gl.SrcAlpha, gl.OneMinusSrcAlpha)
	v.program, err = compileDisplayProgram(v.gl)
	if err != nil {
		return err
	}
	v.roundedRectProgram, err = compileGraphicsProgram(v.gl, roundedRectFragmentShader)
	if err != nil {
		return fmt.Errorf("compile rounded rectangle renderer: %w", err)
	}
	v.roundedRectColor = v.gl.GetUniformLocation(v.roundedRectProgram, "shapeColor")
	v.roundedRectGeometry = v.gl.GetUniformLocation(v.roundedRectProgram, "shapeGeometry")
	v.backgroundProgram, err = compileGraphicsProgram(v.gl, backgroundFragmentShader())
	if err != nil {
		return fmt.Errorf("compile interface background renderer: %w", err)
	}
	v.backgroundGeometry = v.gl.GetUniformLocation(v.backgroundProgram, "backgroundGeometry")
	v.checkmarkProgram, err = compileGraphicsProgram(v.gl, checkmarkFragmentShader)
	if err != nil {
		return fmt.Errorf("compile checkmark renderer: %w", err)
	}
	v.checkmarkColor = v.gl.GetUniformLocation(v.checkmarkProgram, "iconColor")
	vertices := []float32{
		-1, 1, 0, 0,
		-1, -1, 0, 1,
		1, -1, 1, 1,
		-1, 1, 0, 0,
		1, -1, 1, 1,
		1, 1, 1, 0,
	}
	v.gl.GenVertexArrays(1, &v.vertexArray)
	v.gl.GenBuffers(1, &v.vertexBuffer)
	v.gl.BindVertexArray(v.vertexArray)
	v.gl.BindBuffer(gl.ArrayBuffer, v.vertexBuffer)
	v.gl.BufferData(gl.ArrayBuffer, len(vertices)*4, unsafe.Pointer(&vertices[0]), gl.StaticDraw)
	position := v.gl.GetAttribLocation(v.program, "position")
	texcoord := v.gl.GetAttribLocation(v.program, "texcoord")
	if position < 0 || texcoord < 0 {
		return fmt.Errorf("graphics shader attributes are unavailable")
	}
	for name, program := range map[string]uint32{
		"rounded rectangle": v.roundedRectProgram,
		"background":        v.backgroundProgram,
		"checkmark":         v.checkmarkProgram,
	} {
		if candidate := v.gl.GetAttribLocation(program, "position"); candidate != position {
			return fmt.Errorf("%s position attribute is incompatible", name)
		}
		if candidate := v.gl.GetAttribLocation(program, "texcoord"); candidate != texcoord {
			return fmt.Errorf("%s texture coordinate attribute is incompatible", name)
		}
	}
	v.gl.VertexAttribPointer(uint32(position), 2, gl.Float, false, 16, 0)
	v.gl.EnableVertexAttribArray(uint32(position))
	v.gl.VertexAttribPointer(uint32(texcoord), 2, gl.Float, false, 16, 8)
	v.gl.EnableVertexAttribArray(uint32(texcoord))
	v.gl.GenTextures(1, &v.texture)
	v.gl.ActiveTexture(gl.Texture0)
	v.gl.BindTexture(gl.Texture2D, v.texture)
	v.gl.TexParameteri(gl.Texture2D, gl.TextureMinFilter, gl.Nearest)
	v.gl.TexParameteri(gl.Texture2D, gl.TextureMagFilter, gl.Nearest)
	v.gl.TexParameteri(gl.Texture2D, gl.TextureWrapS, gl.ClampToEdge)
	v.gl.TexParameteri(gl.Texture2D, gl.TextureWrapT, gl.ClampToEdge)
	v.gl.PixelStorei(gl.UnpackAlignment, 1)
	v.gl.UseProgram(v.program)
	v.gl.Uniform1i(v.gl.GetUniformLocation(v.program, "framebuffer"), 0)
	if err := v.loadBrandTexture(); err != nil {
		return err
	}
	v.text = gowintext.New(v.gl, 1024, 1024)
	v.text.SetYInverted(true)
	v.text.SetGraphicsShader(v.program)
	v.font, err = v.text.AddFontFromMemory(goregular.TTF)
	if err != nil {
		return fmt.Errorf("load interface font: %w", err)
	}
	v.fontBold, err = v.text.AddFontFromMemory(gobold.TTF)
	if err != nil {
		return fmt.Errorf("load bold interface font: %w", err)
	}
	v.fontMono, err = v.text.AddFontFromMemory(gomono.TTF)
	if err != nil {
		return fmt.Errorf("load console font: %w", err)
	}
	v.fontMonoBold, err = v.text.AddFontFromMemory(gomonobold.TTF)
	if err != nil {
		return fmt.Errorf("load bold console font: %w", err)
	}
	return nil
}

func (v *displayViewer) close() {
	if v.guestCursor != nil {
		v.guestCursor.Close()
	}
	if v.brandTexture != 0 {
		v.gl.DeleteTextures(1, &v.brandTexture)
	}
	if v.texture != 0 {
		v.gl.DeleteTextures(1, &v.texture)
	}
	if v.vertexBuffer != 0 {
		v.gl.DeleteBuffers(1, &v.vertexBuffer)
	}
	if v.vertexArray != 0 {
		v.gl.DeleteVertexArrays(1, &v.vertexArray)
	}
	if v.program != 0 {
		v.gl.DeleteProgram(v.program)
	}
	if v.roundedRectProgram != 0 {
		v.gl.DeleteProgram(v.roundedRectProgram)
	}
	if v.backgroundProgram != 0 {
		v.gl.DeleteProgram(v.backgroundProgram)
	}
	if v.checkmarkProgram != 0 {
		v.gl.DeleteProgram(v.checkmarkProgram)
	}
	v.window.Close()
}

func (v *displayViewer) loop(ctx context.Context) error {
	clipboard := window.GetClipboard()
	v.hostClipboard = clipboard.GetText()
	nextClipboardCheck := time.Now()
	for v.window.Poll() {
		v.drainStartupSerial()
		select {
		case <-ctx.Done():
			if v.startErr != nil {
				return v.startErr
			}
			return nil
		default:
		}

		for {
			select {
			case progress := <-v.startupEvents:
				v.setStartupProgress(progress)
				if progress.Failed {
					v.desktopVisible = false
					v.startErr = fmt.Errorf("%s", progress.Detail)
				}
			case result := <-v.startDone:
				v.starting = false
				if v.returnToSettings {
					v.returnToSettings = false
					v.showSettings = true
					v.startCancel = nil
					v.startErr = nil
					continue
				}
				if result.err != nil {
					v.startCancel = nil
					if ctx.Err() != nil {
						return nil
					}
					v.startErr = result.err
					v.setStartupProgress(failedStartupProgress(result.err))
				} else if result.started.Session == nil {
					v.startCancel = nil
					v.setStartupProgress(failedStartupProgress(fmt.Errorf("VM started without a native display session")))
				} else {
					v.session = result.started.Session
					v.lastResize = image.Pt(v.settings.DisplayWidth, v.settings.DisplayHeight)
					v.attemptStopped = result.started.Stopped
					v.session.SetClipboard(v.hostClipboard)
					v.presentation.markGuestReady()
					v.setStartupProgress(desktopStartupProgress("Waiting for a complete desktop frame"))
				}
			case <-v.cancelDone:
				v.starting = false
				v.startCancel = nil
				v.attemptStopped = nil
				if v.returnToSettings {
					v.returnToSettings = false
					v.showSettings = true
				}
			case result := <-v.preflightDone:
				v.preflightErr = result.err
				if result.err != nil {
					v.showSettings = true
					continue
				}
				v.preflight = result.preflight
				v.settings.CVMFSAutoMirror = result.preflight.CVMFSMirror
				v.preflightReady = true
				v.showSettings = true
			case status := <-v.cvmfsStatusEvents:
				v.cvmfsStatus = v.cvmfsActivity.observe(status, time.Now())
			case <-v.imageRestartReady:
				v.starting = false
				v.startCancel = nil
				v.attemptStopped = nil
				v.beginStart(true)
			default:
				goto eventsDrained
			}
		}
	eventsDrained:

		if v.session == nil || !v.desktopVisible {
			// Do not deliver keys or pointer events to a guest surface the user
			// cannot see. Polling still drains host events while the progress
			// view owns the window.
			v.handleStartupInput(v.window.DrainInputEvents())
		} else if err := v.handleInput(); err != nil {
			return err
		}
		if v.session != nil {
			if err := v.handleResize(); err != nil {
				return err
			}
		}
		if v.desktopVisible && time.Now().After(nextClipboardCheck) {
			if err := v.syncClipboard(clipboard); err != nil {
				return err
			}
			nextClipboardCheck = time.Now().Add(200 * time.Millisecond)
		}
		if v.session != nil {
			update, err := v.updateTexture()
			if err != nil {
				return err
			}
			v.presentation.observe(update, time.Now())
			if v.presentation.ready(time.Now()) {
				v.desktopVisible = true
			}
		}
		if capture, ok := v.window.(window.SystemKeyCaptureSupport); ok {
			capture.SetSystemKeyCaptured(v.desktopVisible)
		}
		if err := v.updateGuestCursor(); err != nil {
			return err
		}
		backingWidth, backingHeight := v.window.BackingSize()
		v.gl.Viewport(0, 0, int32(backingWidth), int32(backingHeight))
		if v.desktopVisible && v.textureWidth > 0 && v.textureHeight > 0 {
			// Display snapshots are XRGB/BGRX. Their fourth byte is padding,
			// not opacity, so blending it would turn valid X11 surfaces
			// transparent wherever that byte happens to be zero.
			v.gl.Disable(gl.Blend)
			v.gl.ClearColor(0, 0, 0, 1)
			v.gl.Clear(gl.ColorBufferBit)
			if v.chromeEnabled {
				scale := normalizedDisplayScale(v.window.Scale())
				logicalWidth := float32(backingWidth) / scale
				logicalHeight := float32(backingHeight) / scale
				v.drawTexture(v.texture, backingWidth, backingHeight, scale, 0, appChromeHeight, logicalWidth, logicalHeight-appChromeHeight)
			} else {
				v.gl.UseProgram(v.program)
				v.gl.ActiveTexture(gl.Texture0)
				v.gl.BindTexture(gl.Texture2D, v.texture)
				v.gl.BindVertexArray(v.vertexArray)
				v.gl.DrawArrays(gl.Triangles, 0, 6)
			}
			// The framebuffer's fourth byte is padding, but the UI overlays use
			// real alpha (notably the font atlas). Restore blending as soon as the
			// opaque framebuffer has been drawn.
			v.gl.Enable(gl.Blend)
			v.drawUpdateNotifications(backingWidth, backingHeight, time.Now())
		} else if v.showSettings {
			v.drawSettings(backingWidth, backingHeight)
		} else {
			v.drawStartup(backingWidth, backingHeight, time.Now())
		}
		if v.chromeEnabled {
			v.drawAppChrome(backingWidth, backingHeight)
		}
		v.window.Swap()
		time.Sleep(time.Second / 120)
	}
	return v.startErr
}

func (v *displayViewer) handleStartupInput(events []window.InputEvent) {
	scale := normalizedDisplayScale(v.window.Scale())
	for _, event := range events {
		if v.handleChromeInput(event) {
			continue
		}
		if (!v.showSettings && v.startup.Failed) || (v.showSettings && v.settingsFailureDetail() != "") {
			switch event.Type {
			case window.InputEventScroll:
				if event.ScrollY > 0 {
					v.startupDetailScroll = max(0, v.startupDetailScroll-2)
				} else if event.ScrollY < 0 {
					v.startupDetailScroll += 2
				}
			case window.InputEventKeyDown:
				switch event.Key {
				case window.KeyUp:
					v.startupDetailScroll = max(0, v.startupDetailScroll-1)
				case window.KeyDown:
					v.startupDetailScroll++
				case window.KeyPageUp:
					v.startupDetailScroll = max(0, v.startupDetailScroll-8)
				case window.KeyPageDown:
					v.startupDetailScroll += 8
				case window.KeyHome:
					v.startupDetailScroll = 0
				case window.KeyEnd:
					v.startupDetailScroll = int(^uint(0) >> 1)
				}
			}
		}
		if event.Type == window.InputEventKeyDown && event.Key == window.KeyEscape && !event.Repeat {
			if v.cvmfsMirrorMenuOpen {
				v.cvmfsMirrorMenuOpen = false
				continue
			}
			if v.showSettings && v.showAdvanced {
				v.showAdvanced = false
				v.startupFocus = startupControlAdvanced
				v.startupFocusVisible = true
				continue
			}
			if !v.showSettings {
				v.startErr = nil
				if v.startCancel != nil {
					v.showSettings = false
					if v.starting {
						v.returnToSettings = true
					} else if v.attemptStopped != nil {
						stopped := v.attemptStopped
						v.starting = true
						go func() {
							<-stopped
							v.cancelDone <- struct{}{}
						}()
					}
					v.returnToSettings = true
					v.startCancel()
					v.setStartupProgress(startupProgress{
						Phase:  startupDesktop,
						Title:  "Stopping " + productName(),
						Detail: "Waiting for the virtual machine to stop safely",
					})
				} else {
					v.showSettings = true
				}
				v.session = nil
				v.presentation = desktopPresentationGate{}
				v.desktopVisible = false
			}
			continue
		}
		if !v.showSettings {
			continue
		}
		backingWidth, backingHeight := v.window.BackingSize()
		logicalHeight := float32(backingHeight) / scale
		layout := v.settingsLayout(float32(backingWidth)/scale, logicalHeight)
		point := image.Pt(int(event.MouseX/scale), int(event.MouseY/scale))
		switch event.Type {
		case window.InputEventKeyDown:
			if event.Repeat && event.Key != window.KeyLeft && event.Key != window.KeyRight {
				continue
			}
			switch event.Key {
			case window.KeyTab:
				v.cvmfsMirrorMenuOpen = false
				v.startupFocus = nextStartupControlForOptions(
					v.startupFocus,
					event.Mods&window.ModShift != 0,
					v.preflight.hasUpdate(),
					v.showAdvanced,
					v.hasCVMFSMirrors(),
				)
				v.startupFocusVisible = true
			case window.KeyLeft:
				v.adjustResource(v.startupFocus, -1)
			case window.KeyRight:
				v.adjustResource(v.startupFocus, 1)
			case window.KeySpace:
				if v.startupFocusVisible {
					v.activateStartupControl(v.startupFocus)
				} else {
					v.settings.SSHEnabled = !v.settings.SSHEnabled
				}
			case window.KeyEnter:
				if v.startupFocusVisible {
					v.activateStartupControl(v.startupFocus)
				} else {
					v.beginSettingsStart(v.preflight.hasUpdate())
				}
			}
		case window.InputEventScroll:
			if v.cvmfsMirrorMenuOpen {
				v.scrollCVMFSMirrorMenu(event.ScrollY)
			}
		case window.InputEventMouseMove, window.InputEventMouseDown:
			if event.Type == window.InputEventMouseDown && v.cvmfsMirrorMenuOpen {
				if choice, ok := v.cvmfsMirrorChoiceAt(point, layout, logicalHeight); ok {
					v.settings.CVMFSMirror = choice
					v.cvmfsMirrorMenuOpen = false
					v.startupFocus = startupControlCVMFSMirror
					continue
				}
				if !point.In(layout.cvmfsMirror) {
					v.cvmfsMirrorMenuOpen = false
				}
			}
			control := advancedControlAt(point, layout)
			if control == startupControlNone {
				control = startupControlAt(point, layout, v.preflight.hasUpdate())
			}
			v.startupHover = control
			if event.Type == window.InputEventMouseMove && v.resourceDrag != startupControlNone && v.window.GetButtonState(window.ButtonLeft).IsDown() {
				v.setResourceFromPointer(v.resourceDrag, point.X, layout)
			}
			if event.Type != window.InputEventMouseDown {
				continue
			}
			v.startupFocusVisible = false
			switch control {
			case startupControlSSH:
				v.startupFocus = startupControlSSH
				v.settings.SSHEnabled = !v.settings.SSHEnabled
			case startupControlSystem:
				v.startupFocus = startupControlSystem
				v.settings.SystemInstall = !v.settings.SystemInstall
			case startupControlAdvanced:
				v.startupFocus = startupControlAdvanced
				v.showAdvanced = !v.showAdvanced
			case startupControlMemory, startupControlCPUs:
				v.startupFocus = control
				v.resourceDrag = control
				v.setResourceFromPointer(control, point.X, layout)
			case startupControlCVMFSMirror:
				v.startupFocus = control
				v.cvmfsMirrorMenuOpen = !v.cvmfsMirrorMenuOpen
				v.ensureCVMFSMirrorVisible()
			case startupControlSharedFolder:
				v.startupFocus = startupControlSharedFolder
				v.chooseSharedFolder()
			case startupControlSkip:
				v.startupFocus = startupControlSkip
				v.beginSettingsStart(false)
			case startupControlPrimary:
				v.startupFocus = startupControlPrimary
				v.beginSettingsStart(v.preflight.hasUpdate())
			}
		case window.InputEventMouseUp:
			v.resourceDrag = startupControlNone
		}
	}
}

func (v *displayViewer) activateStartupControl(control startupControl) {
	switch control {
	case startupControlSSH:
		v.settings.SSHEnabled = !v.settings.SSHEnabled
	case startupControlSystem:
		v.settings.SystemInstall = !v.settings.SystemInstall
	case startupControlAdvanced:
		v.showAdvanced = !v.showAdvanced
		v.cvmfsMirrorMenuOpen = false
	case startupControlCVMFSMirror:
		v.cvmfsMirrorMenuOpen = !v.cvmfsMirrorMenuOpen
		v.ensureCVMFSMirrorVisible()
	case startupControlSharedFolder:
		v.chooseSharedFolder()
	case startupControlSkip:
		if v.preflight.hasUpdate() {
			v.beginSettingsStart(false)
		}
	case startupControlPrimary:
		v.beginSettingsStart(v.preflight.hasUpdate())
	}
}

func (v *displayViewer) adjustResource(control startupControl, delta int) {
	switch control {
	case startupControlMemory:
		step := memorySliderStep(v.settings.MemoryMB, v.settings.MaxMemoryMB)
		v.settings.MemoryMB = memoryForSliderStep(step+delta, v.settings.MaxMemoryMB)
	case startupControlCPUs:
		v.settings.CPUs = min(v.settings.MaxCPUs, max(1, v.settings.CPUs+delta))
	case startupControlCVMFSMirror:
		v.cycleCVMFSMirror(delta)
	}
}

func (v *displayViewer) setResourceFromPointer(control startupControl, x int, layout startupControlLayout) {
	var bounds image.Rectangle
	switch control {
	case startupControlMemory:
		bounds = layout.memorySlider
	case startupControlCPUs:
		bounds = layout.cpuSlider
	default:
		return
	}
	position := float32(x-bounds.Min.X) / float32(max(1, bounds.Dx()))
	if control == startupControlMemory {
		step := sliderValue(position, 0, memorySliderSteps(v.settings.MaxMemoryMB))
		v.settings.MemoryMB = memoryForSliderStep(step, v.settings.MaxMemoryMB)
	} else {
		v.settings.CPUs = sliderValue(position, 1, v.settings.MaxCPUs)
	}
}

func (v *displayViewer) beginSettingsStart(refreshImage bool) {
	share, err := createStorageShare(v.settings.SharedFolder)
	if err != nil {
		v.folderSelectionErr = err.Error()
		return
	}
	v.settings.SharedFolder = share.Source
	v.folderSelectionErr = ""
	if v.preflight.hasUpdate() {
		v.imageDismissed = true
	}
	v.beginStart(refreshImage)
}

func (v *displayViewer) chooseSharedFolder() {
	dialog, ok := v.window.(window.FileDialogSupport)
	if !ok {
		v.folderSelectionErr = "Folder selection is unavailable on this desktop."
		return
	}
	selected := strings.TrimSpace(dialog.ShowOpenPanel(window.FileDialogTypeDirectory, nil))
	if selected == "" {
		return
	}
	share, err := createStorageShare(selected)
	if err != nil {
		v.folderSelectionErr = err.Error()
		return
	}
	v.settings.SharedFolder = share.Source
	v.folderSelectionErr = ""
}

func (v *displayViewer) updateTexture() (display.FramebufferUpdate, error) {
	width, height := v.session.Size()
	update := v.session.Snapshot(image.Rect(0, 0, width, height), v.generation, v.generation != 0)
	if update.Generation == v.generation && update.Rect.Empty() {
		return update, nil
	}
	v.gl.ActiveTexture(gl.Texture0)
	v.gl.BindTexture(gl.Texture2D, v.texture)
	if update.Width != v.textureWidth || update.Height != v.textureHeight {
		v.textureWidth, v.textureHeight = update.Width, update.Height
		v.gl.TexImage2D(gl.Texture2D, 0, int32(gl.RGBA), int32(update.Width), int32(update.Height), 0, gl.RGBA, gl.UnsignedByte, nil)
		update = v.session.Snapshot(image.Rect(0, 0, update.Width, update.Height), 0, false)
	}
	if !update.Rect.Empty() && len(update.Pixels) != 0 {
		v.gl.TexSubImage2D(gl.Texture2D, 0, int32(update.Rect.Min.X), int32(update.Rect.Min.Y),
			int32(update.Rect.Dx()), int32(update.Rect.Dy()), glBGRA, gl.UnsignedByte, unsafe.Pointer(&update.Pixels[0]))
	}
	v.generation = update.Generation
	return update, nil
}

func (v *displayViewer) drawSettings(backingWidth, backingHeight int) {
	v.gl.Enable(gl.Blend)
	scale := normalizedDisplayScale(v.window.Scale())
	width := float32(backingWidth) / scale
	height := float32(backingHeight) / scale
	v.gl.UseProgram(v.program)
	v.drawBackground(backingWidth, backingHeight)

	layout := v.settingsLayout(width, height)
	panel := layout.panel
	left := float32(panel.Min.X)
	contentTop := float32(panel.Min.Y)
	panelWidth := float32(panel.Dx())
	brand := layout.brand
	v.drawTexture(
		v.brandTexture, backingWidth, backingHeight, scale,
		float32(brand.Min.X), float32(brand.Min.Y), float32(brand.Dx()), float32(brand.Dy()),
	)

	v.text.SetViewport(int32(width), int32(height))
	v.text.SetScale(scale)
	title := "Checking your system"
	detail := "Confirming the host and desktop image are ready."
	state := "CHECKING"
	stateColor := uiTextSecondary
	stateBackground := uiSurfaceRaised
	titleColor := uiText
	if v.preflightErr != nil {
		title = "Checks failed"
		detail = "See the complete error below."
		state = "ACTION NEEDED"
		stateColor = uiError
		stateBackground = uiErrorSurface
		titleColor = uiErrorStrong
	} else if v.preflightReady {
		title = "Ready to start"
		detail = "Your system and desktop image are ready."
		state = "READY"
		stateColor = uiSuccess
		stateBackground = uiSuccessSurface
		if !v.preflight.Image.Installed {
			title = "Set up " + productName()
			detail = "Download the desktop image, then start automatically."
			state = "DOWNLOAD NEEDED"
			stateColor = uiWarning
			stateBackground = uiWarningSurface
		} else if v.preflight.hasUpdate() && v.preflight.ReleaseUpdate != nil {
			title = "Updates available"
			detail = "Update the desktop image and virtual machine manager."
			state = "UPDATES"
			stateColor = uiAccentSoft
			stateBackground = uiSurfaceHover
		} else if v.preflight.hasUpdate() || v.preflight.ReleaseUpdate != nil {
			title = "Update available"
			detail = "A newer " + productName() + " component is ready to install."
			state = "UPDATE"
			stateColor = uiAccentSoft
			stateBackground = uiSurfaceHover
		}
		if !v.preflight.canPrepareImage() {
			title = "Can't start yet"
			detail = "Review the failed host requirement before preparing " + productName() + "."
			state = "ACTION NEEDED"
			stateColor = uiWarning
			stateBackground = uiWarningSurface
			titleColor = uiWarning
		} else if !preflightCanStartWithMirror(v.preflight, v.settings.CVMFSMirror) {
			title = "Ready to prepare"
			detail = "The desktop image will be prepared before CVMFS connects and the virtual machine starts."
			state = "SETUP READY"
			stateColor = uiWarning
			stateBackground = uiWarningSurface
		}
	}

	stateBounds := layout.state
	v.drawRoundedRect(backingWidth, backingHeight, scale, stateBounds, stateBounds.Dy()/2, stateBackground)
	v.text.BeginDraw()
	v.drawTextBold(productName(), left+60, contentTop+21, 19, uiText)
	v.drawText(productSubtitle(), left+60, contentTop+43, 14, uiAccent)
	v.drawCenteredTextBold(state, stateBounds, 13, stateColor)
	v.drawTextBold(fitStartupText(title, panelWidth, 34), left, contentTop+99, 34, titleColor)
	for index, line := range wrapStartupText(detail, panelWidth, 18, 2) {
		v.drawText(line, left, contentTop+129+float32(index*21), 18, uiTextSecondary)
	}
	v.text.EndDraw()
	if failure := v.settingsFailureDetail(); failure != "" {
		v.drawSettingsFailure(backingWidth, backingHeight, scale, left, contentTop+158, panelWidth, failure)
		return
	}
	if !layout.status[0].Empty() {
		v.drawPreflightCard(backingWidth, backingHeight, scale, layout.status[0],
			preflightStatus(v.preflightReady, v.preflight.VirtualizationOK),
			"Virtualization",
			preflightVirtualizationDetail(v.preflightReady, v.preflight),
		)
		v.drawPreflightCard(backingWidth, backingHeight, scale, layout.status[1],
			preflightStatus(v.preflightReady, v.preflight.DiskOK),
			"Disk space",
			preflightDiskDetail(v.preflightReady, v.preflight),
		)
		imageOK := v.preflightReady
		v.drawPreflightCard(backingWidth, backingHeight, scale, layout.status[2],
			imagePreflightStatus(v.preflightReady, imageOK, v.preflight),
			"Desktop image",
			preflightImageDetail(v.preflightReady, v.preflight, v.settings.DownloadRate),
		)
		v.drawPreflightCard(backingWidth, backingHeight, scale, layout.status[3],
			releasePreflightStatus(v.preflightReady, v.preflight),
			"Virtual machine manager",
			preflightReleaseDetail(v.preflightReady, v.preflight),
		)
		if !layout.cvmfsStatus.Empty() {
			v.drawPreflightCard(backingWidth, backingHeight, scale, layout.cvmfsStatus,
				cvmfsPreflightStatus(v.preflightReady, v.preflight),
				"CVMFS mirror",
				cvmfsPreflightDetail(v.preflightReady, v.preflight),
			)
		}
	}

	v.drawSettingsOption(
		backingWidth, backingHeight, scale, layout.sshCheckbox,
		"SSH access · ssh "+appConfig.SSHHost, "Modifies ~/.ssh/config",
		v.settings.SSHEnabled,
		v.startupFocusVisible && v.startupFocus == startupControlSSH,
		v.startupHover == startupControlSSH,
	)
	v.drawSettingsOption(
		backingWidth, backingHeight, scale, layout.systemCheckbox,
		"System install", "Store data in your user cache",
		v.settings.SystemInstall,
		v.startupFocusVisible && v.startupFocus == startupControlSystem,
		v.startupHover == startupControlSystem,
	)
	v.drawAdvancedOption(backingWidth, backingHeight, scale, layout.advanced)
	if v.showAdvanced {
		v.drawAdvancedSettings(backingWidth, backingHeight, scale, layout)
	}
	v.drawSharedFolderOption(backingWidth, backingHeight, scale, layout)

	button := layout.button
	buttonColor := uiDisabled
	buttonText := uiDisabled
	if v.preflightReady && v.preflight.canPrepareImage() && !v.starting {
		buttonColor = uiPrimary
		buttonText = uiWhite
		if v.startupHover == startupControlPrimary {
			buttonColor = uiPrimaryHover
		}
	}
	divider := layout.actionDivider
	v.drawRect(backingWidth, backingHeight, scale,
		float32(divider.Min.X), float32(divider.Min.Y), float32(divider.Dx()), float32(divider.Dy()),
		uiSurfaceRaised)
	v.drawRoundedRect(backingWidth, backingHeight, scale, button, 9, buttonColor)
	label := "Start " + productName()
	if v.preflightReady && !v.preflight.Image.Installed {
		label = "Download & start"
	}
	if v.preflightReady && v.preflight.hasUpdate() {
		label = "Update & start"
		skip := layout.skip
		skipColor := uiSurface
		skipText := uiText
		if v.starting {
			skipText = uiDisabled
		} else if v.startupHover == startupControlSkip {
			skipColor = uiSurfaceHover
		}
		v.drawPanel(backingWidth, backingHeight, scale, skip, 9,
			uiBorderStrong, skipColor)
		if v.startupFocusVisible && v.startupFocus == startupControlSkip {
			v.drawOutline(backingWidth, backingHeight, scale, skip, uiAccent)
		}
		v.text.BeginDraw()
		v.drawCenteredTextBold("Skip", skip, 15, skipText)
		v.text.EndDraw()
	}
	if v.starting {
		label = "Starting…"
	}
	if v.startupFocusVisible && v.startupFocus == startupControlPrimary {
		v.drawOutline(backingWidth, backingHeight, scale, button, uiAccent)
	}
	v.text.BeginDraw()
	v.drawCenteredTextBold(label, button, 15, buttonText)
	shortcutWidth := float32(layout.skip.Min.X) - left - 12
	if !v.preflight.hasUpdate() {
		shortcutWidth = float32(button.Min.X) - left - 12
	}
	v.drawText(fitStartupText("Space  select   ·   Tab  move   ·   Enter  start", shortcutWidth, 15),
		left, float32(button.Min.Y+33), 15, uiAccentSoft)
	v.text.EndDraw()
	if v.cvmfsMirrorMenuOpen {
		v.drawCVMFSMirrorMenu(backingWidth, backingHeight, scale, layout, height)
	}
}

func (v *displayViewer) drawAdvancedOption(backingWidth, backingHeight int, scale float32, bounds image.Rectangle) {
	borderColor := uiBorder
	fillColor := uiSurface
	if v.startupHover == startupControlAdvanced {
		borderColor = uiBorderStrong
		fillColor = uiSurfaceHover
	}
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10, borderColor, fillColor)
	if v.startupFocusVisible && v.startupFocus == startupControlAdvanced {
		v.drawOutline(backingWidth, backingHeight, scale, bounds, uiAccent)
	}
	v.text.BeginDraw()
	v.drawTextBold("Advanced", float32(bounds.Min.X+14), float32(bounds.Min.Y+24), 16, uiText)
	summary := fmt.Sprintf("%s · %d vCPU", formatMemoryAmount(v.settings.MemoryMB), v.settings.CPUs)
	if v.hasCVMFSMirrors() {
		mode := "CVMFS auto"
		if strings.TrimSpace(v.settings.CVMFSMirror) != "" {
			mode = "CVMFS override"
		}
		summary += " · " + mode
	}
	v.drawText(fitStartupText(summary, float32(bounds.Dx()-48), 14),
		float32(bounds.Min.X+14), float32(bounds.Min.Y+47), 14, uiAccentSoft)
	indicator := "+"
	if v.showAdvanced {
		indicator = "-"
	}
	v.drawTextBold(indicator, float32(bounds.Max.X-24), float32(bounds.Min.Y+32), 18, uiAccent)
	v.text.EndDraw()
}

func (v *displayViewer) drawAdvancedSettings(backingWidth, backingHeight int, scale float32, layout startupControlLayout) {
	bounds := layout.advancedPanel
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10, uiBorder, uiSurface)
	memoryValue := image.Rect(layout.memorySlider.Max.X-72, layout.memorySlider.Min.Y-34, layout.memorySlider.Max.X, layout.memorySlider.Min.Y-6)
	cpuValue := image.Rect(layout.cpuSlider.Max.X-52, layout.cpuSlider.Min.Y-34, layout.cpuSlider.Max.X, layout.cpuSlider.Min.Y-6)
	v.drawPanel(backingWidth, backingHeight, scale, memoryValue, 7, uiBorderStrong, uiSurfaceRaised)
	v.drawPanel(backingWidth, backingHeight, scale, cpuValue, 7, uiBorderStrong, uiSurfaceRaised)
	v.text.BeginDraw()
	v.drawText("Host maximums can leave the host unresponsive.", float32(layout.memorySlider.Min.X), float32(bounds.Min.Y+27), 14, uiTextSecondary)
	v.drawTextBold("Memory", float32(layout.memorySlider.Min.X), float32(layout.memorySlider.Min.Y-13), 15, uiText)
	v.drawCenteredTextBold(formatMemoryAmount(v.settings.MemoryMB), memoryValue, 14, uiAccent)
	v.drawText("1 GB", float32(layout.memorySlider.Min.X), float32(layout.memorySlider.Max.Y+18), 13, uiTextMuted)
	v.drawText(formatMemoryAmount(v.settings.MaxMemoryMB), float32(layout.memorySlider.Max.X-42), float32(layout.memorySlider.Max.Y+18), 13, uiTextMuted)
	v.drawTextBold("Virtual CPUs", float32(layout.cpuSlider.Min.X), float32(layout.cpuSlider.Min.Y-13), 15, uiText)
	v.drawCenteredTextBold(fmt.Sprintf("%d", v.settings.CPUs), cpuValue, 14, uiAccent)
	v.drawText("1 vCPU", float32(layout.cpuSlider.Min.X), float32(layout.cpuSlider.Max.Y+18), 13, uiTextMuted)
	v.drawText(fmt.Sprintf("%d vCPU", v.settings.MaxCPUs), float32(layout.cpuSlider.Max.X-50), float32(layout.cpuSlider.Max.Y+18), 13, uiTextMuted)
	v.text.EndDraw()
	v.drawResourceSlider(backingWidth, backingHeight, scale, layout.memorySlider,
		sliderPosition(memorySliderStep(v.settings.MemoryMB, v.settings.MaxMemoryMB), 0, memorySliderSteps(v.settings.MaxMemoryMB)),
		v.startupFocusVisible && v.startupFocus == startupControlMemory)
	v.drawResourceSlider(backingWidth, backingHeight, scale, layout.cpuSlider,
		sliderPosition(v.settings.CPUs, 1, v.settings.MaxCPUs),
		v.startupFocusVisible && v.startupFocus == startupControlCPUs)
	if !layout.cvmfsMirror.Empty() {
		fill := uiSurfaceRaised
		border := uiBorderStrong
		if v.startupHover == startupControlCVMFSMirror {
			fill = uiSurfaceHover
		}
		v.drawPanel(backingWidth, backingHeight, scale, layout.cvmfsMirror, 8, border, fill)
		if v.startupFocusVisible && v.startupFocus == startupControlCVMFSMirror {
			v.drawOutline(backingWidth, backingHeight, scale, layout.cvmfsMirror, uiAccent)
		}
		v.text.BeginDraw()
		v.drawTextBold("CVMFS mirror", float32(layout.cvmfsMirror.Min.X+12), float32(layout.cvmfsMirror.Min.Y+18), 14, uiText)
		label := fitStartupText(v.cvmfsMirrorSelectionLabel(), float32(layout.cvmfsMirror.Dx()-48), 13)
		v.drawText(label, float32(layout.cvmfsMirror.Min.X+12), float32(layout.cvmfsMirror.Min.Y+39), 13, uiAccentSoft)
		v.drawTextBold("v", float32(layout.cvmfsMirror.Max.X-25), float32(layout.cvmfsMirror.Min.Y+29), 17, uiAccent)
		v.text.EndDraw()
	}
}

const cvmfsMirrorMenuVisibleRows = 7

func (v *displayViewer) hasCVMFSMirrors() bool { return len(v.settings.CVMFSMirrors) != 0 }

func (v *displayViewer) chromeContentTop() float32 {
	if v.chromeEnabled {
		return appChromeHeight
	}
	return 0
}

func (v *displayViewer) settingsLayout(width, height float32) startupControlLayout {
	top := v.chromeContentTop()
	layout := settingsControlLayoutForOptions(width, max(float32(1), height-top), v.showAdvanced, v.hasCVMFSMirrors())
	if top == 0 {
		return layout
	}
	offset := image.Pt(0, int(top))
	layout.panel = layout.panel.Add(offset)
	layout.brand = layout.brand.Add(offset)
	layout.state = layout.state.Add(offset)
	for index := range layout.status {
		layout.status[index] = layout.status[index].Add(offset)
	}
	layout.cvmfsStatus = layout.cvmfsStatus.Add(offset)
	layout.sshCheckbox = layout.sshCheckbox.Add(offset)
	layout.systemCheckbox = layout.systemCheckbox.Add(offset)
	layout.advanced = layout.advanced.Add(offset)
	layout.sharedFolder = layout.sharedFolder.Add(offset)
	layout.sharedBrowse = layout.sharedBrowse.Add(offset)
	layout.advancedPanel = layout.advancedPanel.Add(offset)
	layout.memorySlider = layout.memorySlider.Add(offset)
	layout.cpuSlider = layout.cpuSlider.Add(offset)
	layout.cvmfsMirror = layout.cvmfsMirror.Add(offset)
	layout.actionDivider = layout.actionDivider.Add(offset)
	layout.skip = layout.skip.Add(offset)
	layout.button = layout.button.Add(offset)
	return layout
}

func (v *displayViewer) cvmfsMirrorChoices() []string {
	if !v.hasCVMFSMirrors() {
		return nil
	}
	return append([]string{""}, v.settings.CVMFSMirrors...)
}

func (v *displayViewer) cvmfsMirrorSelectionLabel() string {
	if mirror := strings.TrimSpace(v.settings.CVMFSMirror); mirror != "" {
		return mirrorDisplayName(mirror)
	}
	if mirror := strings.TrimSpace(v.settings.CVMFSAutoMirror); mirror != "" {
		return "Automatic · " + mirrorDisplayName(mirror)
	}
	if detail := strings.TrimSpace(v.preflight.CVMFSDetail); detail != "" {
		return "Automatic · " + detail
	}
	return "Automatic · measuring root catalogs"
}

func mirrorDisplayName(mirror string) string {
	mirror = strings.TrimSpace(strings.TrimRight(mirror, "/"))
	mirror = strings.TrimPrefix(mirror, "https://")
	mirror = strings.TrimPrefix(mirror, "http://")
	mirror = strings.TrimSuffix(mirror, "/cvmfs")
	return mirror
}

func (v *displayViewer) selectedCVMFSMirrorIndex() int {
	selected := strings.TrimSpace(v.settings.CVMFSMirror)
	for index, choice := range v.cvmfsMirrorChoices() {
		if choice == selected {
			return index
		}
	}
	return 0
}

func (v *displayViewer) cycleCVMFSMirror(delta int) {
	choices := v.cvmfsMirrorChoices()
	if len(choices) == 0 || delta == 0 {
		return
	}
	index := (v.selectedCVMFSMirrorIndex() + delta) % len(choices)
	if index < 0 {
		index += len(choices)
	}
	v.settings.CVMFSMirror = choices[index]
	v.ensureCVMFSMirrorVisible()
}

func (v *displayViewer) ensureCVMFSMirrorVisible() {
	choices := v.cvmfsMirrorChoices()
	visible := min(cvmfsMirrorMenuVisibleRows, len(choices))
	selected := v.selectedCVMFSMirrorIndex()
	if selected < v.cvmfsMirrorOffset {
		v.cvmfsMirrorOffset = selected
	} else if selected >= v.cvmfsMirrorOffset+visible {
		v.cvmfsMirrorOffset = selected - visible + 1
	}
	v.cvmfsMirrorOffset = min(max(0, len(choices)-visible), max(0, v.cvmfsMirrorOffset))
}

func (v *displayViewer) scrollCVMFSMirrorMenu(delta float32) {
	if delta > 0 {
		v.cvmfsMirrorOffset--
	} else if delta < 0 {
		v.cvmfsMirrorOffset++
	}
	choices := v.cvmfsMirrorChoices()
	visible := min(cvmfsMirrorMenuVisibleRows, len(choices))
	v.cvmfsMirrorOffset = min(max(0, len(choices)-visible), max(0, v.cvmfsMirrorOffset))
}

func cvmfsMirrorMenuLayout(field image.Rectangle, viewportHeight, choiceCount, offset int) (image.Rectangle, []image.Rectangle) {
	visible := min(cvmfsMirrorMenuVisibleRows, max(0, choiceCount-offset))
	if field.Empty() || visible == 0 {
		return image.Rectangle{}, nil
	}
	const rowHeight = 30
	height := visible * rowHeight
	top := field.Max.Y + 4
	if top+height > viewportHeight-12 {
		top = field.Min.Y - 4 - height
	}
	bounds := image.Rect(field.Min.X, top, field.Max.X, top+height)
	rows := make([]image.Rectangle, visible)
	for index := range rows {
		rows[index] = image.Rect(bounds.Min.X, bounds.Min.Y+index*rowHeight, bounds.Max.X, bounds.Min.Y+(index+1)*rowHeight)
	}
	return bounds, rows
}

func (v *displayViewer) cvmfsMirrorChoiceAt(point image.Point, layout startupControlLayout, viewportHeight float32) (string, bool) {
	choices := v.cvmfsMirrorChoices()
	_, rows := cvmfsMirrorMenuLayout(layout.cvmfsMirror, int(viewportHeight), len(choices), v.cvmfsMirrorOffset)
	for index, row := range rows {
		if point.In(row) {
			return choices[v.cvmfsMirrorOffset+index], true
		}
	}
	return "", false
}

func (v *displayViewer) drawCVMFSMirrorMenu(backingWidth, backingHeight int, scale float32, layout startupControlLayout, viewportHeight float32) {
	choices := v.cvmfsMirrorChoices()
	bounds, rows := cvmfsMirrorMenuLayout(layout.cvmfsMirror, int(viewportHeight), len(choices), v.cvmfsMirrorOffset)
	if bounds.Empty() {
		return
	}
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 8, uiBorderStrong, uiCanvas)
	selected := v.selectedCVMFSMirrorIndex()
	for index, row := range rows {
		choiceIndex := v.cvmfsMirrorOffset + index
		if choiceIndex == selected {
			v.drawRect(backingWidth, backingHeight, scale, float32(row.Min.X+2), float32(row.Min.Y+1), float32(row.Dx()-4), float32(row.Dy()-2), uiSurfaceHover)
		}
		label := mirrorDisplayName(choices[choiceIndex])
		if choiceIndex == 0 {
			label = "Automatic (fastest root catalog)"
		}
		v.text.BeginDraw()
		v.drawText(fitStartupText(label, float32(row.Dx()-24), 13), float32(row.Min.X+12), float32(row.Min.Y+20), 13, uiText)
		v.text.EndDraw()
	}
}

func (v *displayViewer) drawResourceSlider(backingWidth, backingHeight int, scale float32, bounds image.Rectangle, position float32, focused bool) {
	position = min(float32(1), max(float32(0), position))
	centerY := bounds.Min.Y + bounds.Dy()/2
	track := image.Rect(bounds.Min.X, centerY-3, bounds.Max.X, centerY+3)
	v.drawRoundedRect(backingWidth, backingHeight, scale, track, 3, uiSurfaceRaised)
	knobX := bounds.Min.X + int(float32(bounds.Dx())*position)
	if knobX > bounds.Min.X {
		v.drawRoundedRect(backingWidth, backingHeight, scale,
			image.Rect(bounds.Min.X, centerY-3, knobX, centerY+3), 3, uiPrimary)
	}
	knob := image.Rect(knobX-9, centerY-9, knobX+9, centerY+9)
	v.drawRoundedRect(backingWidth, backingHeight, scale, knob, 9, uiText)
	if focused {
		v.drawOutline(backingWidth, backingHeight, scale, bounds, uiAccent)
	}
}

func formatMemoryAmount(memoryMB uint64) string {
	if memoryMB%1024 == 0 {
		return fmt.Sprintf("%d GB", memoryMB/1024)
	}
	return fmt.Sprintf("%.1f GB", float64(memoryMB)/1024)
}

func (v *displayViewer) settingsFailureDetail() string {
	if v.preflightErr != nil {
		return v.preflightErr.Error()
	}
	if !v.preflightReady {
		return ""
	}
	var failures []string
	if !v.preflight.VirtualizationOK {
		detail := strings.TrimSpace(v.preflight.VirtualizationDetail)
		if detail == "" {
			detail = "Hardware virtualization is unavailable."
		}
		failures = append(failures, detail)
	}
	if !v.preflight.DiskOK {
		failures = append(failures, fmt.Sprintf("Not enough disk space: %s is free and %s is required.",
			formatBytes(v.preflight.FreeBytes), formatBytes(v.preflight.RequiredBytes)))
	}
	return strings.Join(failures, "\n")
}

func (v *displayViewer) drawSettingsFailure(backingWidth, backingHeight int, scale, left, top, width float32, detail string) {
	const lineHeight = float32(19)
	const visibleLines = 10
	lines := wrapStartupTextAll(detail, width-28, 15)
	maxScroll := max(0, len(lines)-visibleLines)
	v.startupDetailScroll = min(maxScroll, max(0, v.startupDetailScroll))
	bounds := image.Rect(int(left), int(top), int(left+width), int(top+230))
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10,
		uiErrorBorder, uiErrorSurface)
	v.text.BeginDraw()
	v.drawTextBold("STARTUP CHECK DETAILS", left+14, top+24, 13, uiError)
	for index, line := range lines[v.startupDetailScroll:min(len(lines), v.startupDetailScroll+visibleLines)] {
		v.drawText(line, left+14, top+49+float32(index)*lineHeight, 15, uiText)
	}
	footer := "Close this window after reviewing the error"
	if maxScroll > 0 {
		footer = fmt.Sprintf("Scroll or use arrow keys for the full error  ·  lines %d–%d of %d",
			v.startupDetailScroll+1, min(len(lines), v.startupDetailScroll+visibleLines), len(lines))
	}
	v.drawText(footer, left, top+260, 15, uiTextSecondary)
	v.text.EndDraw()
}

func (v *displayViewer) drawPreflightCard(
	backingWidth, backingHeight int,
	scale float32,
	bounds image.Rectangle,
	status, label, detail string,
) {
	left := float32(bounds.Min.X)
	top := float32(bounds.Min.Y)
	width := float32(bounds.Dx())
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10,
		uiSurfaceRaised, uiCanvas)
	statusColor := uiTextMuted
	badgeColor := uiSurfaceRaised
	switch status {
	case "PASS":
		statusColor = uiSuccess
		badgeColor = uiSuccessSurface
	case "FAIL":
		statusColor = uiErrorStrong
		badgeColor = uiErrorSurface
	case "UPDATE":
		statusColor = uiAccentSoft
		badgeColor = uiPrimary
	}
	badge := image.Rect(int(left+12), int(top+14), int(left+46), int(top+48))
	v.drawRoundedRect(backingWidth, backingHeight, scale, badge, 9, badgeColor)
	if status == "PASS" {
		v.drawCheckmark(backingWidth, backingHeight, scale, badge, statusColor)
	}
	v.text.BeginDraw()
	v.drawCenteredTextBold(statusSymbol(status), badge, 18, statusColor)
	v.drawTextBold(label, left+58, top+25, 16, uiText)
	v.drawText(fitStartupText(detail, width-70, 15), left+58, top+49, 15, uiAccentSoft)
	v.text.EndDraw()
}

func (v *displayViewer) drawSettingsOption(
	backingWidth, backingHeight int,
	scale float32,
	bounds image.Rectangle,
	title, detail string,
	enabled, focused, hovered bool,
) {
	borderColor := uiBorder
	fillColor := uiSurface
	if hovered {
		borderColor = uiBorderStrong
		fillColor = uiSurfaceHover
	}
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10,
		borderColor, fillColor)
	toggleX := float32(bounds.Min.X + 12)
	toggleY := float32(bounds.Min.Y + 19)
	toggleColor := uiSurfaceRaised
	knobX := toggleX + 3
	if enabled {
		toggleColor = uiPrimary
		knobX = toggleX + 19
	}
	v.drawRoundedRect(backingWidth, backingHeight, scale, image.Rect(int(toggleX), int(toggleY), int(toggleX+38), int(toggleY+22)), 11, toggleColor)
	v.drawRoundedRect(backingWidth, backingHeight, scale, image.Rect(int(knobX), int(toggleY+3), int(knobX+16), int(toggleY+19)), 8, uiText)
	if focused {
		v.drawOutline(backingWidth, backingHeight, scale, bounds, uiAccent)
	}
	v.text.BeginDraw()
	textX := float32(bounds.Min.X + 62)
	textWidth := float32(bounds.Dx() - 74)
	v.drawTextBold(title, textX, float32(bounds.Min.Y+24), 16, uiText)
	v.drawText(fitStartupText(detail, textWidth, 15), textX, float32(bounds.Min.Y+47), 15, uiAccentSoft)
	v.text.EndDraw()
}

func (v *displayViewer) drawSharedFolderOption(
	backingWidth, backingHeight int,
	scale float32,
	layout startupControlLayout,
) {
	bounds := layout.sharedFolder
	browse := layout.sharedBrowse
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10,
		uiBorder, uiSurface)
	browseColor := uiSurfaceRaised
	if v.startupHover == startupControlSharedFolder {
		browseColor = uiSurfaceHover
	}
	v.drawPanel(backingWidth, backingHeight, scale, browse, 8,
		uiBorderStrong, browseColor)
	if v.startupFocusVisible && v.startupFocus == startupControlSharedFolder {
		v.drawOutline(backingWidth, backingHeight, scale, browse, uiAccent)
	}
	detail := v.settings.SharedFolder
	detailColor := uiAccentSoft
	if v.folderSelectionErr != "" {
		detail = v.folderSelectionErr
		detailColor = uiError
	}
	textX := float32(bounds.Min.X + 14)
	textWidth := float32(browse.Min.X-bounds.Min.X) - 26
	v.text.BeginDraw()
	v.drawTextBold("Shared folder", textX, float32(bounds.Min.Y+21), 16, uiText)
	v.drawText(fitStartupText(detail, textWidth, 14), textX, float32(bounds.Min.Y+43), 14, detailColor)
	v.drawCenteredTextBold("Choose folder", browse, 14, uiText)
	v.text.EndDraw()
}

func preflightStatus(ready, ok bool) string {
	if !ready {
		return "WAIT"
	}
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func imagePreflightStatus(ready, ok bool, preflight startupPreflight) string {
	if ready && ok && preflight.hasUpdate() {
		return "UPDATE"
	}
	return preflightStatus(ready, ok)
}

func releasePreflightStatus(ready bool, preflight startupPreflight) string {
	if ready && preflight.ReleaseUpdate != nil {
		return "UPDATE"
	}
	if ready && !preflight.ReleaseChecked {
		return "SKIP"
	}
	return preflightStatus(ready, true)
}

func preflightVirtualizationDetail(ready bool, preflight startupPreflight) string {
	if !ready {
		return "Checking"
	}
	if preflight.VirtualizationOK {
		return "Available"
	}
	return preflight.VirtualizationDetail
}

func preflightDiskDetail(ready bool, preflight startupPreflight) string {
	if !ready {
		return "Checking"
	}
	return fmt.Sprintf("%s free · %s needed",
		formatBytes(preflight.FreeBytes), formatBytes(preflight.RequiredBytes))
}

func preflightImageDetail(ready bool, preflight startupPreflight, downloadRate float64) string {
	if !ready {
		return "Checking"
	}
	if preflight.Image.Available {
		return "Ready"
	}
	size := formatBytes(preflight.Image.BytesToDownload)
	eta := formatStartupETA(preflight.downloadETA(downloadRate))
	if preflight.hasUpdate() {
		return fmt.Sprintf("%s update · %s", size, eta)
	}
	return fmt.Sprintf("%s download · %s", size, eta)
}

func preflightReleaseDetail(ready bool, preflight startupPreflight) string {
	if !ready {
		return "Checking GitHub Releases"
	}
	if !preflight.ReleaseChecked {
		if preflight.ReleaseCheckDetail != "" {
			return preflight.ReleaseCheckDetail
		}
		return "Check skipped"
	}
	if preflight.ReleaseUpdate == nil {
		return "Current"
	}
	detail := preflight.ReleaseUpdate.Version
	if preflight.ReleaseUpdate.Size > 0 {
		detail += " · " + formatBytes(preflight.ReleaseUpdate.Size)
	}
	return detail
}

func cvmfsPreflightStatus(ready bool, preflight startupPreflight) string {
	if !preflight.CVMFSRequired {
		return "SKIP"
	}
	return preflightStatus(ready, preflight.CVMFSOK)
}

func cvmfsPreflightDetail(ready bool, preflight startupPreflight) string {
	if !ready {
		return "Detecting the nearest mirror"
	}
	if mirror := mirrorDisplayName(preflight.CVMFSMirror); mirror != "" {
		if detail := strings.TrimSpace(preflight.CVMFSDetail); detail != "" {
			return mirror + " · " + detail
		}
		return mirror
	}
	if detail := strings.TrimSpace(preflight.CVMFSDetail); detail != "" {
		return detail
	}
	return "No reachable mirror detected"
}

func (v *displayViewer) drawStartup(backingWidth, backingHeight int, now time.Time) {
	v.gl.Enable(gl.Blend)
	scale := normalizedDisplayScale(v.window.Scale())
	width := float32(backingWidth) / scale
	height := float32(backingHeight) / scale
	v.gl.UseProgram(v.program)
	v.drawBackground(backingWidth, backingHeight)

	chromeTop := v.chromeContentTop()
	layout := calculateStartupScreenLayout(width, max(float32(1), height-chromeTop))
	layout.panel = layout.panel.Add(image.Pt(0, int(chromeTop)))
	layout.brand = layout.brand.Add(image.Pt(0, int(chromeTop)))
	layout.state = layout.state.Add(image.Pt(0, int(chromeTop)))
	layout.bar = layout.bar.Add(image.Pt(0, int(chromeTop)))
	for index := range layout.steps {
		layout.steps[index] = layout.steps[index].Add(image.Pt(0, int(chromeTop)))
	}
	panel := layout.panel
	panelWidth := float32(panel.Dx())
	left := float32(panel.Min.X)
	contentTop := float32(panel.Min.Y)
	brand := layout.brand
	v.drawTexture(
		v.brandTexture, backingWidth, backingHeight, scale,
		float32(brand.Min.X), float32(brand.Min.Y), float32(brand.Dx()), float32(brand.Dy()),
	)

	v.text.SetViewport(int32(width), int32(height))
	v.text.SetScale(scale)

	state := "IN PROGRESS"
	stateColor := uiAccentSoft
	stateBackground := uiSurfaceHover
	if v.startup.Failed {
		state = "INTERRUPTED"
		stateColor = uiError
		stateBackground = uiErrorSurface
	}
	stateBounds := layout.state
	v.drawRoundedRect(backingWidth, backingHeight, scale, stateBounds, stateBounds.Dy()/2, stateBackground)
	v.text.BeginDraw()
	v.drawTextBold(productName(), left+60, contentTop+21, 19, uiText)
	v.drawText(startupEyebrow(v.startup), left+60, contentTop+43, 14, uiAccent)
	v.drawCenteredTextBold(state, stateBounds, 13, stateColor)
	v.text.EndDraw()
	if v.startup.Failed {
		v.drawStartupFailure(backingWidth, backingHeight, scale, left, contentTop+82, panelWidth)
		return
	}
	if v.startupTerminal != nil && v.startupTerminal.Snapshot().BytesRead != 0 {
		v.drawStartupSerial(backingWidth, backingHeight, scale, left, contentTop+82, panelWidth)
		return
	}
	v.drawStartupChecklist(backingWidth, backingHeight, scale, left, contentTop+84, panelWidth, now)

	accent := uiPrimary
	drawBar := func(y, height, fraction float32, determinate bool, barColor color.RGBA) {
		v.drawRect(backingWidth, backingHeight, scale, left, y, panelWidth, height, uiSurfaceRaised)
		if determinate {
			fraction = min(float32(1), max(float32(0), fraction))
			v.drawRect(backingWidth, backingHeight, scale, left, y, panelWidth*fraction, height, barColor)
			return
		}
		sweepWidth := panelWidth * 0.26
		distance := panelWidth + sweepWidth
		offset := float32(math.Mod(float64(now.UnixNano())/float64(1400*time.Millisecond), 1))*distance - sweepWidth
		visibleLeft := max(float32(0), offset)
		visibleRight := min(panelWidth, offset+sweepWidth)
		if visibleRight > visibleLeft {
			v.drawRect(backingWidth, backingHeight, scale, left+visibleLeft, y, visibleRight-visibleLeft, height, barColor)
		}
	}

	if v.startup.ImagePipeline {
		downloadText := formatStartupDownload(v.startup)
		indexText := formatStartupIndex(v.startup)
		v.text.BeginDraw()
		v.drawTextBold("DOWNLOAD", left, contentTop+174, 15, uiAccent)
		downloadWidth := float32(v.text.GetAdvance(v.font, 15, downloadText))
		v.drawText(downloadText, left+panelWidth-downloadWidth, contentTop+174, 15, uiTextSecondary)
		v.drawText("Indexing concurrently", left, contentTop+215, 15, uiTextMuted)
		indexWidth := float32(v.text.GetAdvance(v.font, 15, indexText))
		v.drawText(indexText, left+panelWidth-indexWidth, contentTop+215, 15, uiTextMuted)
		v.text.EndDraw()
		drawBar(contentTop+181, 9, float32(v.startup.DownloadProgress), true, accent)
	} else {
		if v.startup.Failed {
			accent = uiErrorStrong
		}
		drawBar(float32(layout.bar.Min.Y), float32(layout.bar.Dy()), float32(v.startup.Progress), v.startup.Determinate || v.startup.Failed, accent)
		transfer := formatStartupTransfer(v.startup)
		eta := formatStartupETA(v.startup.ETA)
		v.text.BeginDraw()
		v.drawText(transfer, left, contentTop+220, 15, uiAccentSoft)
		if eta != "" {
			etaWidth := float32(v.text.GetAdvance(v.font, 15, eta))
			v.drawText(eta, left+panelWidth-etaWidth, contentTop+220, 15, uiAccentSoft)
		}
		v.text.EndDraw()
	}

	labels := []string{"IMAGE", "START VM", "DESKTOP"}
	for index, label := range labels {
		bounds := layout.steps[index]
		x := float32(bounds.Min.X)
		y := float32(bounds.Min.Y)
		stepColor := uiBorder
		textColor := uiTextSecondary
		if startupPhase(index) < v.startup.Phase {
			stepColor = uiPrimary
			textColor = uiAccent
		} else if startupPhase(index) == v.startup.Phase {
			stepColor = uiAccent
			textColor = uiText
		}
		v.drawRect(backingWidth, backingHeight, scale, x, y, float32(bounds.Dx()), 3, stepColor)
		v.text.BeginDraw()
		v.drawTextBold(fmt.Sprintf("%d · %s", index+1, label), x, y+31, 15, textColor)
		v.text.EndDraw()
	}

	v.text.BeginDraw()
	footer := "Esc for settings  ·  Close this window to cancel"
	if v.startup.Failed {
		footer = "Close this window to stop"
	}
	v.drawText(footer, left, contentTop+338, 15, uiTextSecondary)
	v.text.EndDraw()
}

func (v *displayViewer) drawStartupFailure(backingWidth, backingHeight int, scale, left, top, width float32) {
	const lineHeight = float32(19)
	const visibleLines = 9
	lines := wrapStartupTextAll(v.startup.Detail, width-28, 15)
	if len(lines) == 0 {
		lines = []string{"An unexpected error occurred"}
	}
	maxScroll := max(0, len(lines)-visibleLines)
	v.startupDetailScroll = min(maxScroll, max(0, v.startupDetailScroll))
	bounds := image.Rect(int(left), int(top), int(left+width), int(top+226))
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10,
		uiErrorBorder, uiErrorSurface)

	v.text.BeginDraw()
	v.drawTextBold(v.startup.Title, left+14, top+25, 16, uiError)
	for index, line := range lines[v.startupDetailScroll:min(len(lines), v.startupDetailScroll+visibleLines)] {
		v.drawText(line, left+14, top+49+float32(index)*lineHeight, 15, uiText)
	}
	footer := "Esc for settings  ·  Close this window to stop"
	if maxScroll > 0 {
		footer = fmt.Sprintf("Scroll or use arrow keys for the full error  ·  lines %d–%d of %d",
			v.startupDetailScroll+1, min(len(lines), v.startupDetailScroll+visibleLines), len(lines))
	}
	v.drawText(footer, left, top+256, 15, uiTextSecondary)
	v.text.EndDraw()
}

func (v *displayViewer) drawStartupSerial(backingWidth, backingHeight int, scale, left, top, width float32) {
	const (
		fontSize    = float32(13)
		lineHeight  = float32(17)
		visibleRows = 12
	)
	cellWidth := float32(v.text.GetAdvance(v.fontMono, float64(fontSize), "M"))
	cols := max(20, int((width-24)/cellWidth))
	v.startupTerminal.Resize(ptyterm.Size{Cols: cols, Rows: visibleRows})
	snapshot := v.startupTerminal.Snapshot()
	bounds := image.Rect(int(left), int(top), int(left+width), int(top+246))
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 8,
		uiSurfaceRaised, uiCanvas)
	textLeft := left + 12
	textTop := top + 37
	for row, cells := range snapshot.Cells {
		for column := 0; column < len(cells); {
			_, bg := startupTerminalCellColors(cells[column].Attr)
			end := column + 1
			for end < len(cells) {
				_, nextBG := startupTerminalCellColors(cells[end].Attr)
				if nextBG != bg {
					break
				}
				end++
			}
			if bg.A != 0 {
				v.drawRect(backingWidth, backingHeight, scale,
					textLeft+float32(column)*cellWidth, textTop+float32(row)*lineHeight-13,
					float32(end-column)*cellWidth, lineHeight, bg)
			}
			column = end
		}
		for column := 0; column < len(cells); {
			if !cells[column].Attr.Underline {
				column++
				continue
			}
			fg, _ := startupTerminalCellColors(cells[column].Attr)
			end := column + 1
			for end < len(cells) && cells[end].Attr == cells[column].Attr && cells[end].Attr.Underline {
				end++
			}
			v.drawRect(backingWidth, backingHeight, scale,
				textLeft+float32(column)*cellWidth, textTop+float32(row)*lineHeight+2,
				float32(end-column)*cellWidth, 1, fg)
			column = end
		}
	}
	v.text.BeginDraw()
	v.drawTextBold("BOOT CONSOLE", textLeft, top+19, 12, uiAccent)
	for row, cells := range snapshot.Cells {
		for column := 0; column < len(cells); {
			attr := cells[column].Attr
			end := column + 1
			for end < len(cells) && cells[end].Attr == attr {
				end++
			}
			var text strings.Builder
			for _, cell := range cells[column:end] {
				text.WriteRune(startupTerminalRune(cell.R))
			}
			fg, _ := startupTerminalCellColors(attr)
			font := v.fontMono
			if attr.Bold {
				font = v.fontMonoBold
			}
			value := strings.TrimRight(text.String(), " ")
			if value != "" {
				v.drawTextFont(font, value, textLeft+float32(column)*cellWidth,
					textTop+float32(row)*lineHeight, fontSize, fg)
			}
			column = end
		}
	}
	v.drawText("Esc for settings  ·  Console follows the latest boot output", left, top+276, 15,
		uiTextSecondary)
	v.text.EndDraw()
}

func startupTerminalRune(r rune) rune {
	if r == 0 || r == ' ' {
		return ' '
	}
	if r < ' ' || r > '~' {
		return '?'
	}
	return r
}

func startupTerminalCellColors(attr ptyterm.Attr) (color.RGBA, color.RGBA) {
	fg := startupTerminalColor(attr.FG, uiTextSecondary, attr.Bold)
	bg := startupTerminalColor(attr.BG, color.RGBA{}, false)
	if attr.Inverse {
		originalFG := fg
		if bg.A == 0 {
			fg = uiCanvas
		} else {
			fg = bg
		}
		bg = originalFG
	}
	return fg, bg
}

func startupTerminalColor(value int, fallback color.RGBA, bold bool) color.RGBA {
	if value < 0 {
		return fallback
	}
	if value >= 1<<24 {
		return color.RGBA{R: uint8(value >> 16), G: uint8(value >> 8), B: uint8(value), A: 255}
	}
	palette := [...]color.RGBA{
		{R: 24, G: 20, B: 27, A: 255}, {R: 224, G: 90, B: 90, A: 255},
		{R: 108, G: 190, B: 124, A: 255}, {R: 224, G: 190, B: 83, A: 255},
		{R: 102, G: 153, B: 235, A: 255}, {R: 189, G: 121, B: 224, A: 255},
		{R: 91, G: 190, B: 202, A: 255}, {R: 218, G: 211, B: 222, A: 255},
		{R: 104, G: 94, B: 110, A: 255}, {R: 255, G: 126, B: 126, A: 255},
		{R: 143, G: 226, B: 156, A: 255}, {R: 250, G: 238, B: 82, A: 255},
		{R: 135, G: 180, B: 255, A: 255}, {R: 221, G: 170, B: 255, A: 255},
		{R: 117, G: 222, B: 229, A: 255}, {R: 250, G: 247, B: 251, A: 255},
	}
	if bold && value < 8 {
		value += 8
	}
	if value < len(palette) {
		return palette[value]
	}
	if value >= 16 && value <= 231 {
		cube := value - 16
		steps := [...]uint8{0, 95, 135, 175, 215, 255}
		return color.RGBA{R: steps[(cube/36)%6], G: steps[(cube/6)%6], B: steps[cube%6], A: 255}
	}
	if value >= 232 && value <= 255 {
		shade := uint8(8 + (value-232)*10)
		return color.RGBA{R: shade, G: shade, B: shade, A: 255}
	}
	return fallback
}

func (v *displayViewer) drawStartupChecklist(
	backingWidth, backingHeight int,
	scale, left, top, width float32,
	now time.Time,
) {
	const (
		visibleRows = 3
		rowHeight   = float32(24)
		listHeight  = rowHeight * visibleRows
	)
	if len(v.startupChecklist) == 0 {
		return
	}

	finalFirst := max(0, len(v.startupChecklist)-visibleRows)
	first := finalFirst
	offset := float32(0)
	const scrollDuration = 260 * time.Millisecond
	if first > 0 {
		elapsed := now.Sub(v.checklistChangedAt)
		if elapsed >= 0 && elapsed < scrollDuration {
			position := float32(elapsed) / float32(scrollDuration)
			eased := 1 - float32(math.Pow(float64(1-position), 3))
			offset = rowHeight * (1 - eased)
			first--
		}
	}

	scissorLeft := int32(math.Round(float64(left * scale)))
	scissorBottom := int32(math.Round(float64(float32(backingHeight) - (top+listHeight)*scale)))
	scissorWidth := int32(math.Ceil(float64(width * scale)))
	scissorHeight := int32(math.Ceil(float64(listHeight * scale)))
	v.gl.Enable(gl.ScissorTest)
	v.gl.Scissor(scissorLeft, scissorBottom, scissorWidth, scissorHeight)

	for index := first; index < len(v.startupChecklist); index++ {
		row := index - finalFirst
		y := top + float32(row)*rowHeight + offset
		icon := image.Rect(int(left), int(y+3), int(left+18), int(y+21))
		if index < len(v.startupChecklist)-1 {
			v.drawRoundedRect(backingWidth, backingHeight, scale, icon, 9, uiPrimary)
			v.drawCheckmark(backingWidth, backingHeight, scale, icon, uiAccentSoft)
			continue
		}
		dotColor := uiAccent
		if v.startupChecklist[index].Failed {
			dotColor = uiErrorStrong
		}
		dot := image.Rect(int(left+5), int(y+8), int(left+13), int(y+16))
		v.drawRoundedRect(backingWidth, backingHeight, scale, dot, 4, dotColor)
	}

	v.text.BeginDraw()
	for index := first; index < len(v.startupChecklist); index++ {
		row := index - finalFirst
		y := top + float32(row)*rowHeight + offset
		item := v.startupChecklist[index]
		line := item.Title
		if item.Detail != "" {
			line += "  ·  " + item.Detail
		}
		line = fitStartupText(line, width-32, 16)
		if index == len(v.startupChecklist)-1 {
			textColor := uiText
			if item.Failed {
				textColor = uiError
			}
			v.drawTextBold(line, left+30, y+18, 16, textColor)
		} else {
			v.drawText(line, left+30, y+18, 16, uiTextSecondary)
		}
	}
	v.text.EndDraw()
	v.gl.Disable(gl.ScissorTest)
}

func (v *displayViewer) loadBrandTexture() error {
	decoded, err := png.Decode(bytes.NewReader(appConfig.BrandPNG))
	if err != nil {
		return fmt.Errorf("decode embedded %s artwork: %w", productName(), err)
	}
	bounds := decoded.Bounds()
	rgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	imagedraw.Draw(rgba, rgba.Bounds(), decoded, bounds.Min, imagedraw.Src)
	if len(rgba.Pix) == 0 {
		return fmt.Errorf("embedded %s artwork is empty", productName())
	}

	v.brandWidth, v.brandHeight = bounds.Dx(), bounds.Dy()
	v.gl.GenTextures(1, &v.brandTexture)
	v.gl.ActiveTexture(gl.Texture0)
	v.gl.BindTexture(gl.Texture2D, v.brandTexture)
	v.gl.TexParameteri(gl.Texture2D, gl.TextureMinFilter, gl.Linear)
	v.gl.TexParameteri(gl.Texture2D, gl.TextureMagFilter, gl.Linear)
	v.gl.TexParameteri(gl.Texture2D, gl.TextureWrapS, gl.ClampToEdge)
	v.gl.TexParameteri(gl.Texture2D, gl.TextureWrapT, gl.ClampToEdge)
	v.gl.PixelStorei(gl.UnpackAlignment, 1)
	v.gl.TexImage2D(gl.Texture2D, 0, int32(gl.RGBA), int32(rgba.Rect.Dx()), int32(rgba.Rect.Dy()),
		0, gl.RGBA, gl.UnsignedByte, unsafe.Pointer(&rgba.Pix[0]))
	return nil
}

func (v *displayViewer) drawTexture(texture uint32, backingWidth, backingHeight int, scale, x, y, width, height float32) {
	if texture == 0 || width <= 0 || height <= 0 {
		return
	}
	left := int32(math.Round(float64(x * scale)))
	bottom := int32(math.Round(float64(float32(backingHeight) - (y+height)*scale)))
	pixelWidth := int32(math.Ceil(float64(width * scale)))
	pixelHeight := int32(math.Ceil(float64(height * scale)))
	v.gl.Viewport(left, bottom, pixelWidth, pixelHeight)
	v.gl.UseProgram(v.program)
	v.gl.ActiveTexture(gl.Texture0)
	v.gl.BindTexture(gl.Texture2D, texture)
	v.gl.BindVertexArray(v.vertexArray)
	v.gl.DrawArrays(gl.Triangles, 0, 6)
	v.gl.Viewport(0, 0, int32(backingWidth), int32(backingHeight))
}

func cvmfsChromeStatusBounds(width float32, insets window.TitleBarInsets) image.Rectangle {
	right := max(1, int(width-max(float32(12), insets.Right)))
	left := min(right-1, max(int(insets.Left)+150, right-300))
	left = max(0, left)
	return image.Rect(left, 4, right, int(appChromeHeight)-4)
}

func cvmfsChromeDetailBounds(width float32, insets window.TitleBarInsets, transfers int) image.Rectangle {
	status := cvmfsChromeStatusBounds(width, insets)
	panelWidth := min(540, max(320, int(width)-24))
	right := status.Max.X
	left := max(12, right-panelWidth)
	height := 54 + min(6, max(1, transfers))*52
	return image.Rect(left, int(appChromeHeight)+6, right, int(appChromeHeight)+6+height)
}

func (v *displayViewer) drawAppChrome(backingWidth, backingHeight int) {
	scale := normalizedDisplayScale(v.window.Scale())
	width := float32(backingWidth) / scale
	v.drawRect(backingWidth, backingHeight, scale, 0, 0, width, appChromeHeight, uiCanvas)
	v.drawRect(backingWidth, backingHeight, scale, 0, appChromeHeight-1, width, 1, uiBorder)

	statusBounds := cvmfsChromeStatusBounds(width, v.chromeInsets)
	v.text.SetViewport(int32(width), int32(float32(backingHeight)/scale))
	v.text.SetScale(scale)

	statusColor := uiAccent
	statusBackground := uiSurfaceRaised
	label := cvmfsChromeLabel(v.cvmfsStatus, time.Now())
	switch v.cvmfsStatus.State {
	case "error":
		statusColor = uiError
		statusBackground = uiErrorSurface
	}
	v.drawRoundedRect(backingWidth, backingHeight, scale, statusBounds, statusBounds.Dy()/2, statusBackground)
	if v.cvmfsStatus.State == "downloading" {
		fraction := float32(v.cvmfsStatus.Progress)
		if fraction <= 0 {
			fraction = 0.08
		}
		fraction = min(float32(1), max(float32(0.02), fraction))
		v.drawRect(backingWidth, backingHeight, scale, float32(statusBounds.Min.X)+8, float32(statusBounds.Max.Y)-4,
			float32(statusBounds.Dx()-16)*fraction, 2, uiPrimary)
	}
	controls := chromeWindowControlButtons(width, platformWindowControlsAvailable())
	for _, button := range controls {
		if v.chromeControlHover == button.control {
			background := uiSurfaceHover
			if button.control == chromeWindowControlClose {
				background = uiError
			}
			v.drawRect(backingWidth, backingHeight, scale,
				float32(button.bounds.Min.X), float32(button.bounds.Min.Y),
				float32(button.bounds.Dx()), float32(button.bounds.Dy()), background)
		}
		centerX := float32(button.bounds.Min.X+button.bounds.Max.X) / 2
		centerY := float32(button.bounds.Min.Y+button.bounds.Max.Y) / 2
		switch button.control {
		case chromeWindowControlMinimize:
			v.drawRect(backingWidth, backingHeight, scale, centerX-5, centerY+3, 10, 1, uiText)
		case chromeWindowControlMaximize:
			v.drawOutline(backingWidth, backingHeight, scale,
				image.Rect(int(centerX)-5, int(centerY)-4, int(centerX)+5, int(centerY)+4), uiText)
		}
	}
	// Draw title-bar text after all chrome shapes so it remains on top.
	v.text.BeginDraw()
	v.drawCenteredTextBold(productName(), image.Rect(0, 0, int(width), int(appChromeHeight)), 13, uiText)
	v.drawCenteredTextBold(fitStartupText(label, float32(statusBounds.Dx()-24), 13), statusBounds, 13, statusColor)
	for _, button := range controls {
		if button.control == chromeWindowControlClose {
			v.drawCenteredTextBold("×", button.bounds, 16, uiText)
		}
	}
	v.text.EndDraw()
	if v.cvmfsExpanded {
		v.drawCVMFSDetails(backingWidth, backingHeight, scale, width)
	}
}

func chromeWindowControlButtons(width float32, available bool) []chromeWindowControlButton {
	if !available {
		return nil
	}
	const buttonWidth = 46
	right := int(width)
	left := max(0, right-3*buttonWidth)
	return []chromeWindowControlButton{
		{control: chromeWindowControlMinimize, bounds: image.Rect(left, 0, left+buttonWidth, int(appChromeHeight))},
		{control: chromeWindowControlMaximize, bounds: image.Rect(left+buttonWidth, 0, left+2*buttonWidth, int(appChromeHeight))},
		{control: chromeWindowControlClose, bounds: image.Rect(left+2*buttonWidth, 0, right, int(appChromeHeight))},
	}
}

func cvmfsChromeLabel(status client.CVMFSStatusResponse, now time.Time) string {
	if status.State == "error" {
		return "CVMFS needs attention"
	}
	count := len(status.ActiveTransfers)
	if count == 0 {
		return "CVMFS ready"
	}
	label := fmt.Sprintf("CVMFS · %d download", count)
	if count != 1 {
		label += "s"
	}
	if rate := cvmfsDownloadRate(status.ActiveTransfers, now); rate > 0 {
		label += " · " + formatBytes(int64(rate)) + "/s"
	}
	return label
}

func cvmfsDownloadRate(transfers []client.CVMFSTransferState, now time.Time) float64 {
	var rate float64
	for _, transfer := range transfers {
		started, err := time.Parse(time.RFC3339Nano, transfer.StartedAt)
		if err != nil || transfer.Bytes <= 0 {
			continue
		}
		elapsed := now.Sub(started).Seconds()
		if elapsed > 0 {
			rate += float64(transfer.Bytes) / elapsed
		}
	}
	return rate
}

func (v *displayViewer) drawCVMFSDetails(backingWidth, backingHeight int, scale, width float32) {
	bounds := cvmfsChromeDetailBounds(width, v.chromeInsets, len(v.cvmfsStatus.ActiveTransfers))
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10, uiBorderStrong, uiCanvas)
	v.text.BeginDraw()
	v.drawTextBold("CVMFS transfers", float32(bounds.Min.X+16), float32(bounds.Min.Y+24), 15, uiText)
	summary := "No active downloads"
	if len(v.cvmfsStatus.ActiveTransfers) != 0 {
		summary = fmt.Sprintf("%s of %s", formatBytes(v.cvmfsStatus.Bytes), formatBytes(v.cvmfsStatus.TotalBytes))
	}
	v.drawText(summary, float32(bounds.Min.X+16), float32(bounds.Min.Y+44), 13, uiTextSecondary)
	v.text.EndDraw()
	transfers := v.cvmfsStatus.ActiveTransfers
	if len(transfers) > 6 {
		transfers = transfers[:6]
	}
	for index, transfer := range transfers {
		y := float32(bounds.Min.Y + 64 + index*52)
		labelWidth := float32(bounds.Dx() - 32)
		v.text.BeginDraw()
		v.drawText(fitStartupText(transfer.Path, labelWidth, 13), float32(bounds.Min.X+16), y, 13, uiText)
		amount := formatBytes(transfer.Bytes)
		if transfer.TotalBytes > 0 {
			amount += " / " + formatBytes(transfer.TotalBytes)
		}
		amountWidth := float32(v.text.GetAdvance(v.font, 12, amount))
		v.drawText(amount, float32(bounds.Max.X-16)-amountWidth, y+17, 12, uiTextSecondary)
		v.text.EndDraw()
		barWidth := float32(bounds.Dx() - 32)
		v.drawRect(backingWidth, backingHeight, scale, float32(bounds.Min.X+16), y+24, barWidth, 4, uiSurfaceRaised)
		fraction := float32(transfer.Progress)
		if fraction <= 0 {
			fraction = 0.04
		}
		v.drawRect(backingWidth, backingHeight, scale, float32(bounds.Min.X+16), y+24,
			barWidth*min(float32(1), fraction), 4, uiPrimary)
	}
}

func startupEyebrow(progress startupProgress) string {
	if progress.Failed {
		return "STARTUP INTERRUPTED"
	}
	switch progress.Phase {
	case startupImage:
		return "PREPARING IMAGE"
	case startupBoot:
		return "STARTING VIRTUAL MACHINE"
	default:
		return "STARTING " + strings.ToUpper(productName())
	}
}

func (v *displayViewer) drawRect(backingWidth, backingHeight int, scale, x, y, width, height float32, col color.RGBA) {
	if width <= 0 || height <= 0 {
		return
	}
	left := int32(math.Round(float64(x * scale)))
	bottom := int32(math.Round(float64(float32(backingHeight) - (y+height)*scale)))
	pixelWidth := int32(math.Ceil(float64(width * scale)))
	pixelHeight := int32(math.Ceil(float64(height * scale)))
	v.gl.Enable(gl.ScissorTest)
	v.gl.Scissor(left, bottom, pixelWidth, pixelHeight)
	v.gl.ClearColor(float32(col.R)/255, float32(col.G)/255, float32(col.B)/255, float32(col.A)/255)
	v.gl.Clear(gl.ColorBufferBit)
	v.gl.Disable(gl.ScissorTest)
}

func (v *displayViewer) drawBackground(backingWidth, backingHeight int) {
	v.gl.Viewport(0, 0, int32(backingWidth), int32(backingHeight))
	v.gl.UseProgram(v.backgroundProgram)
	v.gl.Uniform4f(v.backgroundGeometry, float32(backingWidth), float32(backingHeight), 0, 0)
	v.gl.BindVertexArray(v.vertexArray)
	v.gl.DrawArrays(gl.Triangles, 0, 6)
	v.gl.UseProgram(v.program)
}

func (v *displayViewer) drawCheckmark(
	backingWidth, backingHeight int,
	scale float32,
	bounds image.Rectangle,
	col color.RGBA,
) {
	left := int32(math.Round(float64(float32(bounds.Min.X) * scale)))
	bottom := int32(math.Round(float64(float32(backingHeight) - float32(bounds.Max.Y)*scale)))
	pixelWidth := int32(math.Ceil(float64(float32(bounds.Dx()) * scale)))
	pixelHeight := int32(math.Ceil(float64(float32(bounds.Dy()) * scale)))
	v.gl.Viewport(left, bottom, pixelWidth, pixelHeight)
	v.gl.UseProgram(v.checkmarkProgram)
	v.gl.Uniform4f(v.checkmarkColor,
		float32(col.R)/255, float32(col.G)/255, float32(col.B)/255, float32(col.A)/255)
	v.gl.BindVertexArray(v.vertexArray)
	v.gl.DrawArrays(gl.Triangles, 0, 6)
	v.gl.Viewport(0, 0, int32(backingWidth), int32(backingHeight))
	v.gl.UseProgram(v.program)
}

func (v *displayViewer) drawRoundedRect(
	backingWidth, backingHeight int,
	scale float32,
	bounds image.Rectangle,
	radius int,
	col color.RGBA,
) {
	radius = min(radius, min(bounds.Dx()/2, bounds.Dy()/2))
	if radius <= 1 {
		v.drawRect(backingWidth, backingHeight, scale, float32(bounds.Min.X), float32(bounds.Min.Y), float32(bounds.Dx()), float32(bounds.Dy()), col)
		return
	}
	left := int32(math.Round(float64(float32(bounds.Min.X) * scale)))
	bottom := int32(math.Round(float64(float32(backingHeight) - float32(bounds.Max.Y)*scale)))
	pixelWidth := int32(math.Ceil(float64(float32(bounds.Dx()) * scale)))
	pixelHeight := int32(math.Ceil(float64(float32(bounds.Dy()) * scale)))
	if pixelWidth <= 0 || pixelHeight <= 0 {
		return
	}
	v.gl.Viewport(left, bottom, pixelWidth, pixelHeight)
	v.gl.UseProgram(v.roundedRectProgram)
	v.gl.Uniform4f(v.roundedRectColor,
		float32(col.R)/255, float32(col.G)/255, float32(col.B)/255, float32(col.A)/255)
	v.gl.Uniform4f(v.roundedRectGeometry,
		float32(pixelWidth), float32(pixelHeight), float32(radius)*scale, 1)
	v.gl.BindVertexArray(v.vertexArray)
	v.gl.DrawArrays(gl.Triangles, 0, 6)
	v.gl.Viewport(0, 0, int32(backingWidth), int32(backingHeight))
	v.gl.UseProgram(v.program)
}

func (v *displayViewer) drawPanel(
	backingWidth, backingHeight int,
	scale float32,
	bounds image.Rectangle,
	radius int,
	border, fill color.RGBA,
) {
	v.drawRoundedRect(backingWidth, backingHeight, scale, bounds, radius, border)
	v.drawRoundedRect(backingWidth, backingHeight, scale, bounds.Inset(1), max(1, radius-1), fill)
}

func (v *displayViewer) drawOutline(
	backingWidth, backingHeight int,
	scale float32,
	bounds image.Rectangle,
	col color.RGBA,
) {
	const thickness = float32(2)
	x := float32(bounds.Min.X)
	y := float32(bounds.Min.Y)
	width := float32(bounds.Dx())
	height := float32(bounds.Dy())
	v.drawRect(backingWidth, backingHeight, scale, x, y, width, thickness, col)
	v.drawRect(backingWidth, backingHeight, scale, x, y+height-thickness, width, thickness, col)
	v.drawRect(backingWidth, backingHeight, scale, x, y, thickness, height, col)
	v.drawRect(backingWidth, backingHeight, scale, x+width-thickness, y, thickness, height, col)
}

func (v *displayViewer) drawText(value string, x, y, size float32, col color.RGBA) {
	v.drawTextFont(v.font, value, x, y, size, col)
}

func (v *displayViewer) drawTextFont(font int, value string, x, y, size float32, col color.RGBA) {
	if strings.TrimSpace(value) == "" {
		return
	}
	rgba := [4]float32{
		float32(col.R) / 255,
		float32(col.G) / 255,
		float32(col.B) / 255,
		float32(col.A) / 255,
	}
	v.text.DrawText(font, float64(size), float64(x), float64(y), value, rgba)
}

func (v *displayViewer) drawTextBold(value string, x, y, size float32, col color.RGBA) {
	if strings.TrimSpace(value) == "" {
		return
	}
	rgba := [4]float32{float32(col.R) / 255, float32(col.G) / 255, float32(col.B) / 255, float32(col.A) / 255}
	v.text.DrawText(v.fontBold, float64(size), float64(x), float64(y), value, rgba)
}

func (v *displayViewer) drawCenteredTextBold(value string, bounds image.Rectangle, size float32, col color.RGBA) {
	textWidth := float32(v.text.GetAdvance(v.fontBold, float64(size), value))
	x := float32(bounds.Min.X) + (float32(bounds.Dx())-textWidth)/2
	y := float32(bounds.Min.Y) + (float32(bounds.Dy())+size)/2
	v.drawTextBold(value, x, y, size, col)
}

func statusSymbol(status string) string {
	switch status {
	case "PASS":
		return ""
	case "FAIL":
		return "!"
	case "UPDATE":
		return "UP"
	case "SKIP":
		return "--"
	default:
		return "..."
	}
}

func fitStartupText(value string, width, size float32) string {
	value = strings.TrimSpace(value)
	if value == "" || width <= 0 || size <= 0 {
		return value
	}
	// Roboto Mono's average advance is close to 0.6em. This conservative
	// bound prevents long backend details from running out of the window.
	limit := max(8, int(width/(size*0.62)))
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:max(1, limit-1)]) + "…"
}

func wrapStartupText(value string, width, size float32, maxLines int) []string {
	lines := wrapStartupTextAll(value, width, size)
	if len(lines) <= maxLines {
		return lines
	}
	lines = lines[:maxLines]
	lines[maxLines-1] = fitStartupText(lines[maxLines-1]+" …", width, size)
	return lines
}

func wrapStartupTextAll(value string, width, size float32) []string {
	value = strings.TrimSpace(value)
	if value == "" || width <= 0 || size <= 0 {
		return nil
	}
	limit := max(8, int(width/(size*0.62)))
	words := strings.Fields(value)
	var expanded []string
	for _, word := range words {
		runes := []rune(word)
		for len(runes) > limit {
			expanded = append(expanded, string(runes[:limit]))
			runes = runes[limit:]
		}
		if len(runes) != 0 {
			expanded = append(expanded, string(runes))
		}
	}
	words = expanded
	lines := make([]string, 0, len(words))
	for len(words) > 0 {
		line := words[0]
		words = words[1:]
		for len(words) > 0 && len([]rune(line))+1+len([]rune(words[0])) <= limit {
			line += " " + words[0]
			words = words[1:]
		}
		lines = append(lines, line)
	}
	return lines
}

func (v *displayViewer) handleResize() error {
	backingWidth, backingHeight := v.window.BackingSize()
	if backingWidth <= 0 || backingHeight <= 0 {
		// A minimized native window has no drawable client area. Keep the guest
		// at its usable resolution instead of briefly resizing it to 1x1.
		v.pendingResize = image.Point{}
		v.windowMinimized = true
		return nil
	}
	if v.windowMinimized {
		// Some OpenGL drivers discard the visible backing contents while a
		// window is minimized. Request a complete guest frame on restoration.
		v.windowMinimized = false
		v.generation = 0
	}
	size := v.guestDisplaySize(backingWidth, backingHeight)
	if size == v.lastResize {
		v.pendingResize = image.Point{}
		return nil
	}
	if size != v.pendingResize {
		v.pendingResize = size
		v.resizeChangedAt = time.Now()
		return nil
	}
	if time.Since(v.resizeChangedAt) < 120*time.Millisecond {
		return nil
	}
	if err := v.session.Resize(size.X, size.Y); err != nil {
		return fmt.Errorf("resize guest display to %dx%d: %w", size.X, size.Y, err)
	}
	v.lastResize = size
	v.pendingResize = image.Point{}
	return nil
}

func guestDisplaySize(backingWidth, backingHeight int, scale float32) image.Point {
	// The guest desktop does not inherit the host's DPI scale. Give it the
	// window's logical size so controls and text keep their intended physical
	// size on Retina and other HiDPI displays. The native UI still renders into
	// the full backing buffer, and pointer input is mapped between the two sizes.
	scale = normalizedDisplayScale(scale)
	width := max(1, int(math.Round(float64(float32(backingWidth)/scale))))
	if aligned := width &^ 7; aligned > 0 {
		width = aligned
	}
	height := max(1, int(math.Round(float64(float32(backingHeight)/scale))))
	return image.Pt(width, height)
}

func (v *displayViewer) guestDisplaySize(backingWidth, backingHeight int) image.Point {
	return guestDisplaySizeWithChrome(backingWidth, backingHeight, v.window.Scale(), v.chromeEnabled)
}

func guestDisplaySizeWithChrome(backingWidth, backingHeight int, scale float32, chrome bool) image.Point {
	size := guestDisplaySize(backingWidth, backingHeight, scale)
	if chrome {
		size.Y = max(1, size.Y-int(appChromeHeight))
	}
	return size
}

func (v *displayViewer) updateGuestCursor() error {
	if v.guestCursor == nil {
		return nil
	}
	provider, ok := v.session.(display.CursorProvider)
	if !v.desktopVisible || !ok {
		return v.guestCursor.Apply(display.CursorUpdate{}, false)
	}
	return v.guestCursor.Apply(provider.Cursor(), true)
}

func normalizedDisplayScale(scale float32) float32 {
	if scale < 1 {
		return 1
	}
	return min(4, scale)
}

func (v *displayViewer) handleInput() error {
	for _, event := range v.window.DrainInputEvents() {
		if v.handleChromeInput(event) {
			continue
		}
		if event.Type == window.InputEventMouseMove {
			v.updateUpdateNotificationHover(event.MouseX, event.MouseY, time.Now())
		}
		if event.Type == window.InputEventMouseDown &&
			v.handleUpdateNotificationClick(event.MouseX, event.MouseY, time.Now()) {
			continue
		}
		if v.handleUpdateNotificationKey(event, time.Now()) {
			continue
		}
		switch event.Type {
		case window.InputEventKeyDown, window.InputEventKeyUp, window.InputEventFlagsChanged:
			code, ok := linuxKeycode(event.Key)
			if ok {
				transitions := keyboardTransitions(event, v.keysDown[event.Key], v.window.GetKeyState(event.Key).IsDown())
				for _, down := range transitions {
					if err := v.session.Key(code, down); err != nil {
						return fmt.Errorf("send keyboard input: %w", err)
					}
					v.keysDown[event.Key] = down
				}
			}
		case window.InputEventMouseDown:
			v.buttons |= mouseButtonMask(event.Button)
			if err := v.sendPointer(event.MouseX, event.MouseY, v.buttons); err != nil {
				return err
			}
		case window.InputEventMouseUp:
			v.buttons &^= mouseButtonMask(event.Button)
			if err := v.sendPointer(event.MouseX, event.MouseY, v.buttons); err != nil {
				return err
			}
		case window.InputEventMouseMove:
			if err := v.sendPointer(event.MouseX, event.MouseY, v.buttons); err != nil {
				return err
			}
		case window.InputEventScroll:
			if event.PreciseScroll {
				deltaX120 := consumePreciseScrollDelta(event.RawScrollX, &v.scrollX120Remainder)
				deltaY120 := consumePreciseScrollDelta(event.RawScrollY, &v.scrollY120Remainder)
				if err := v.sendScroll(deltaX120, deltaY120); err != nil {
					return err
				}
				continue
			}
			deltaX120 := consumeScrollDelta(event.ScrollX, &v.scrollX120Remainder)
			deltaY120 := consumeScrollDelta(event.ScrollY, &v.scrollY120Remainder)
			if err := v.sendScroll(deltaX120, deltaY120); err != nil {
				return err
			}
		}
	}
	for key, down := range v.keysDown {
		if !down || v.window.GetKeyState(key).IsDown() {
			continue
		}
		code, ok := linuxKeycode(key)
		if ok {
			if err := v.session.Key(code, false); err != nil {
				return fmt.Errorf("release keyboard input: %w", err)
			}
		}
		v.keysDown[key] = false
	}
	return nil
}

func (v *displayViewer) handleChromeInput(event window.InputEvent) bool {
	if event.Type == window.InputEventMouseMove {
		v.chromeControlHover = chromeWindowControlNone
	}
	if !v.chromeEnabled {
		return false
	}
	scale := normalizedDisplayScale(v.window.Scale())
	backingWidth, _ := v.window.BackingSize()
	width := float32(backingWidth) / scale
	point := image.Pt(int(event.MouseX/scale), int(event.MouseY/scale))
	statusBounds := cvmfsChromeStatusBounds(width, v.chromeInsets)
	if event.Type == window.InputEventKeyDown && event.Key == window.KeyEscape && v.cvmfsExpanded {
		v.cvmfsExpanded = false
		return true
	}
	if event.Type != window.InputEventMouseDown && event.Type != window.InputEventMouseUp && event.Type != window.InputEventMouseMove {
		return false
	}
	if point.Y >= 0 && float32(point.Y) < appChromeHeight {
		for _, button := range chromeWindowControlButtons(width, platformWindowControlsAvailable()) {
			if !point.In(button.bounds) {
				continue
			}
			v.chromeControlHover = button.control
			if event.Type == window.InputEventMouseDown && event.Button == window.ButtonLeft {
				_ = activatePlatformWindowControl(button.control)
			}
			return true
		}
		if event.Type == window.InputEventMouseDown && event.Button == window.ButtonLeft {
			if point.In(statusBounds) {
				v.cvmfsExpanded = !v.cvmfsExpanded
			} else if v.isTitleBarDoubleClick(point, time.Now()) {
				toggleActiveWindowMaximized()
			} else if chrome, ok := v.window.(window.IntegratedTitleBarSupport); ok {
				chrome.BeginWindowDrag()
			}
		}
		return true
	}
	if v.cvmfsExpanded && point.In(cvmfsChromeDetailBounds(width, v.chromeInsets, len(v.cvmfsStatus.ActiveTransfers))) {
		return true
	}
	return false
}

func (v *displayViewer) isTitleBarDoubleClick(point image.Point, now time.Time) bool {
	const (
		maximumDelay    = 500 * time.Millisecond
		maximumDistance = 5
	)
	deltaX := point.X - v.lastChromeClick.X
	deltaY := point.Y - v.lastChromeClick.Y
	doubleClick := !v.lastChromeClickAt.IsZero() &&
		now.Sub(v.lastChromeClickAt) >= 0 && now.Sub(v.lastChromeClickAt) <= maximumDelay &&
		deltaX*deltaX+deltaY*deltaY <= maximumDistance*maximumDistance
	if doubleClick {
		v.lastChromeClickAt = time.Time{}
		return true
	}
	v.lastChromeClickAt = now
	v.lastChromeClick = point
	return false
}

func keyboardTransitions(event window.InputEvent, wasDown, platformDown bool) []bool {
	switch event.Type {
	case window.InputEventKeyDown:
		return []bool{true}
	case window.InputEventKeyUp:
		return []bool{false}
	case window.InputEventFlagsChanged:
		// Cocoa reports Caps Lock as one flagsChanged event per toggle rather
		// than a physical key-down/key-up pair. Linux toggles the lock on a key
		// press, so forward a complete press for every macOS toggle.
		if event.Key == window.KeyCapsLock {
			return []bool{true, false}
		}
		if down, ok := modifierTransitionDown(event.Key, event.Mods, wasDown); ok {
			return []bool{down}
		}
		return []bool{platformDown}
	default:
		return nil
	}
}

func (v *displayViewer) sendScroll(deltaX120, deltaY120 int32) error {
	if scroller, ok := v.session.(display.HighResolutionScroller); ok {
		if err := scroller.Scroll(deltaX120, deltaY120); err != nil {
			return fmt.Errorf("send scroll input: %w", err)
		}
		return nil
	}
	v.legacyScrollY120 += int64(deltaY120)
	steps := v.legacyScrollY120 / 120
	v.legacyScrollY120 -= steps * 120
	x, y := v.window.Cursor()
	for steps != 0 {
		mask := uint8(8)
		if steps < 0 {
			mask = 16
			steps++
		} else {
			steps--
		}
		if err := v.sendPointer(x, y, v.buttons|mask); err != nil {
			return err
		}
		if err := v.sendPointer(x, y, v.buttons); err != nil {
			return err
		}
	}
	return nil
}

// consumePreciseScrollDelta maps macOS logical-point deltas onto Linux's v120
// high-resolution wheel units. Forty logical points correspond to one
// traditional wheel detent, preserving movement down to 1/120th of a detent.
func consumePreciseScrollDelta(points float32, remainder *float64) int32 {
	const v120PerLogicalPoint = 3.0
	total := float64(points)*v120PerLogicalPoint + *remainder
	whole, fraction := math.Modf(total)
	*remainder = fraction
	if whole > math.MaxInt32 {
		*remainder = 0
		return math.MaxInt32
	}
	if whole < math.MinInt32 {
		*remainder = 0
		return math.MinInt32
	}
	return int32(whole)
}

func consumeScrollDelta(ticks float32, remainder *float64) int32 {
	value := float64(ticks)*120 + *remainder
	whole, fraction := math.Modf(value)
	*remainder = fraction
	if whole > math.MaxInt32 {
		*remainder = 0
		return math.MaxInt32
	}
	if whole < math.MinInt32 {
		*remainder = 0
		return math.MinInt32
	}
	return int32(whole)
}

func modifierTransitionDown(key window.Key, mods window.KeyMods, wasDown bool) (bool, bool) {
	var active bool
	switch key {
	case window.KeyLeftShift, window.KeyRightShift:
		active = mods&window.ModShift != 0
	case window.KeyLeftControl, window.KeyRightControl:
		active = mods&window.ModCtrl != 0
	case window.KeyLeftAlt, window.KeyRightAlt:
		active = mods&window.ModAlt != 0
	case window.KeyLeftSuper, window.KeyRightSuper:
		active = mods&window.ModSuper != 0
	default:
		return false, false
	}
	if !active {
		return false, true
	}
	// Aggregate platform flags cannot identify left from right. A flags-changed
	// event names the key that changed, so toggle that key when the modifier
	// remains active (for example, pressing or releasing one of two Ctrl keys).
	return !wasDown, true
}

func (v *displayViewer) sendPointer(x, y float32, buttons uint8) error {
	backingWidth, backingHeight := v.window.BackingSize()
	guestWidth, guestHeight := v.session.Size()
	if backingWidth <= 0 || backingHeight <= 0 || guestWidth <= 0 || guestHeight <= 0 {
		return nil
	}
	guestTop := float32(0)
	if v.chromeEnabled {
		guestTop = appChromeHeight * normalizedDisplayScale(v.window.Scale())
	}
	guestBackingHeight := max(float32(1), float32(backingHeight)-guestTop)
	guestX := uint32(min(guestWidth-1, max(0, int(x*float32(guestWidth)/float32(backingWidth)))))
	guestY := uint32(min(guestHeight-1, max(0, int((y-guestTop)*float32(guestHeight)/guestBackingHeight))))
	if err := v.session.Pointer(guestX, guestY, buttons, v.sentButtons); err != nil {
		return fmt.Errorf("send pointer input: %w", err)
	}
	v.sentButtons = buttons
	return nil
}

func (v *displayViewer) syncClipboard(clipboard window.Clipboard) error {
	hostText := clipboard.GetText()
	guestText, guestGeneration := v.session.GuestClipboard()
	decision := reconcileClipboard(
		v.hostClipboard,
		v.guestClipboardGen,
		hostText,
		guestText,
		guestGeneration,
	)
	v.hostClipboard = decision.text
	v.guestClipboardGen = decision.guestGeneration
	if decision.sendToGuest {
		v.session.SetClipboard(decision.text)
	}
	if decision.writeToHost {
		if err := clipboard.SetText(decision.text); err != nil {
			return fmt.Errorf("update host clipboard: %w", err)
		}
	}
	return nil
}

type clipboardDecision struct {
	text            string
	guestGeneration uint64
	sendToGuest     bool
	writeToHost     bool
}

func reconcileClipboard(cachedText string, cachedGuestGeneration uint64, hostText, guestText string, guestGeneration uint64) clipboardDecision {
	hostChanged := hostText != cachedText
	guestChanged := guestGeneration != 0 && guestGeneration != cachedGuestGeneration
	if hostChanged {
		// A pasteboard edit and a guest update can arrive in the same polling
		// interval. Prefer the host edit and acknowledge the observed guest
		// generation so stale guest text cannot immediately overwrite it.
		return clipboardDecision{
			text:            hostText,
			guestGeneration: guestGeneration,
			sendToGuest:     true,
		}
	}
	if guestChanged {
		return clipboardDecision{
			text:            guestText,
			guestGeneration: guestGeneration,
			writeToHost:     guestText != hostText,
		}
	}
	return clipboardDecision{text: cachedText, guestGeneration: cachedGuestGeneration}
}

func mouseButtonMask(button window.Button) uint8 {
	switch button {
	case window.ButtonLeft:
		return 1
	case window.ButtonMiddle:
		return 2
	case window.ButtonRight:
		return 4
	default:
		return 0
	}
}

func compileDisplayProgram(api gl.OpenGL) (uint32, error) {
	return compileGraphicsProgram(api, displayFragmentShader)
}

func compileGraphicsProgram(api gl.OpenGL, fragmentSource string) (uint32, error) {
	compile := func(kind uint32, source string) (uint32, error) {
		shader := api.CreateShader(kind)
		api.ShaderSource(shader, source)
		api.CompileShader(shader)
		var status int32
		api.GetShaderiv(shader, gl.CompileStatus, &status)
		if status == 0 {
			detail := api.GetShaderInfoLog(shader)
			api.DeleteShader(shader)
			return 0, fmt.Errorf("compile graphics shader: %s", detail)
		}
		return shader, nil
	}
	vertex, err := compile(gl.VertexShader, displayVertexShader)
	if err != nil {
		return 0, err
	}
	defer api.DeleteShader(vertex)
	fragment, err := compile(gl.FragmentShader, fragmentSource)
	if err != nil {
		return 0, err
	}
	defer api.DeleteShader(fragment)
	program := api.CreateProgram()
	api.AttachShader(program, vertex)
	api.AttachShader(program, fragment)
	api.LinkProgram(program)
	var status int32
	api.GetProgramiv(program, gl.LinkStatus, &status)
	if status == 0 {
		detail := api.GetProgramInfoLog(program)
		api.DeleteProgram(program)
		return 0, fmt.Errorf("link graphics program: %s", detail)
	}
	return program, nil
}

func linuxKeycode(key window.Key) (uint16, bool) {
	code, ok := gowinLinuxKeycodes[key]
	return code, ok
}

var gowinLinuxKeycodes = map[window.Key]uint16{
	window.KeyEscape: 1,
	window.Key1:      2, window.Key2: 3, window.Key3: 4, window.Key4: 5, window.Key5: 6,
	window.Key6: 7, window.Key7: 8, window.Key8: 9, window.Key9: 10, window.Key0: 11,
	window.KeyMinus: 12, window.KeyEqual: 13, window.KeyBackspace: 14, window.KeyTab: 15,
	window.KeyQ: 16, window.KeyW: 17, window.KeyE: 18, window.KeyR: 19, window.KeyT: 20,
	window.KeyY: 21, window.KeyU: 22, window.KeyI: 23, window.KeyO: 24, window.KeyP: 25,
	window.KeyLeftBracket: 26, window.KeyRightBracket: 27, window.KeyEnter: 28,
	window.KeyLeftControl: 29, window.KeyA: 30, window.KeyS: 31, window.KeyD: 32,
	window.KeyF: 33, window.KeyG: 34, window.KeyH: 35, window.KeyJ: 36, window.KeyK: 37,
	window.KeyL: 38, window.KeySemicolon: 39, window.KeyApostrophe: 40,
	window.KeyGraveAccent: 41, window.KeyLeftShift: 42, window.KeyBackslash: 43,
	window.KeyZ: 44, window.KeyX: 45, window.KeyC: 46, window.KeyV: 47, window.KeyB: 48,
	window.KeyN: 49, window.KeyM: 50, window.KeyComma: 51, window.KeyPeriod: 52,
	window.KeySlash: 53, window.KeyRightShift: 54, window.KeyNumpadMultiply: 55,
	window.KeyLeftAlt: 56, window.KeySpace: 57, window.KeyCapsLock: 58,
	window.KeyF1: 59, window.KeyF2: 60, window.KeyF3: 61, window.KeyF4: 62,
	window.KeyF5: 63, window.KeyF6: 64, window.KeyF7: 65, window.KeyF8: 66,
	window.KeyF9: 67, window.KeyF10: 68, window.KeyNumLock: 69, window.KeyScrollLock: 70,
	window.KeyNumpad7: 71, window.KeyNumpad8: 72, window.KeyNumpad9: 73,
	window.KeyNumpadSubtract: 74, window.KeyNumpad4: 75, window.KeyNumpad5: 76,
	window.KeyNumpad6: 77, window.KeyNumpadAdd: 78, window.KeyNumpad1: 79,
	window.KeyNumpad2: 80, window.KeyNumpad3: 81, window.KeyNumpad0: 82,
	window.KeyNumpadDecimal: 83, window.KeyF11: 87, window.KeyF12: 88,
	window.KeyNumpadEnter: 96, window.KeyRightControl: 97, window.KeyNumpadDivide: 98,
	window.KeyPrintScreen: 99, window.KeyRightAlt: 100, window.KeyHome: 102,
	window.KeyUp: 103, window.KeyPageUp: 104, window.KeyLeft: 105, window.KeyRight: 106,
	window.KeyEnd: 107, window.KeyDown: 108, window.KeyPageDown: 109, window.KeyInsert: 110,
	window.KeyDelete: 111, window.KeyPause: 119, window.KeyLeftSuper: 125,
	window.KeyRightSuper: 126, window.KeyNumpadEqual: 117,
}
