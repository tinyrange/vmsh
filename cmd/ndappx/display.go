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
	"time"
	"unsafe"

	"github.com/tinyrange/gowin/gl"
	gowintext "github.com/tinyrange/gowin/text"
	"github.com/tinyrange/gowin/window"
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
)

type displayViewer struct {
	session           display.Session
	window            window.Window
	gl                gl.OpenGL
	text              *gowintext.Stash
	font              int
	program           uint32
	vertexArray       uint32
	vertexBuffer      uint32
	texture           uint32
	wordmarkTexture   uint32
	wordmarkWidth     int
	wordmarkHeight    int
	textureWidth      int
	textureHeight     int
	generation        uint64
	buttons           uint8
	sentButtons       uint8
	keysDown          map[window.Key]bool
	guestClipboardGen uint64
	hostClipboard     string
	lastResize        image.Point
	pendingResize     image.Point
	resizeChangedAt   time.Time
	startup           startupProgress
	startupEvents     chan startupProgress
	startDone         chan displayStartResult
	presentation      desktopPresentationGate
	desktopVisible    bool
	startErr          error
}

type displayStartResult struct {
	session display.Session
	err     error
}

func openDisplayWindow(ctx context.Context, title string, width, height int, start displayStart) error {
	win, err := window.New(title, width, height, true)
	if err != nil {
		return fmt.Errorf("open graphics window: %w", err)
	}
	viewer := &displayViewer{
		window:        win,
		keysDown:      make(map[window.Key]bool),
		startup:       initialStartupProgress(),
		startupEvents: make(chan startupProgress, 16),
		startDone:     make(chan displayStartResult, 1),
	}
	if err := viewer.init(); err != nil {
		win.Close()
		return err
	}
	defer viewer.close()
	viewer.beginStart(ctx, start)
	return viewer.loop(ctx)
}

func (v *displayViewer) beginStart(ctx context.Context, start displayStart) {
	publish := func(progress startupProgress) {
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
		session, err := start(ctx, publish)
		v.startDone <- displayStartResult{session: session, err: err}
	}()
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
	if err := v.loadWordmarkTexture(); err != nil {
		return err
	}
	v.text = gowintext.New(v.gl, 1024, 1024)
	v.text.SetYInverted(true)
	v.text.SetGraphicsShader(v.program)
	v.font, err = v.text.AddFontFromMemory(gowintext.EMBEDDED_FONT)
	if err != nil {
		return fmt.Errorf("load progress font: %w", err)
	}
	return nil
}

func (v *displayViewer) close() {
	if v.wordmarkTexture != 0 {
		v.gl.DeleteTextures(1, &v.wordmarkTexture)
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
	v.window.Close()
}

func (v *displayViewer) loop(ctx context.Context) error {
	clipboard := window.GetClipboard()
	v.hostClipboard = clipboard.GetText()
	nextClipboardCheck := time.Now()
	for v.window.Poll() {
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
				v.startup = progress
				if progress.Failed {
					v.desktopVisible = false
					v.startErr = fmt.Errorf("%s", progress.Detail)
				}
			case result := <-v.startDone:
				if result.err != nil {
					if ctx.Err() != nil {
						return nil
					}
					v.startErr = result.err
					v.startup = failedStartupProgress(result.err)
				} else if result.session == nil {
					v.startup = failedStartupProgress(fmt.Errorf("VM started without a native display session"))
				} else {
					v.session = result.session
					v.session.SetClipboard(v.hostClipboard)
					v.presentation.markGuestReady()
					v.startup = desktopStartupProgress("Waiting for a complete desktop frame")
				}
			default:
				goto eventsDrained
			}
		}
	eventsDrained:

		if v.session == nil || !v.desktopVisible {
			// Do not deliver keys or pointer events to a guest surface the user
			// cannot see. Polling still drains host events while the progress
			// view owns the window.
			_ = v.window.DrainInputEvents()
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
		} else {
			v.drawStartup(backingWidth, backingHeight, time.Now())
		}
		v.window.Swap()
		time.Sleep(time.Second / 120)
	}
	return v.startErr
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

