package main

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
	"j5.nz/cc/display"
)

const (
	glBGRA = 0x80e1

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
	backgroundFragmentShader = `#version 150
uniform vec4 backgroundGeometry;
in vec2 uv;
out vec4 color;
void main() {
	vec2 size = backgroundGeometry.xy;
	vec2 point = uv * size;
	vec2 glowCenter = vec2(size.x * 0.92, -size.y * 0.08);
	float glowRadius = min(size.x, size.y) * 0.56;
	float glow = (1.0 - smoothstep(0.0, glowRadius, length(point - glowCenter))) * 0.22;
	vec3 base = vec3(23.0, 14.0, 31.0) / 255.0;
	vec3 purple = vec3(101.0, 29.0, 241.0) / 255.0;
	color = vec4(mix(base, purple, glow), 1.0);
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
	firstRunComplete    bool
	showSettings        bool
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
}

type displayStartResult struct {
	started displayStarted
	err     error
}

type displayPreflightResult struct {
	preflight startupPreflight
	err       error
}

func openDisplayWindow(
	ctx context.Context,
	title string,
	width, height int,
	settings startupOptions,
	firstRunComplete bool,
	preflight displayPreflight,
	start displayStart,
) error {
	win, err := window.New(title, width, height, true)
	if err != nil {
		return fmt.Errorf("open graphics window: %w", err)
	}
	viewer := &displayViewer{
		window:             win,
		keysDown:           make(map[window.Key]bool),
		startup:            initialStartupProgress(),
		startupEvents:      make(chan startupProgress, 16),
		startDone:          make(chan displayStartResult, 1),
		cancelDone:         make(chan struct{}, 1),
		preflightDone:      make(chan displayPreflightResult, 1),
		imageRestartReady:  make(chan struct{}, 1),
		settings:           settings,
		firstRunComplete:   firstRunComplete,
		showSettings:       !firstRunComplete,
		updateConsumedKeys: make(map[window.Key]bool),
		parentContext:      ctx,
		start:              start,
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

func (v *displayViewer) beginPreflight(ctx context.Context, preflight displayPreflight) {
	go func() {
		result, err := preflight(ctx)
		v.preflightDone <- displayPreflightResult{preflight: result, err: err}
	}()
}

func (v *displayViewer) beginStart(refreshImage bool) {
	if v.starting || !v.preflightReady || !v.preflight.canStart() {
		return
	}
	v.settings.RefreshImage = refreshImage
	backingWidth, backingHeight := v.window.BackingSize()
	displaySize := guestDisplaySize(backingWidth, backingHeight, v.window.Scale())
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
	v.backgroundProgram, err = compileGraphicsProgram(v.gl, backgroundFragmentShader)
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
				v.preflightReady = true
				if !v.firstRunComplete || !v.preflight.canStart() {
					v.showSettings = true
				} else {
					v.beginStart(false)
				}
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
		backingWidth, backingHeight := v.window.BackingSize()
		v.gl.Viewport(0, 0, int32(backingWidth), int32(backingHeight))
		if v.desktopVisible && v.textureWidth > 0 && v.textureHeight > 0 {
			// Display snapshots are XRGB/BGRX. Their fourth byte is padding,
			// not opacity, so blending it would turn valid X11 surfaces
			// transparent wherever that byte happens to be zero.
			v.gl.Disable(gl.Blend)
			v.gl.ClearColor(0, 0, 0, 1)
			v.gl.Clear(gl.ColorBufferBit)
			v.gl.UseProgram(v.program)
			v.gl.ActiveTexture(gl.Texture0)
			v.gl.BindTexture(gl.Texture2D, v.texture)
			v.gl.BindVertexArray(v.vertexArray)
			v.gl.DrawArrays(gl.Triangles, 0, 6)
			v.drawUpdateNotifications(backingWidth, backingHeight, time.Now())
		} else if v.showSettings {
			v.drawSettings(backingWidth, backingHeight)
		} else {
			v.drawStartup(backingWidth, backingHeight, time.Now())
		}
		v.window.Swap()
		time.Sleep(time.Second / 120)
	}
	return v.startErr
}

func (v *displayViewer) handleStartupInput(events []window.InputEvent) {
	scale := normalizedDisplayScale(v.window.Scale())
	for _, event := range events {
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
						Title:  "Stopping SquadVM",
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
		switch event.Type {
		case window.InputEventKeyDown:
			if event.Repeat {
				continue
			}
			switch event.Key {
			case window.KeyTab:
				v.startupFocus = nextStartupControl(
					v.startupFocus,
					event.Mods&window.ModShift != 0,
					v.preflight.hasUpdate(),
				)
				v.startupFocusVisible = true
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
		case window.InputEventMouseMove, window.InputEventMouseDown:
			backingWidth, backingHeight := v.window.BackingSize()
			layout := settingsControlLayout(float32(backingWidth)/scale, float32(backingHeight)/scale)
			point := image.Pt(int(event.MouseX/scale), int(event.MouseY/scale))
			control := startupControlAt(point, layout, v.preflight.hasUpdate())
			v.startupHover = control
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
			case startupControlSkip:
				v.startupFocus = startupControlSkip
				v.beginSettingsStart(false)
			case startupControlPrimary:
				v.startupFocus = startupControlPrimary
				v.beginSettingsStart(v.preflight.hasUpdate())
			}
		}
	}
}

func (v *displayViewer) activateStartupControl(control startupControl) {
	switch control {
	case startupControlSSH:
		v.settings.SSHEnabled = !v.settings.SSHEnabled
	case startupControlSystem:
		v.settings.SystemInstall = !v.settings.SystemInstall
	case startupControlSkip:
		if v.preflight.hasUpdate() {
			v.beginSettingsStart(false)
		}
	case startupControlPrimary:
		v.beginSettingsStart(v.preflight.hasUpdate())
	}
}

func (v *displayViewer) beginSettingsStart(refreshImage bool) {
	if v.preflight.hasUpdate() {
		v.imageDismissed = true
	}
	v.beginStart(refreshImage)
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

	layout := settingsControlLayout(width, height)
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
	stateColor := color.RGBA{R: 199, G: 184, B: 210, A: 255}
	stateBackground := color.RGBA{R: 51, G: 39, B: 61, A: 255}
	titleColor := color.RGBA{R: 243, G: 245, B: 239, A: 255}
	if v.preflightErr != nil {
		title = "Checks failed"
		detail = "See the complete error below."
		state = "ACTION NEEDED"
		stateColor = color.RGBA{R: 255, G: 151, B: 151, A: 255}
		stateBackground = color.RGBA{R: 75, G: 36, B: 47, A: 255}
		titleColor = color.RGBA{R: 255, G: 137, B: 137, A: 255}
	} else if v.preflightReady {
		title = "Ready to start"
		detail = "Your system and desktop image are ready."
		state = "READY"
		stateColor = color.RGBA{R: 143, G: 226, B: 156, A: 255}
		stateBackground = color.RGBA{R: 31, G: 62, B: 45, A: 255}
		if !v.preflight.Image.Installed {
			title = "Set up SquadVM"
			detail = "Download the desktop image, then start automatically."
			state = "DOWNLOAD NEEDED"
			stateColor = color.RGBA{R: 255, G: 193, B: 111, A: 255}
			stateBackground = color.RGBA{R: 72, G: 52, B: 31, A: 255}
		} else if v.preflight.hasUpdate() && v.preflight.ReleaseUpdate != nil {
			title = "Updates available"
			detail = "Update the desktop image and virtual machine manager."
			state = "UPDATES"
			stateColor = color.RGBA{R: 221, G: 202, B: 255, A: 255}
			stateBackground = color.RGBA{R: 66, G: 39, B: 101, A: 255}
		} else if v.preflight.hasUpdate() || v.preflight.ReleaseUpdate != nil {
			title = "Update available"
			detail = "A newer SquadVM component is ready to install."
			state = "UPDATE"
			stateColor = color.RGBA{R: 221, G: 202, B: 255, A: 255}
			stateBackground = color.RGBA{R: 66, G: 39, B: 101, A: 255}
		}
		if !v.preflight.canStart() {
			title = "Can't start yet"
			detail = "Review the failed requirement before starting SquadVM."
			state = "ACTION NEEDED"
			stateColor = color.RGBA{R: 255, G: 193, B: 111, A: 255}
			stateBackground = color.RGBA{R: 72, G: 52, B: 31, A: 255}
			titleColor = color.RGBA{R: 255, G: 193, B: 111, A: 255}
		}
	}

	stateBounds := layout.state
	v.drawRoundedRect(backingWidth, backingHeight, scale, stateBounds, stateBounds.Dy()/2, stateBackground)
	v.text.BeginDraw()
	v.drawTextBold("SquadVM", left+60, contentTop+21, 19, color.RGBA{R: 243, G: 245, B: 239, A: 255})
	v.drawText("UQ Cyber Squad", left+60, contentTop+43, 14, color.RGBA{R: 189, G: 151, B: 255, A: 255})
	v.drawCenteredTextBold(state, stateBounds, 13, stateColor)
	v.drawTextBold(fitStartupText(title, panelWidth, 34), left, contentTop+99, 34, titleColor)
	for index, line := range wrapStartupText(detail, panelWidth, 18, 2) {
		v.drawText(line, left, contentTop+129+float32(index*21), 18, color.RGBA{R: 224, G: 216, B: 230, A: 255})
	}
	v.text.EndDraw()
	if failure := v.settingsFailureDetail(); failure != "" {
		v.drawSettingsFailure(backingWidth, backingHeight, scale, left, contentTop+158, panelWidth, failure)
		return
	}

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

	v.drawSettingsOption(
		backingWidth, backingHeight, scale, layout.sshCheckbox,
		"SSH access", "Adds \"ssh squadvm\" to SSH config",
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

	button := layout.button
	buttonColor := color.RGBA{R: 69, G: 54, B: 80, A: 255}
	buttonText := color.RGBA{R: 153, G: 142, B: 163, A: 255}
	if v.preflightReady && v.preflight.canStart() && !v.starting {
		buttonColor = color.RGBA{R: 95, G: 23, B: 238, A: 255}
		buttonText = color.RGBA{R: 255, G: 255, B: 255, A: 255}
		if v.startupHover == startupControlPrimary {
			buttonColor = color.RGBA{R: 117, G: 49, B: 255, A: 255}
		}
	}
	divider := layout.actionDivider
	v.drawRect(backingWidth, backingHeight, scale,
		float32(divider.Min.X), float32(divider.Min.Y), float32(divider.Dx()), float32(divider.Dy()),
		color.RGBA{R: 57, G: 43, B: 67, A: 255})
	v.drawRoundedRect(backingWidth, backingHeight, scale, button, 9, buttonColor)
	label := "Start SquadVM"
	if v.preflightReady && !v.preflight.Image.Installed {
		label = "Download & start"
	}
	if v.preflightReady && v.preflight.hasUpdate() {
		label = "Update & start"
		skip := layout.skip
		skipColor := color.RGBA{R: 54, G: 41, B: 65, A: 255}
		skipText := color.RGBA{R: 235, G: 229, B: 240, A: 255}
		if v.starting {
			skipText = color.RGBA{R: 153, G: 142, B: 163, A: 255}
		} else if v.startupHover == startupControlSkip {
			skipColor = color.RGBA{R: 68, G: 52, B: 80, A: 255}
		}
		v.drawPanel(backingWidth, backingHeight, scale, skip, 9,
			color.RGBA{R: 76, G: 59, B: 89, A: 255}, skipColor)
		if v.startupFocusVisible && v.startupFocus == startupControlSkip {
			v.drawOutline(backingWidth, backingHeight, scale, skip, color.RGBA{R: 250, G: 238, B: 52, A: 255})
		}
		v.text.BeginDraw()
		v.drawCenteredTextBold("Skip", skip, 15, skipText)
		v.text.EndDraw()
	}
	if v.starting {
		label = "Starting…"
	}
	if v.startupFocusVisible && v.startupFocus == startupControlPrimary {
		v.drawOutline(backingWidth, backingHeight, scale, button, color.RGBA{R: 250, G: 238, B: 52, A: 255})
	}
	v.text.BeginDraw()
	v.drawCenteredTextBold(label, button, 15, buttonText)
	shortcutWidth := float32(layout.skip.Min.X) - left - 12
	if !v.preflight.hasUpdate() {
		shortcutWidth = float32(button.Min.X) - left - 12
	}
	v.drawText(fitStartupText("Space  SSH   ·   Tab  move   ·   Enter  start", shortcutWidth, 15),
		left, float32(button.Min.Y+33), 15, color.RGBA{R: 211, G: 202, B: 218, A: 255})
	v.text.EndDraw()
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
		color.RGBA{R: 92, G: 43, B: 56, A: 255}, color.RGBA{R: 39, G: 24, B: 32, A: 255})
	v.text.BeginDraw()
	v.drawTextBold("STARTUP CHECK DETAILS", left+14, top+24, 13, color.RGBA{R: 255, G: 151, B: 151, A: 255})
	for index, line := range lines[v.startupDetailScroll:min(len(lines), v.startupDetailScroll+visibleLines)] {
		v.drawText(line, left+14, top+49+float32(index)*lineHeight, 15, color.RGBA{R: 243, G: 229, B: 234, A: 255})
	}
	footer := "Close this window after reviewing the error"
	if maxScroll > 0 {
		footer = fmt.Sprintf("Scroll or use arrow keys for the full error  ·  lines %d–%d of %d",
			v.startupDetailScroll+1, min(len(lines), v.startupDetailScroll+visibleLines), len(lines))
	}
	v.drawText(footer, left, top+260, 15, color.RGBA{R: 211, G: 190, B: 199, A: 255})
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
		color.RGBA{R: 55, G: 41, B: 64, A: 255}, color.RGBA{R: 33, G: 22, B: 41, A: 255})
	statusColor := color.RGBA{R: 157, G: 144, B: 168, A: 255}
	badgeColor := color.RGBA{R: 58, G: 45, B: 69, A: 255}
	switch status {
	case "PASS":
		statusColor = color.RGBA{R: 143, G: 226, B: 156, A: 255}
		badgeColor = color.RGBA{R: 35, G: 69, B: 52, A: 255}
	case "FAIL":
		statusColor = color.RGBA{R: 255, G: 137, B: 137, A: 255}
		badgeColor = color.RGBA{R: 81, G: 42, B: 54, A: 255}
	case "UPDATE":
		statusColor = color.RGBA{R: 221, G: 202, B: 255, A: 255}
		badgeColor = color.RGBA{R: 75, G: 45, B: 116, A: 255}
	}
	badge := image.Rect(int(left+12), int(top+14), int(left+46), int(top+48))
	v.drawRoundedRect(backingWidth, backingHeight, scale, badge, 9, badgeColor)
	if status == "PASS" {
		v.drawCheckmark(backingWidth, backingHeight, scale, badge, statusColor)
	}
	v.text.BeginDraw()
	v.drawCenteredTextBold(statusSymbol(status), badge, 18, statusColor)
	v.drawTextBold(label, left+58, top+25, 16, color.RGBA{R: 243, G: 245, B: 239, A: 255})
	v.drawText(fitStartupText(detail, width-70, 15), left+58, top+49, 15, color.RGBA{R: 211, G: 202, B: 218, A: 255})
	v.text.EndDraw()
}

func (v *displayViewer) drawSettingsOption(
	backingWidth, backingHeight int,
	scale float32,
	bounds image.Rectangle,
	title, detail string,
	enabled, focused, hovered bool,
) {
	borderColor := color.RGBA{R: 68, G: 52, B: 79, A: 255}
	fillColor := color.RGBA{R: 38, G: 27, B: 47, A: 255}
	if hovered {
		borderColor = color.RGBA{R: 101, G: 74, B: 121, A: 255}
		fillColor = color.RGBA{R: 47, G: 33, B: 58, A: 255}
	}
	v.drawPanel(backingWidth, backingHeight, scale, bounds, 10,
		borderColor, fillColor)
	toggleX := float32(bounds.Min.X + 12)
	toggleY := float32(bounds.Min.Y + 19)
	toggleColor := color.RGBA{R: 75, G: 59, B: 87, A: 255}
	knobX := toggleX + 3
	if enabled {
		toggleColor = color.RGBA{R: 95, G: 23, B: 238, A: 255}
		knobX = toggleX + 19
	}
	v.drawRoundedRect(backingWidth, backingHeight, scale, image.Rect(int(toggleX), int(toggleY), int(toggleX+38), int(toggleY+22)), 11, toggleColor)
	v.drawRoundedRect(backingWidth, backingHeight, scale, image.Rect(int(knobX), int(toggleY+3), int(knobX+16), int(toggleY+19)), 8, color.RGBA{R: 250, G: 249, B: 251, A: 255})
	if focused {
		v.drawOutline(backingWidth, backingHeight, scale, bounds, color.RGBA{R: 250, G: 238, B: 52, A: 255})
	}
	v.text.BeginDraw()
	textX := float32(bounds.Min.X + 62)
	textWidth := float32(bounds.Dx() - 74)
	v.drawTextBold(title, textX, float32(bounds.Min.Y+24), 16, color.RGBA{R: 243, G: 245, B: 239, A: 255})
	v.drawText(fitStartupText(detail, textWidth, 15), textX, float32(bounds.Min.Y+47), 15, color.RGBA{R: 211, G: 202, B: 218, A: 255})
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

func (v *displayViewer) drawStartup(backingWidth, backingHeight int, now time.Time) {
	v.gl.Enable(gl.Blend)
	scale := normalizedDisplayScale(v.window.Scale())
	width := float32(backingWidth) / scale
	height := float32(backingHeight) / scale
	v.gl.UseProgram(v.program)
	v.drawBackground(backingWidth, backingHeight)

	layout := calculateStartupScreenLayout(width, height)
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
	stateColor := color.RGBA{R: 221, G: 202, B: 255, A: 255}
	stateBackground := color.RGBA{R: 66, G: 39, B: 101, A: 255}
	if v.startup.Failed {
		state = "INTERRUPTED"
		stateColor = color.RGBA{R: 255, G: 151, B: 151, A: 255}
		stateBackground = color.RGBA{R: 75, G: 36, B: 47, A: 255}
	}
	stateBounds := layout.state
	v.drawRoundedRect(backingWidth, backingHeight, scale, stateBounds, stateBounds.Dy()/2, stateBackground)
	v.text.BeginDraw()
	v.drawTextBold("SquadVM", left+60, contentTop+21, 19, color.RGBA{R: 243, G: 245, B: 239, A: 255})
	v.drawText(startupEyebrow(v.startup), left+60, contentTop+43, 14, color.RGBA{R: 189, G: 151, B: 255, A: 255})
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
	v.drawStartupChecklist(backingWidth, backingHeight, scale, left, contentTop+92, panelWidth, now)

	accent := color.RGBA{R: 95, G: 23, B: 238, A: 255}
	drawBar := func(y, height, fraction float32, determinate bool, barColor color.RGBA) {
		v.drawRect(backingWidth, backingHeight, scale, left, y, panelWidth, height, color.RGBA{R: 50, G: 35, B: 63, A: 255})
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
		v.drawTextBold("DOWNLOAD", left, contentTop+169, 15, color.RGBA{R: 189, G: 151, B: 255, A: 255})
		downloadWidth := float32(v.text.GetAdvance(v.font, 15, downloadText))
		v.drawText(downloadText, left+panelWidth-downloadWidth, contentTop+169, 15, color.RGBA{R: 211, G: 202, B: 218, A: 255})
		v.drawTextBold("INDEX", left, contentTop+207, 15, color.RGBA{R: 250, G: 238, B: 52, A: 255})
		indexWidth := float32(v.text.GetAdvance(v.font, 15, indexText))
		v.drawText(indexText, left+panelWidth-indexWidth, contentTop+207, 15, color.RGBA{R: 211, G: 202, B: 218, A: 255})
		v.text.EndDraw()
		drawBar(contentTop+176, 9, float32(v.startup.DownloadProgress), true, accent)
		drawBar(contentTop+214, 9, float32(v.startup.IndexProgress), true, color.RGBA{R: 250, G: 238, B: 52, A: 255})
	} else {
		if v.startup.Failed {
			accent = color.RGBA{R: 224, G: 90, B: 90, A: 255}
		}
		drawBar(float32(layout.bar.Min.Y), float32(layout.bar.Dy()), float32(v.startup.Progress), v.startup.Determinate || v.startup.Failed, accent)
		transfer := formatStartupTransfer(v.startup)
		eta := formatStartupETA(v.startup.ETA)
		v.text.BeginDraw()
		v.drawText(transfer, left, contentTop+220, 15, color.RGBA{R: 211, G: 202, B: 218, A: 255})
		if eta != "" {
			etaWidth := float32(v.text.GetAdvance(v.font, 15, eta))
			v.drawText(eta, left+panelWidth-etaWidth, contentTop+220, 15, color.RGBA{R: 211, G: 202, B: 218, A: 255})
		}
		v.text.EndDraw()
	}

	labels := []string{"IMAGE", "START VM", "DESKTOP"}
	for index, label := range labels {
		bounds := layout.steps[index]
		x := float32(bounds.Min.X)
		y := float32(bounds.Min.Y)
		stepColor := color.RGBA{R: 57, G: 43, B: 68, A: 255}
		textColor := color.RGBA{R: 157, G: 144, B: 168, A: 255}
		if startupPhase(index) < v.startup.Phase {
			stepColor = color.RGBA{R: 95, G: 23, B: 238, A: 255}
			textColor = color.RGBA{R: 189, G: 151, B: 255, A: 255}
		} else if startupPhase(index) == v.startup.Phase {
			stepColor = color.RGBA{R: 250, G: 238, B: 52, A: 255}
			textColor = color.RGBA{R: 243, G: 245, B: 239, A: 255}
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
	v.drawText(footer, left, contentTop+338, 15, color.RGBA{R: 190, G: 177, B: 201, A: 255})
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
		color.RGBA{R: 92, G: 43, B: 56, A: 255}, color.RGBA{R: 39, G: 24, B: 32, A: 255})

	v.text.BeginDraw()
	v.drawTextBold(v.startup.Title, left+14, top+25, 16, color.RGBA{R: 255, G: 151, B: 151, A: 255})
	for index, line := range lines[v.startupDetailScroll:min(len(lines), v.startupDetailScroll+visibleLines)] {
		v.drawText(line, left+14, top+49+float32(index)*lineHeight, 15, color.RGBA{R: 243, G: 229, B: 234, A: 255})
	}
	footer := "Esc for settings  ·  Close this window to stop"
	if maxScroll > 0 {
		footer = fmt.Sprintf("Scroll or use arrow keys for the full error  ·  lines %d–%d of %d",
			v.startupDetailScroll+1, min(len(lines), v.startupDetailScroll+visibleLines), len(lines))
	}
	v.drawText(footer, left, top+256, 15, color.RGBA{R: 211, G: 190, B: 199, A: 255})
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
		color.RGBA{R: 79, G: 59, B: 91, A: 255}, color.RGBA{R: 14, G: 11, B: 17, A: 255})
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
	v.drawTextBold("BOOT CONSOLE", textLeft, top+19, 12, color.RGBA{R: 189, G: 151, B: 255, A: 255})
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
		color.RGBA{R: 190, G: 177, B: 201, A: 255})
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
	fg := startupTerminalColor(attr.FG, color.RGBA{R: 218, G: 211, B: 222, A: 255}, attr.Bold)
	bg := startupTerminalColor(attr.BG, color.RGBA{}, false)
	if attr.Inverse {
		originalFG := fg
		if bg.A == 0 {
			fg = color.RGBA{R: 14, G: 11, B: 17, A: 255}
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
			v.drawRoundedRect(backingWidth, backingHeight, scale, icon, 9, color.RGBA{R: 95, G: 23, B: 238, A: 255})
			v.drawCheckmark(backingWidth, backingHeight, scale, icon, color.RGBA{R: 243, G: 237, B: 255, A: 255})
			continue
		}
		dotColor := color.RGBA{R: 250, G: 238, B: 52, A: 255}
		if v.startupChecklist[index].Failed {
			dotColor = color.RGBA{R: 255, G: 137, B: 137, A: 255}
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
			textColor := color.RGBA{R: 243, G: 245, B: 239, A: 255}
			if item.Failed {
				textColor = color.RGBA{R: 255, G: 151, B: 151, A: 255}
			}
			v.drawTextBold(line, left+30, y+18, 16, textColor)
		} else {
			v.drawText(line, left+30, y+18, 16, color.RGBA{R: 177, G: 163, B: 187, A: 255})
		}
	}
	v.text.EndDraw()
	v.gl.Disable(gl.ScissorTest)
}

func (v *displayViewer) loadBrandTexture() error {
	decoded, err := png.Decode(bytes.NewReader(squadvmBrandPNG))
	if err != nil {
		return fmt.Errorf("decode embedded SquadVM artwork: %w", err)
	}
	bounds := decoded.Bounds()
	rgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	imagedraw.Draw(rgba, rgba.Bounds(), decoded, bounds.Min, imagedraw.Src)
	if len(rgba.Pix) == 0 {
		return fmt.Errorf("embedded SquadVM artwork is empty")
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

func startupEyebrow(progress startupProgress) string {
	if progress.Failed {
		return "STARTUP INTERRUPTED"
	}
	switch progress.Phase {
	case startupImage:
		return "SQUADVM IMAGE"
	case startupBoot:
		return "STARTING VIRTUAL MACHINE"
	default:
		return "STARTING SQUADVM"
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
	size := guestDisplaySize(backingWidth, backingHeight, v.window.Scale())
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
	scale = normalizedDisplayScale(scale)
	width := max(1, int(math.Round(float64(float32(backingWidth)/scale))))
	if aligned := width &^ 7; aligned > 0 {
		width = aligned
	}
	return image.Pt(width, max(1, int(math.Round(float64(float32(backingHeight)/scale)))))
}

func normalizedDisplayScale(scale float32) float32 {
	if scale < 1 {
		return 1
	}
	return min(4, scale)
}

func (v *displayViewer) handleInput() error {
	for _, event := range v.window.DrainInputEvents() {
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
				down := event.Type == window.InputEventKeyDown
				if event.Type == window.InputEventFlagsChanged {
					if modifierDown, ok := modifierTransitionDown(event.Key, event.Mods, v.keysDown[event.Key]); ok {
						down = modifierDown
					} else {
						down = v.window.GetKeyState(event.Key).IsDown()
					}
				}
				if err := v.session.Key(code, down); err != nil {
					return fmt.Errorf("send keyboard input: %w", err)
				}
				v.keysDown[event.Key] = down
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
	guestX := uint32(min(guestWidth-1, max(0, int(x*float32(guestWidth)/float32(backingWidth)))))
	guestY := uint32(min(guestHeight-1, max(0, int(y*float32(guestHeight)/float32(backingHeight)))))
	if err := v.session.Pointer(guestX, guestY, buttons, v.sentButtons); err != nil {
		return fmt.Errorf("send pointer input: %w", err)
	}
	v.sentButtons = buttons
	return nil
}

func (v *displayViewer) syncClipboard(clipboard window.Clipboard) error {
	if text := clipboard.GetText(); text != v.hostClipboard {
		v.hostClipboard = text
		v.session.SetClipboard(text)
	}
	text, generation := v.session.GuestClipboard()
	if generation != 0 && generation != v.guestClipboardGen {
		v.guestClipboardGen = generation
		v.hostClipboard = text
		if err := clipboard.SetText(text); err != nil {
			return fmt.Errorf("update host clipboard: %w", err)
		}
	}
	return nil
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
