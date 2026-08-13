package desktopapp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"j5.nz/cc/display"
)

const (
	maximumAutomationCaptureFrames = 1000
	maximumAutomationRequestBytes  = 1 << 20
	automationSourceFramebuffer    = "framebuffer"
	automationSourcePresentation   = "presentation"
)

type desktopAutomationConfig struct {
	listen      string
	token       []byte
	captureRoot string
}

type desktopAutomation struct {
	token       []byte
	captureRoot string
	server      *http.Server

	mu             sync.Mutex
	session        display.Session
	pointerButtons uint8
	capture        *desktopCapture
}

type desktopCapture struct {
	status               automationCaptureStatus
	cancel               context.CancelFunc
	presentationEncoding bool
}

type automationCaptureStatus struct {
	State      string     `json:"state"`
	Source     string     `json:"source"`
	Directory  string     `json:"directory"`
	Requested  int        `json:"requested"`
	Captured   int        `json:"captured"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type automationStatus struct {
	Enabled       bool                     `json:"enabled"`
	SessionReady  bool                     `json:"session_ready"`
	DisplayWidth  int                      `json:"display_width,omitempty"`
	DisplayHeight int                      `json:"display_height,omitempty"`
	Capture       *automationCaptureStatus `json:"capture,omitempty"`
}

func loadDesktopAutomationConfig(listen, tokenFile, captureRoot string) (*desktopAutomationConfig, error) {
	listen = strings.TrimSpace(listen)
	tokenFile = strings.TrimSpace(tokenFile)
	captureRoot = strings.TrimSpace(captureRoot)
	if listen == "" && tokenFile == "" && captureRoot == "" {
		return nil, nil
	}
	if listen == "" || tokenFile == "" || captureRoot == "" {
		return nil, fmt.Errorf("--automation-listen, --automation-token-file, and --automation-capture-dir must be provided together")
	}
	host, portText, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("parse automation listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("automation listen address must use a literal loopback IP")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("automation listen port must be between 0 and 65535")
	}
	tokenInfo, err := os.Stat(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("inspect automation token file: %w", err)
	}
	if !tokenInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("automation token file must be a regular file")
	}
	if tokenInfo.Size() > 4096 {
		return nil, fmt.Errorf("automation token file is too large")
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read automation token file: %w", err)
	}
	token = []byte(strings.TrimSpace(string(token)))
	if len(token) < 32 {
		return nil, fmt.Errorf("automation token must contain at least 32 characters")
	}
	root, err := filepath.Abs(captureRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve automation capture directory: %w", err)
	}
	return &desktopAutomationConfig{listen: listen, token: token, captureRoot: root}, nil
}

func startDesktopAutomation(ctx context.Context, config *desktopAutomationConfig, logOutput io.Writer) (*desktopAutomation, error) {
	if config == nil {
		return nil, nil
	}
	if err := os.MkdirAll(config.captureRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create automation capture directory: %w", err)
	}
	root, err := filepath.EvalSymlinks(config.captureRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve automation capture directory: %w", err)
	}
	listener, err := net.Listen("tcp", config.listen)
	if err != nil {
		return nil, fmt.Errorf("listen for desktop automation: %w", err)
	}
	automation := &desktopAutomation{
		token:       append([]byte(nil), config.token...),
		captureRoot: root,
	}
	automation.server = &http.Server{
		Handler:           automation.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if logOutput != nil {
		fmt.Fprintf(logOutput, "%s automation API listening on http://%s\n", productName(), listener.Addr())
	}
	go func() {
		_ = automation.server.Serve(listener)
	}()
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = automation.close(shutdownContext)
	}()
	return automation, nil
}

func (a *desktopAutomation) close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	if a.capture != nil && a.capture.status.State == "capturing" {
		a.capture.cancel()
	}
	a.session = nil
	a.pointerButtons = 0
	a.mu.Unlock()
	if a.server == nil {
		return nil
	}
	err := a.server.Shutdown(ctx)
	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("stop desktop automation API: %w", err)
	}
	return nil
}

func (a *desktopAutomation) setSession(session display.Session) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if session == nil && a.capture != nil && a.capture.status.State == "capturing" {
		a.capture.cancel()
	}
	a.session = session
	a.pointerButtons = 0
}

func (a *desktopAutomation) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", a.handleStatus)
	mux.HandleFunc("POST /v1/captures", a.handleStartCapture)
	mux.HandleFunc("DELETE /v1/captures/current", a.handleCancelCapture)
	mux.HandleFunc("POST /v1/input/key", a.handleKey)
	mux.HandleFunc("POST /v1/input/pointer", a.handlePointer)
	mux.HandleFunc("POST /v1/input/scroll", a.handleScroll)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if !automationRemoteIsLoopback(request.RemoteAddr) {
			writeAutomationError(w, http.StatusForbidden, "loopback clients only")
			return
		}
		const prefix = "Bearer "
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authorization, prefix)), a.token) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeAutomationError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		mux.ServeHTTP(w, request)
	})
}

func automationRemoteIsLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *desktopAutomation) handleStatus(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	status := automationStatus{Enabled: true, SessionReady: a.session != nil}
	if a.session != nil {
		status.DisplayWidth, status.DisplayHeight = a.session.Size()
	}
	if a.capture != nil {
		capture := a.capture.status
		status.Capture = &capture
	}
	a.mu.Unlock()
	writeAutomationJSON(w, http.StatusOK, status)
}

func (a *desktopAutomation) handleStartCapture(w http.ResponseWriter, request *http.Request) {
	var input struct {
		Frames int    `json:"frames"`
		Subdir string `json:"subdir"`
		Source string `json:"source"`
	}
	if err := decodeAutomationJSON(w, request, &input); err != nil {
		writeAutomationError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, err := a.startCapture(input.Frames, input.Subdir, input.Source)
	if err != nil {
		writeAutomationError(w, http.StatusConflict, err.Error())
		return
	}
	writeAutomationJSON(w, http.StatusAccepted, status)
}

func (a *desktopAutomation) handleCancelCapture(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	if a.capture == nil || a.capture.status.State != "capturing" {
		a.mu.Unlock()
		writeAutomationError(w, http.StatusConflict, "no capture is active")
		return
	}
	a.capture.cancel()
	a.mu.Unlock()
	writeAutomationJSON(w, http.StatusAccepted, map[string]bool{"cancelling": true})
}

func (a *desktopAutomation) startCapture(frames int, subdir, source string) (automationCaptureStatus, error) {
	if frames < 1 || frames > maximumAutomationCaptureFrames {
		return automationCaptureStatus{}, fmt.Errorf("frames must be between 1 and %d", maximumAutomationCaptureFrames)
	}
	subdir = strings.TrimSpace(subdir)
	if subdir == "" || subdir == "." || subdir == ".." || !filepath.IsLocal(subdir) ||
		filepath.Base(subdir) != subdir || strings.ContainsAny(subdir, `/\\`) {
		return automationCaptureStatus{}, fmt.Errorf("subdir must be one directory name")
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = automationSourceFramebuffer
	}
	if source != automationSourceFramebuffer && source != automationSourcePresentation {
		return automationCaptureStatus{}, fmt.Errorf("source must be %q or %q", automationSourceFramebuffer, automationSourcePresentation)
	}
	directory := filepath.Join(a.captureRoot, subdir)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		return automationCaptureStatus{}, fmt.Errorf("desktop session is not ready")
	}
	if a.capture != nil && a.capture.status.State == "capturing" {
		return automationCaptureStatus{}, fmt.Errorf("a capture is already active")
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return automationCaptureStatus{}, fmt.Errorf("create capture subdirectory: %w", err)
	}
	captureContext, cancel := context.WithCancel(context.Background())
	status := automationCaptureStatus{
		State: "capturing", Source: source, Directory: directory, Requested: frames, StartedAt: time.Now().UTC(),
	}
	a.capture = &desktopCapture{status: status, cancel: cancel}
	session := a.session
	job := a.capture
	if source == automationSourceFramebuffer {
		go a.captureFrames(captureContext, session, job)
	} else {
		go func() {
			<-captureContext.Done()
			a.finishCapture(job, "cancelled", nil)
		}()
	}
	return status, nil
}

func (a *desktopAutomation) beginPresentationFrame() (*desktopCapture, bool) {
	if a == nil {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.capture == nil || a.capture.status.State != "capturing" ||
		a.capture.status.Source != automationSourcePresentation || a.capture.presentationEncoding {
		return nil, false
	}
	a.capture.presentationEncoding = true
	return a.capture, true
}

func (a *desktopAutomation) submitPresentationFrame(job *desktopCapture, width, height int, generation uint64, pixels []byte) {
	go func() {
		a.mu.Lock()
		if a.capture != job || job.status.State != "capturing" {
			job.presentationEncoding = false
			a.mu.Unlock()
			return
		}
		frameNumber := job.status.Captured + 1
		directory := job.status.Directory
		a.mu.Unlock()

		name := fmt.Sprintf("frame-%06d-generation-%020d.png", frameNumber, generation)
		err := writeAutomationPresentationFrame(filepath.Join(directory, name), width, height, pixels)

		a.mu.Lock()
		job.presentationEncoding = false
		if a.capture != job || job.status.State != "capturing" {
			a.mu.Unlock()
			return
		}
		if err != nil {
			a.mu.Unlock()
			a.finishCapture(job, "failed", err)
			return
		}
		job.status.Captured = frameNumber
		complete := job.status.Captured == job.status.Requested
		a.mu.Unlock()
		if complete {
			a.finishCapture(job, "complete", nil)
		}
	}()
}

func (a *desktopAutomation) captureFrames(ctx context.Context, session display.Session, job *desktopCapture) {
	width, height := session.Size()
	initial := session.Snapshot(image.Rect(0, 0, width, height), 0, false)
	if err := validAutomationFrame(initial); err != nil {
		a.finishCapture(job, "failed", err)
		return
	}
	generation := initial.Generation
	for {
		select {
		case <-ctx.Done():
			a.finishCapture(job, "cancelled", nil)
			return
		case <-session.Changed():
		}
		select {
		case <-ctx.Done():
			a.finishCapture(job, "cancelled", nil)
			return
		default:
		}
		width, height = session.Size()
		frame := session.Snapshot(image.Rect(0, 0, width, height), generation, false)
		if frame.Generation <= generation {
			continue
		}
		generation = frame.Generation
		if err := validAutomationFrame(frame); err != nil {
			a.finishCapture(job, "failed", err)
			return
		}
		a.mu.Lock()
		frameNumber := job.status.Captured + 1
		directory := job.status.Directory
		a.mu.Unlock()
		name := fmt.Sprintf("frame-%06d-generation-%020d.png", frameNumber, frame.Generation)
		if err := writeAutomationFrame(filepath.Join(directory, name), frame); err != nil {
			a.finishCapture(job, "failed", err)
			return
		}
		a.mu.Lock()
		job.status.Captured = frameNumber
		complete := job.status.Captured == job.status.Requested
		a.mu.Unlock()
		if complete {
			a.finishCapture(job, "complete", nil)
			return
		}
	}
}

func validAutomationFrame(frame display.FramebufferUpdate) error {
	if frame.Width <= 0 || frame.Height <= 0 || frame.Rect != image.Rect(0, 0, frame.Width, frame.Height) {
		return fmt.Errorf("desktop framebuffer snapshot is unavailable")
	}
	maximumInt := int(^uint(0) >> 1)
	if frame.Width > maximumInt/4 || frame.Height > maximumInt/(frame.Width*4) {
		return fmt.Errorf("desktop framebuffer snapshot has an invalid size")
	}
	required := frame.Width * frame.Height * 4
	if len(frame.Pixels) != required {
		return fmt.Errorf("desktop framebuffer snapshot has an invalid size")
	}
	return nil
}

func writeAutomationFrame(path string, frame display.FramebufferUpdate) (retErr error) {
	imageFrame := image.NewNRGBA(image.Rect(0, 0, frame.Width, frame.Height))
	for source, destination := 0, 0; source < len(frame.Pixels); source, destination = source+4, destination+4 {
		imageFrame.Pix[destination+0] = frame.Pixels[source+2]
		imageFrame.Pix[destination+1] = frame.Pixels[source+1]
		imageFrame.Pix[destination+2] = frame.Pixels[source+0]
		imageFrame.Pix[destination+3] = 0xff
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create captured frame: %w", err)
	}
	defer func() {
		if err := file.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close captured frame: %w", err)
		}
	}()
	if err := png.Encode(file, imageFrame); err != nil {
		return fmt.Errorf("encode captured frame: %w", err)
	}
	return nil
}

func writeAutomationPresentationFrame(path string, width, height int, pixels []byte) error {
	maximumInt := int(^uint(0) >> 1)
	if width <= 0 || height <= 0 || width > maximumInt/4 || height > maximumInt/(width*4) ||
		len(pixels) != width*height*4 {
		return fmt.Errorf("presented frame has an invalid size")
	}
	imageFrame := image.NewNRGBA(image.Rect(0, 0, width, height))
	rowBytes := width * 4
	for y := 0; y < height; y++ {
		sourceStart := (height - 1 - y) * rowBytes
		destinationStart := y * imageFrame.Stride
		copy(imageFrame.Pix[destinationStart:destinationStart+rowBytes], pixels[sourceStart:sourceStart+rowBytes])
		for x := 3; x < rowBytes; x += 4 {
			imageFrame.Pix[destinationStart+x] = 0xff
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create captured presentation frame: %w", err)
	}
	if err := png.Encode(file, imageFrame); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode captured presentation frame: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close captured presentation frame: %w", err)
	}
	return nil
}

func (a *desktopAutomation) finishCapture(job *desktopCapture, state string, captureErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.capture != job || job.status.State != "capturing" {
		return
	}
	job.status.State = state
	finished := time.Now().UTC()
	job.status.FinishedAt = &finished
	if captureErr != nil {
		job.status.Error = captureErr.Error()
	}
	job.cancel()
}

func (a *desktopAutomation) handleKey(w http.ResponseWriter, request *http.Request) {
	var input struct {
		Code uint16 `json:"code"`
		Down *bool  `json:"down"`
	}
	if err := decodeAutomationJSON(w, request, &input); err != nil {
		writeAutomationError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.Code == 0 || input.Down == nil {
		writeAutomationError(w, http.StatusBadRequest, "code and down are required")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		writeAutomationError(w, http.StatusConflict, "desktop session is not ready")
		return
	}
	if err := a.session.Key(input.Code, *input.Down); err != nil {
		writeAutomationError(w, http.StatusInternalServerError, fmt.Sprintf("send key input: %v", err))
		return
	}
	writeAutomationJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func (a *desktopAutomation) handlePointer(w http.ResponseWriter, request *http.Request) {
	var input struct {
		X       *uint32 `json:"x"`
		Y       *uint32 `json:"y"`
		Buttons *uint8  `json:"buttons"`
	}
	if err := decodeAutomationJSON(w, request, &input); err != nil {
		writeAutomationError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.X == nil || input.Y == nil || input.Buttons == nil {
		writeAutomationError(w, http.StatusBadRequest, "x, y, and buttons are required")
		return
	}
	if *input.Buttons&^0x1f != 0 {
		writeAutomationError(w, http.StatusBadRequest, "buttons contains an unsupported bit")
		return
	}
	if err := a.pointer(*input.X, *input.Y, *input.Buttons); err != nil {
		writeAutomationError(w, http.StatusConflict, err.Error())
		return
	}
	writeAutomationJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func (a *desktopAutomation) pointer(x, y uint32, buttons uint8) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		return fmt.Errorf("desktop session is not ready")
	}
	width, height := a.session.Size()
	if width <= 0 || height <= 0 || x >= uint32(width) || y >= uint32(height) {
		return fmt.Errorf("pointer coordinates are outside the guest display")
	}
	if err := a.session.Pointer(x, y, buttons, a.pointerButtons); err != nil {
		return fmt.Errorf("send pointer input: %w", err)
	}
	a.pointerButtons = buttons
	return nil
}

func (a *desktopAutomation) handleScroll(w http.ResponseWriter, request *http.Request) {
	var input struct {
		DeltaX120 int32 `json:"delta_x_120"`
		DeltaY120 int32 `json:"delta_y_120"`
	}
	if err := decodeAutomationJSON(w, request, &input); err != nil {
		writeAutomationError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.DeltaX120 == 0 && input.DeltaY120 == 0 {
		writeAutomationError(w, http.StatusBadRequest, "a non-zero scroll delta is required")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		writeAutomationError(w, http.StatusConflict, "desktop session is not ready")
		return
	}
	scroller, ok := a.session.(display.HighResolutionScroller)
	if !ok {
		writeAutomationError(w, http.StatusNotImplemented, "high-resolution scrolling is unavailable")
		return
	}
	if err := scroller.Scroll(input.DeltaX120, input.DeltaY120); err != nil {
		writeAutomationError(w, http.StatusInternalServerError, fmt.Sprintf("send scroll input: %v", err))
		return
	}
	writeAutomationJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func decodeAutomationJSON(w http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(w, request.Body, maximumAutomationRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}

func writeAutomationError(w http.ResponseWriter, status int, message string) {
	writeAutomationJSON(w, status, map[string]string{"error": message})
}

func writeAutomationJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