func (v *displayViewer) drawStartup(backingWidth, backingHeight int, now time.Time) {
	v.gl.Enable(gl.Blend)
	scale := v.window.Scale()
	if scale <= 0 {
		scale = 1
	}
	width := float32(backingWidth) / scale
	height := float32(backingHeight) / scale
	v.gl.UseProgram(v.program)
	v.drawRect(backingWidth, backingHeight, scale, 0, 0, width, height, color.RGBA{R: 17, G: 23, B: 14, A: 255})

	panelWidth := max(float32(1), min(float32(760), width-64))
	left := (width - panelWidth) / 2
	contentTop := max(float32(40), (height-405)/2)

	wordmarkWidth := min(float32(328), panelWidth)
	wordmarkHeight := wordmarkWidth * float32(v.wordmarkHeight) / float32(v.wordmarkWidth)
	v.drawTexture(v.wordmarkTexture, backingWidth, backingHeight, scale, left, contentTop, wordmarkWidth, wordmarkHeight)

	v.text.SetViewport(int32(width), int32(height))
	v.text.SetScale(scale)

	v.text.BeginDraw()
	titleColor := color.RGBA{R: 243, G: 245, B: 239, A: 255}
	if v.startup.Failed {
		titleColor = color.RGBA{R: 255, G: 137, B: 137, A: 255}
	}
	v.drawTextBold(startupEyebrow(v.startup), left, contentTop+96, 14, color.RGBA{R: 143, G: 200, B: 102, A: 255})
	v.drawTextBold(fitStartupText(v.startup.Title, panelWidth, 34), left, contentTop+137, 34, titleColor)
	v.drawText(fitStartupText(v.startup.Detail, panelWidth, 16), left, contentTop+176, 16, color.RGBA{R: 203, G: 211, B: 197, A: 255})
	v.text.EndDraw()

	barY := contentTop + 214
	v.drawRect(backingWidth, backingHeight, scale, left, barY, panelWidth, 10, color.RGBA{R: 44, G: 56, B: 40, A: 255})
	accent := color.RGBA{R: 107, G: 164, B: 66, A: 255}
	if v.startup.Failed {
		accent = color.RGBA{R: 224, G: 90, B: 90, A: 255}
		v.drawRect(backingWidth, backingHeight, scale, left, barY, panelWidth, 10, accent)
	} else if v.startup.Determinate {
		fraction := float32(min(1, max(0, v.startup.Progress)))
		v.drawRect(backingWidth, backingHeight, scale, left, barY, panelWidth*fraction, 10, accent)
	} else {
		sweepWidth := panelWidth * 0.26
		distance := panelWidth + sweepWidth
		offset := float32(math.Mod(float64(now.UnixNano())/float64(1400*time.Millisecond), 1))*distance - sweepWidth
		visibleLeft := max(float32(0), offset)
		visibleRight := min(panelWidth, offset+sweepWidth)
		if visibleRight > visibleLeft {
			v.drawRect(backingWidth, backingHeight, scale, left+visibleLeft, barY, visibleRight-visibleLeft, 10, accent)
		}
	}

	transfer := formatStartupTransfer(v.startup)
	eta := formatStartupETA(v.startup.ETA)
	v.text.BeginDraw()
	v.drawText(transfer, left, contentTop+258, 14, color.RGBA{R: 180, G: 192, B: 173, A: 255})
	if eta != "" {
		etaWidth := float32(v.text.GetAdvance(v.font, 14, eta))
		v.drawText(eta, left+panelWidth-etaWidth, contentTop+258, 14, color.RGBA{R: 180, G: 192, B: 173, A: 255})
	}
	v.text.EndDraw()

	stepY := contentTop + 304
	stepWidth := max(float32(1), (panelWidth-24)/4)
	labels := []string{"IMAGE", "PREPARE", "START VM", "DESKTOP"}
	for index, label := range labels {
		x := left + float32(index)*(stepWidth+8)
		stepColor := color.RGBA{R: 52, G: 64, B: 47, A: 255}
		textColor := color.RGBA{R: 150, G: 161, B: 143, A: 255}
		if startupPhase(index) < v.startup.Phase {
			stepColor = color.RGBA{R: 107, G: 164, B: 66, A: 255}
			textColor = color.RGBA{R: 143, G: 200, B: 102, A: 255}
		} else if startupPhase(index) == v.startup.Phase {
			stepColor = color.RGBA{R: 104, G: 168, B: 204, A: 255}
			textColor = color.RGBA{R: 243, G: 245, B: 239, A: 255}
		}
		v.drawRect(backingWidth, backingHeight, scale, x, stepY, stepWidth, 3, stepColor)
		v.text.BeginDraw()
		v.drawTextBold(label, x, stepY+32, 14, textColor)
		v.text.EndDraw()
	}

	v.text.BeginDraw()
	footer := "Close this window to cancel"
	if v.startup.Failed {
		footer = "Close this window to stop"
	}
	v.drawText(footer, left, min(height-30, contentTop+389), 14, color.RGBA{R: 156, G: 168, B: 148, A: 255})
	v.text.EndDraw()
}

func (v *displayViewer) loadWordmarkTexture() error {
	decoded, err := png.Decode(bytes.NewReader(neurodeskWordmarkPNG))
	if err != nil {
		return fmt.Errorf("decode embedded Neurodesk wordmark: %w", err)
	}
	bounds := decoded.Bounds()
	rgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	imagedraw.Draw(rgba, rgba.Bounds(), decoded, bounds.Min, imagedraw.Src)
	if len(rgba.Pix) == 0 {
		return fmt.Errorf("embedded Neurodesk wordmark is empty")
	}

	v.wordmarkWidth, v.wordmarkHeight = bounds.Dx(), bounds.Dy()
	v.gl.GenTextures(1, &v.wordmarkTexture)
	v.gl.ActiveTexture(gl.Texture0)
	v.gl.BindTexture(gl.Texture2D, v.wordmarkTexture)
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
	case startupPull:
		return "NEURODESKTOP IMAGE"
	case startupPrepare:
		return "PREPARING NEURODESKTOP"
	case startupBoot:
		return "STARTING VIRTUAL MACHINE"
	default:
		return "STARTING NEURODESKTOP"
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

func (v *displayViewer) drawText(value string, x, y, size float32, col color.RGBA) {
	if strings.TrimSpace(value) == "" {
		return
	}
	rgba := [4]float32{
		float32(col.R) / 255,
		float32(col.G) / 255,
		float32(col.B) / 255,
		float32(col.A) / 255,
	}
	v.text.DrawText(v.font, float64(size), float64(x), float64(y), value, rgba)
}

func (v *displayViewer) drawTextBold(value string, x, y, size float32, col color.RGBA) {
	// Gowin embeds a variable Roboto Mono face but its current Fontstash API
	// does not expose the weight axis. Overlapping the same glyph at a
	// sub-pixel horizontal offset gives headings a stable, portable bold
	// weight while body copy remains the untouched regular face.
	v.drawText(value, x, y, size, col)
	v.drawText(value, x+0.7, y, size, col)
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
	return image.Pt(
		max(1, int(math.Round(float64(float32(backingWidth)/scale)))),
		max(1, int(math.Round(float64(float32(backingHeight)/scale)))),
	)
}

func normalizedDisplayScale(scale float32) float32 {
	if scale < 1 {
		return 1
	}
	return min(4, scale)
}

func (v *displayViewer) handleInput() error {
	for _, event := range v.window.DrainInputEvents() {
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
			if event.ScrollY == 0 {
				continue
			}
			mask := uint8(8)
			if event.ScrollY < 0 {
				mask = 16
			}
			x, y := v.window.Cursor()
			if err := v.sendPointer(x, y, v.buttons|mask); err != nil {
				return err
			}
			if err := v.sendPointer(x, y, v.buttons); err != nil {
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
	fragment, err := compile(gl.FragmentShader, displayFragmentShader)
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
