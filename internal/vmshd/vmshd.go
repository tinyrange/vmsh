package vmshd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/vmsh/internal/backend"
	"golang.org/x/net/websocket"
	"j5.nz/cc/ccvmd"
	"j5.nz/cc/client"
)

const Kind = "vmshd"

const maxJobLogBytes = 64 * 1024

type Status struct {
	Kind      string                 `json:"kind"`
	Status    string                 `json:"status"`
	Sessions  []SessionSummary       `json:"sessions"`
	Streams   []StreamSummary        `json:"active_streams"`
	VMs       []client.InstanceState `json:"vms"`
	StartedAt time.Time              `json:"started_at"`
}

type Event struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Session    *Session          `json:"session,omitempty"`
	Attachment *ClientAttachment `json:"attachment,omitempty"`
	At         time.Time         `json:"at"`
}

type StreamSummary struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	State        string    `json:"state"`
	SessionID    string    `json:"session_id,omitempty"`
	AttachmentID string    `json:"attachment_id,omitempty"`
	ConnectedAt  time.Time `json:"connected_at"`
	LastEventID  string    `json:"last_event_id,omitempty"`
}

type Session struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	State           string             `json:"state"`
	HostCWD         string             `json:"host_cwd,omitempty"`
	SelectedContext *SessionContext    `json:"selected_context,omitempty"`
	HostShells      []ShellHandle      `json:"host_shells,omitempty"`
	GuestShells     []ShellHandle      `json:"guest_shells,omitempty"`
	SSHShells       []ShellHandle      `json:"ssh_shells,omitempty"`
	Jobs            []JobSummary       `json:"jobs,omitempty"`
	Attachments     []ClientAttachment `json:"attached_clients"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

type SessionSummary struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	State           string             `json:"state"`
	HostCWD         string             `json:"host_cwd,omitempty"`
	SelectedContext *SessionContext    `json:"selected_context,omitempty"`
	HostShells      []ShellHandle      `json:"host_shells,omitempty"`
	GuestShells     []ShellHandle      `json:"guest_shells,omitempty"`
	SSHShells       []ShellHandle      `json:"ssh_shells,omitempty"`
	Jobs            []JobSummary       `json:"jobs,omitempty"`
	AttachedClients []ClientAttachment `json:"attached_clients"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

type SessionContext struct {
	Mode     string `json:"mode"`
	Name     string `json:"name,omitempty"`
	Short    string `json:"short,omitempty"`
	Source   string `json:"source,omitempty"`
	VMID     string `json:"vm,omitempty"`
	Image    string `json:"image,omitempty"`
	SSHHost  string `json:"ssh_host,omitempty"`
	CWD      string `json:"cwd,omitempty"`
	User     string `json:"user,omitempty"`
	Isolated bool   `json:"isolated,omitempty"`
}

type JobSummary struct {
	ID         int       `json:"id"`
	SessionID  string    `json:"session_id,omitempty"`
	Context    string    `json:"context,omitempty"`
	Command    string    `json:"command"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	Control    string    `json:"control,omitempty"`
	Logs       string    `json:"logs,omitempty"`
	LogDropped bool      `json:"log_dropped,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type StartHostJobRequest struct {
	Command []string `json:"command"`
	WorkDir string   `json:"workdir,omitempty"`
	Env     []string `json:"env,omitempty"`
	Context string   `json:"context,omitempty"`
}

type ShellHandle struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Context string `json:"context,omitempty"`
	CWD     string `json:"cwd,omitempty"`
	VMID    string `json:"vm,omitempty"`
	SSHHost string `json:"ssh_host,omitempty"`
	User    string `json:"user,omitempty"`
	State   string `json:"state"`
}

type ClientAttachment struct {
	ID         string    `json:"id"`
	Mode       string    `json:"mode"`
	Terminal   *Terminal `json:"terminal,omitempty"`
	AttachedAt time.Time `json:"attached_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Terminal struct {
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
}

type TerminalStreamMessage struct {
	Kind     string         `json:"kind"`
	Data     []byte         `json:"data,omitempty"`
	Terminal *Terminal      `json:"terminal,omitempty"`
	Stream   *StreamSummary `json:"stream,omitempty"`
}

type CreateSessionRequest struct {
	Name string `json:"name,omitempty"`
}

type UpdateSessionRequest struct {
	HostCWD         string          `json:"host_cwd,omitempty"`
	SelectedContext *SessionContext `json:"selected_context,omitempty"`
	HostShells      []ShellHandle   `json:"host_shells,omitempty"`
	GuestShells     []ShellHandle   `json:"guest_shells,omitempty"`
	SSHShells       []ShellHandle   `json:"ssh_shells,omitempty"`
	Jobs            []JobSummary    `json:"jobs,omitempty"`
}

type AttachSessionRequest struct {
	Mode     string    `json:"mode,omitempty"`
	Terminal *Terminal `json:"terminal,omitempty"`
}

type AttachSessionResponse struct {
	Session    Session          `json:"session"`
	Attachment ClientAttachment `json:"attachment"`
}

type DetachSessionRequest struct {
	AttachmentID string `json:"attachment_id"`
}

type sessionRegistry struct {
	mu         sync.Mutex
	next       int
	nextAttach int
	nextJob    int
	sessions   map[string]Session
	jobs       map[string]map[int]JobSummary
}

type eventHub struct {
	mu          sync.Mutex
	next        int
	subscribers map[int]*eventSubscriber
}

type eventSubscriber struct {
	id          int
	ch          chan Event
	connectedAt time.Time
	lastEventID string
}

type streamRegistry struct {
	mu      sync.Mutex
	next    int
	streams map[string]StreamSummary
}

type Server struct {
	token    string
	registry *sessionRegistry
	events   *eventHub
	streams  *streamRegistry
	jobs     *hostJobRunner

	startedAt time.Time
}

type hostJobRunner struct {
	mu      sync.Mutex
	cancels map[int]context.CancelFunc
}

func Main(args []string) {
	started, err := Run(args)
	if err == nil {
		return
	}
	if !started {
		_ = json.NewEncoder(os.Stdout).Encode(client.ServerHello{
			Kind:   "error",
			Error:  "vmshd failed to start",
			Detail: err.Error(),
		})
	}
	fmt.Fprintf(os.Stderr, "vmshd startup failed: %v\n", err)
	os.Exit(1)
}

func Run(args []string) (bool, error) {
	cacheDir, err := resolveCacheDir(scanCacheDir(args))
	if err != nil {
		return false, err
	}
	tokenPath := filepath.Join(cacheDir, "vmshd.token")
	token, err := writeTokenFile(tokenPath)
	if err != nil {
		return false, err
	}

	srv := NewServer(token)
	return ccvmd.RunServer(args, ccvmd.ServerOptions{
		Kind:      Kind,
		TokenPath: tokenPath,
		RegisterHandlers: func(mux *http.ServeMux, runtime ccvmd.RuntimeView) {
			srv.RegisterHandlers(mux, runtime)
		},
		WrapHandler: srv.Authenticate,
	})
}

func NewServer(token string) *Server {
	return &Server{
		token:     token,
		registry:  newSessionRegistry(),
		events:    newEventHub(),
		streams:   newStreamRegistry(),
		jobs:      newHostJobRunner(),
		startedAt: time.Now(),
	}
}

func (s *Server) RegisterHandlers(mux *http.ServeMux, runtime ccvmd.RuntimeView) {
	mux.HandleFunc("GET /vmsh/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Status{
			Kind:      Kind,
			Status:    "ok",
			Sessions:  s.registry.List(),
			Streams:   append(s.events.Streams(), s.streams.List()...),
			VMs:       runtimeInstanceStatuses(runtime),
			StartedAt: s.startedAt,
		})
	})
	mux.HandleFunc("GET /vmsh/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.registry.List())
	})
	mux.HandleFunc("GET /vmsh/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.registry.Jobs())
	})
	mux.HandleFunc("GET /vmsh/events", func(w http.ResponseWriter, r *http.Request) {
		s.streamEvents(w, r)
	})
	mux.HandleFunc("POST /vmsh/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req CreateSessionRequest
		if err := decodeOptionalJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, client.ErrorResponse{Error: err.Error()})
			return
		}
		session := s.registry.Create(req.Name)
		s.events.Publish(Event{Kind: "session_created", Session: &session})
		writeJSON(w, http.StatusOK, session)
	})
	mux.HandleFunc("GET /vmsh/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		session, ok := s.registry.Get(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, client.ErrorResponse{Error: "session not found"})
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
	mux.HandleFunc("PATCH /vmsh/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req UpdateSessionRequest
		if err := decodeOptionalJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, client.ErrorResponse{Error: err.Error()})
			return
		}
		session, err := s.registry.Update(r.PathValue("id"), req)
		if err != nil {
			writeSessionError(w, err)
			return
		}
		s.events.Publish(Event{Kind: "session_updated", Session: &session})
		writeJSON(w, http.StatusOK, session)
	})
	mux.HandleFunc("DELETE /vmsh/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		session, jobIDs, ok := s.registry.Delete(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, client.ErrorResponse{Error: "session not found"})
			return
		}
		s.jobs.Cancel(jobIDs)
		s.events.Publish(Event{Kind: "session_deleted", Session: &session})
		writeJSON(w, http.StatusOK, session)
	})
	mux.HandleFunc("POST /vmsh/sessions/{id}/attach", func(w http.ResponseWriter, r *http.Request) {
		var req AttachSessionRequest
		if err := decodeOptionalJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, client.ErrorResponse{Error: err.Error()})
			return
		}
		session, attachment, err := s.registry.Attach(r.PathValue("id"), req)
		if err != nil {
			writeSessionError(w, err)
			return
		}
		s.events.Publish(Event{Kind: "session_attached", Session: &session, Attachment: &attachment})
		writeJSON(w, http.StatusOK, AttachSessionResponse{Session: session, Attachment: attachment})
	})
	mux.HandleFunc("POST /vmsh/sessions/{id}/detach", func(w http.ResponseWriter, r *http.Request) {
		var req DetachSessionRequest
		if err := decodeOptionalJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, client.ErrorResponse{Error: err.Error()})
			return
		}
		session, err := s.registry.Detach(r.PathValue("id"), req.AttachmentID)
		if err != nil {
			writeSessionError(w, err)
			return
		}
		s.events.Publish(Event{Kind: "session_detached", Session: &session})
		writeJSON(w, http.StatusOK, session)
	})
	mux.HandleFunc("POST /vmsh/sessions/{id}/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req StartHostJobRequest
		if err := decodeRequiredJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, client.ErrorResponse{Error: err.Error()})
			return
		}
		job, err := s.startHostJob(r.PathValue("id"), req)
		if err != nil {
			writeSessionError(w, err)
			return
		}
		session, _ := s.registry.Get(r.PathValue("id"))
		s.events.Publish(Event{Kind: "job_started", Session: &session})
		writeJSON(w, http.StatusOK, job)
	})
	mux.HandleFunc("POST /vmsh/sessions/{id}/attachments/{attachment}/terminal", func(w http.ResponseWriter, r *http.Request) {
		var req Terminal
		if err := decodeRequiredJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, client.ErrorResponse{Error: err.Error()})
			return
		}
		session, attachment, err := s.registry.UpdateTerminal(r.PathValue("id"), r.PathValue("attachment"), req)
		if err != nil {
			writeSessionError(w, err)
			return
		}
		s.events.Publish(Event{Kind: "terminal_updated", Session: &session, Attachment: &attachment})
		writeJSON(w, http.StatusOK, AttachSessionResponse{Session: session, Attachment: attachment})
	})
	mux.Handle("GET /vmsh/sessions/{id}/attachments/{attachment}/stream", websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			s.serveTerminalStream(ws)
		},
	})
}

func (s *Server) startHostJob(sessionID string, req StartHostJobRequest) (JobSummary, error) {
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return JobSummary{}, sessionError{status: http.StatusBadRequest, err: "command is required"}
	}
	sessionID = strings.TrimSpace(sessionID)
	job, err := s.registry.StartJob(sessionID, req)
	if err != nil {
		return JobSummary{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.jobs.Track(job.ID, cancel)
	go func() {
		defer s.jobs.Forget(job.ID)
		cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
		cmd.Dir = strings.TrimSpace(req.WorkDir)
		cmd.Env = append(os.Environ(), req.Env...)
		output, runErr := cmd.CombinedOutput()
		_, ok := s.registry.FinishJob(sessionID, job.ID, summarizeHostJobResult(output, runErr))
		if !ok {
			return
		}
		session, _ := s.registry.Get(sessionID)
		s.events.Publish(Event{Kind: "job_finished", Session: &session})
	}()
	return job, nil
}

func newHostJobRunner() *hostJobRunner {
	return &hostJobRunner{cancels: map[int]context.CancelFunc{}}
}

func (r *hostJobRunner) Track(id int, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[id] = cancel
}

func (r *hostJobRunner) Forget(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, id)
}

func (r *hostJobRunner) Cancel(ids []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		if cancel := r.cancels[id]; cancel != nil {
			cancel()
			delete(r.cancels, id)
		}
	}
}

func summarizeHostJobResult(output []byte, err error) JobSummary {
	job := JobSummary{
		Status:     "exited",
		Logs:       boundedJobLogs(output),
		LogDropped: len(output) > maxJobLogBytes,
		FinishedAt: time.Now(),
	}
	if err == nil {
		return job
	}
	job.Status = "failed"
	job.Error = err.Error()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		job.ExitCode = exitErr.ExitCode()
	}
	return job
}

func boundedJobLogs(output []byte) string {
	if len(output) <= maxJobLogBytes {
		return string(output)
	}
	return string(output[len(output)-maxJobLogBytes:])
}

func (s *Server) serveTerminalStream(ws *websocket.Conn) {
	sessionID := ws.Request().PathValue("id")
	attachmentID := ws.Request().PathValue("attachment")
	session, attachment, ok := s.registry.GetAttachment(sessionID, attachmentID)
	if !ok {
		_ = websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "error"})
		return
	}
	stream, closeStream := s.streams.Open("terminal", session.ID, attachment.ID)
	defer func() {
		closeStream()
		s.events.Publish(Event{Kind: "terminal_stream_closed", Session: &session, Attachment: &attachment})
	}()
	s.events.Publish(Event{Kind: "terminal_stream_opened", Session: &session, Attachment: &attachment})
	if err := websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "attached", Stream: &stream}); err != nil {
		return
	}
	for {
		var msg TerminalStreamMessage
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			return
		}
		switch strings.TrimSpace(msg.Kind) {
		case "resize":
			if msg.Terminal == nil {
				_ = websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "error"})
				continue
			}
			updated, updatedAttachment, err := s.registry.UpdateTerminal(session.ID, attachment.ID, *msg.Terminal)
			if err != nil {
				_ = websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "error"})
				continue
			}
			session = updated
			attachment = updatedAttachment
			s.events.Publish(Event{Kind: "terminal_updated", Session: &session, Attachment: &attachment})
		case "stdin", "data":
			// Terminal bytes are intentionally opaque to vmshd at this layer.
		case "close":
			return
		default:
			_ = websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "error"})
		}
	}
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, client.ErrorResponse{Error: "event streaming is not supported"})
		return
	}
	events, unsubscribe := s.events.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	connected := s.events.Event(Event{Kind: "connected"})
	s.events.MarkDelivered(events, connected.ID)
	if err := json.NewEncoder(w).Encode(connected); err != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-events:
			if err := json.NewEncoder(w).Encode(event); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func runtimeInstanceStatuses(runtime ccvmd.RuntimeView) []client.InstanceState {
	if runtime == nil {
		return []client.InstanceState{}
	}
	statuses := runtime.InstanceStatuses()
	if statuses == nil {
		return []client.InstanceState{}
	}
	return statuses
}

func (s *Server) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validBearerToken(r.Header.Get("Authorization"), s.token) {
			writeJSON(w, http.StatusUnauthorized, client.ErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validBearerToken(header, want string) bool {
	header = strings.TrimSpace(header)
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func scanCacheDir(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-cache-dir" || arg == "--cache-dir" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		for _, prefix := range []string{"-cache-dir=", "--cache-dir="} {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
	}
	return ""
}

func resolveCacheDir(arg string) (string, error) {
	if strings.TrimSpace(arg) != "" {
		if err := os.MkdirAll(arg, 0o700); err != nil {
			return "", err
		}
		return arg, nil
	}
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	dir := filepath.Join(userCacheDir, "vmshd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func writeTokenFile(path string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	return token, nil
}

func newToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: map[string]Session{}, jobs: map[string]map[int]JobSummary{}}
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: map[int]*eventSubscriber{}}
}

func newStreamRegistry() *streamRegistry {
	return &streamRegistry{streams: map[string]StreamSummary{}}
}

func (r *streamRegistry) Open(kind, sessionID, attachmentID string) (StreamSummary, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	stream := StreamSummary{
		ID:           fmt.Sprintf("%s_stream_%08x", strings.TrimSpace(kind), r.next),
		Kind:         strings.TrimSpace(kind),
		State:        "open",
		SessionID:    strings.TrimSpace(sessionID),
		AttachmentID: strings.TrimSpace(attachmentID),
		ConnectedAt:  time.Now(),
	}
	r.streams[stream.ID] = stream
	return stream, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.streams, stream.ID)
	}
}

func (r *streamRegistry) List() []StreamSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.streams))
	for id := range r.streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]StreamSummary, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.streams[id])
	}
	return out
}

func (h *eventHub) Event(event Event) Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nextEventLocked(event)
}

func (h *eventHub) Publish(event Event) {
	event = h.Event(event)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subscribers {
		select {
		case sub.ch <- event:
			sub.lastEventID = event.ID
		default:
		}
	}
}

func (h *eventHub) Subscribe() (<-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	ch := make(chan Event, 16)
	h.subscribers[id] = &eventSubscriber{
		id:          id,
		ch:          ch,
		connectedAt: time.Now(),
	}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		sub := h.subscribers[id]
		delete(h.subscribers, id)
		if sub == nil {
			return
		}
		close(ch)
	}
}

func (h *eventHub) MarkDelivered(events <-chan Event, eventID string) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subscribers {
		if (<-chan Event)(sub.ch) == events {
			sub.lastEventID = eventID
			return
		}
	}
}

func (h *eventHub) Streams() []StreamSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]int, 0, len(h.subscribers))
	for id := range h.subscribers {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]StreamSummary, 0, len(ids))
	for _, id := range ids {
		sub := h.subscribers[id]
		out = append(out, StreamSummary{
			ID:          fmt.Sprintf("event_stream_%08x", sub.id),
			Kind:        "events",
			State:       "open",
			ConnectedAt: sub.connectedAt,
			LastEventID: sub.lastEventID,
		})
	}
	return out
}

func (h *eventHub) nextEventLocked(event Event) Event {
	h.next++
	event.ID = fmt.Sprintf("evt_%08x", h.next)
	if event.At.IsZero() {
		event.At = time.Now()
	}
	if event.Session != nil {
		session := cloneSession(*event.Session)
		event.Session = &session
	}
	if event.Attachment != nil {
		attachment := *event.Attachment
		if attachment.Terminal != nil {
			terminal := *attachment.Terminal
			attachment.Terminal = &terminal
		}
		event.Attachment = &attachment
	}
	return event
}

func (r *sessionRegistry) Create(name string) Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	id := fmt.Sprintf("sess_%08x", r.next)
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	now := time.Now()
	session := Session{
		ID:          id,
		Name:        name,
		State:       "detached",
		Attachments: []ClientAttachment{},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	r.sessions[id] = session
	return cloneSession(session)
}

func (r *sessionRegistry) Get(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[strings.TrimSpace(id)]
	session.Jobs = r.mergedJobsLocked(session.ID, session.Jobs)
	return cloneSession(session), ok
}

func (r *sessionRegistry) GetAttachment(id, attachmentID string) (Session, ClientAttachment, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[strings.TrimSpace(id)]
	if !ok {
		return Session{}, ClientAttachment{}, false
	}
	session.Jobs = r.mergedJobsLocked(session.ID, session.Jobs)
	attachmentID = strings.TrimSpace(attachmentID)
	for _, attachment := range session.Attachments {
		if attachment.ID == attachmentID {
			return cloneSession(session), cloneAttachment(attachment), true
		}
	}
	return Session{}, ClientAttachment{}, false
}

func (r *sessionRegistry) Update(id string, req UpdateSessionRequest) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, sessionError{status: http.StatusNotFound, err: "session not found"}
	}
	session.HostCWD = strings.TrimSpace(req.HostCWD)
	session.SelectedContext = cloneSessionContext(req.SelectedContext)
	session.HostShells = cloneShellHandles(req.HostShells)
	session.GuestShells = cloneShellHandles(req.GuestShells)
	session.SSHShells = cloneShellHandles(req.SSHShells)
	session.Jobs = cloneJobSummaries(req.Jobs)
	session.UpdatedAt = time.Now()
	r.sessions[id] = session
	return cloneSession(session), nil
}

func (r *sessionRegistry) Delete(id string) (Session, []int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, nil, false
	}
	session.Jobs = r.mergedJobsLocked(id, session.Jobs)
	session.State = "closing"
	session.UpdatedAt = time.Now()
	jobIDs := r.jobIDsLocked(id)
	delete(r.sessions, id)
	delete(r.jobs, id)
	return cloneSession(session), jobIDs, true
}

func (r *sessionRegistry) Attach(id string, req AttachSessionRequest) (Session, ClientAttachment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, ClientAttachment{}, sessionError{status: http.StatusNotFound, err: "session not found"}
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "interactive"
	}
	if mode != "interactive" && mode != "observer" {
		return Session{}, ClientAttachment{}, sessionError{status: http.StatusBadRequest, err: "unsupported attachment mode"}
	}
	if mode == "interactive" {
		for _, attachment := range session.Attachments {
			if attachment.Mode == "interactive" {
				return Session{}, ClientAttachment{}, sessionError{status: http.StatusConflict, err: "session already has an interactive attachment"}
			}
		}
	}
	r.nextAttach++
	now := time.Now()
	attachment := ClientAttachment{
		ID:         fmt.Sprintf("attach_%08x", r.nextAttach),
		Mode:       mode,
		Terminal:   normalizeTerminal(req.Terminal),
		AttachedAt: now,
		UpdatedAt:  now,
	}
	session.Attachments = append(session.Attachments, attachment)
	session.State = "attached"
	session.UpdatedAt = now
	r.sessions[id] = session
	return cloneSession(session), attachment, nil
}

func (r *sessionRegistry) Detach(id, attachmentID string) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, sessionError{status: http.StatusNotFound, err: "session not found"}
	}
	attachmentID = strings.TrimSpace(attachmentID)
	if attachmentID == "" {
		return Session{}, sessionError{status: http.StatusBadRequest, err: "attachment_id is required"}
	}
	next := session.Attachments[:0]
	found := false
	for _, attachment := range session.Attachments {
		if attachment.ID == attachmentID {
			found = true
			continue
		}
		next = append(next, attachment)
	}
	if !found {
		return Session{}, sessionError{status: http.StatusNotFound, err: "attachment not found"}
	}
	session.Attachments = append([]ClientAttachment(nil), next...)
	if len(session.Attachments) == 0 {
		session.State = "detached"
	}
	session.UpdatedAt = time.Now()
	r.sessions[id] = session
	return cloneSession(session), nil
}

func (r *sessionRegistry) UpdateTerminal(id, attachmentID string, term Terminal) (Session, ClientAttachment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, ClientAttachment{}, sessionError{status: http.StatusNotFound, err: "session not found"}
	}
	attachmentID = strings.TrimSpace(attachmentID)
	if attachmentID == "" {
		return Session{}, ClientAttachment{}, sessionError{status: http.StatusBadRequest, err: "attachment id is required"}
	}
	for i, attachment := range session.Attachments {
		if attachment.ID != attachmentID {
			continue
		}
		now := time.Now()
		attachment.Terminal = normalizeTerminal(&term)
		attachment.UpdatedAt = now
		session.Attachments[i] = attachment
		session.UpdatedAt = now
		r.sessions[id] = session
		return cloneSession(session), attachment, nil
	}
	return Session{}, ClientAttachment{}, sessionError{status: http.StatusNotFound, err: "attachment not found"}
}

func (r *sessionRegistry) List() []SessionSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]SessionSummary, 0, len(ids))
	for _, id := range ids {
		session := r.sessions[id]
		session.Jobs = r.mergedJobsLocked(id, session.Jobs)
		out = append(out, SessionSummary{
			ID:              session.ID,
			Name:            session.Name,
			State:           session.State,
			HostCWD:         session.HostCWD,
			SelectedContext: cloneSessionContext(session.SelectedContext),
			HostShells:      cloneShellHandles(session.HostShells),
			GuestShells:     cloneShellHandles(session.GuestShells),
			SSHShells:       cloneShellHandles(session.SSHShells),
			Jobs:            cloneJobSummaries(session.Jobs),
			AttachedClients: cloneAttachments(session.Attachments),
			CreatedAt:       session.CreatedAt,
			UpdatedAt:       session.UpdatedAt,
		})
	}
	return out
}

func (r *sessionRegistry) Jobs() []JobSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessionIDs := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		sessionIDs = append(sessionIDs, id)
	}
	sort.Strings(sessionIDs)
	var out []JobSummary
	for _, sessionID := range sessionIDs {
		for _, job := range r.mergedJobsLocked(sessionID, r.sessions[sessionID].Jobs) {
			job.SessionID = sessionID
			out = append(out, job)
		}
	}
	return out
}

func (r *sessionRegistry) StartJob(sessionID string, req StartHostJobRequest) (JobSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessionID = strings.TrimSpace(sessionID)
	if _, ok := r.sessions[sessionID]; !ok {
		return JobSummary{}, sessionError{status: http.StatusNotFound, err: "session not found"}
	}
	r.nextJob++
	command := strings.Join(req.Command, " ")
	if strings.TrimSpace(req.Context) == "" {
		req.Context = "host"
	}
	job := JobSummary{
		ID:        r.nextJob,
		SessionID: sessionID,
		Context:   strings.TrimSpace(req.Context),
		Command:   strings.TrimSpace(command),
		Status:    "running",
		Control:   "vmshd",
		StartedAt: time.Now(),
	}
	if r.jobs[sessionID] == nil {
		r.jobs[sessionID] = map[int]JobSummary{}
	}
	r.jobs[sessionID][job.ID] = job
	session := r.sessions[sessionID]
	session.UpdatedAt = job.StartedAt
	r.sessions[sessionID] = session
	return job, nil
}

func (r *sessionRegistry) FinishJob(sessionID string, jobID int, result JobSummary) (JobSummary, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessionID = strings.TrimSpace(sessionID)
	jobs := r.jobs[sessionID]
	if jobs == nil {
		return JobSummary{}, false
	}
	job, ok := jobs[jobID]
	if !ok {
		return JobSummary{}, false
	}
	job.Status = strings.TrimSpace(result.Status)
	if job.Status == "" {
		job.Status = "exited"
	}
	job.ExitCode = result.ExitCode
	job.Error = strings.TrimSpace(result.Error)
	job.Logs = result.Logs
	job.LogDropped = result.LogDropped
	job.FinishedAt = result.FinishedAt
	if job.FinishedAt.IsZero() {
		job.FinishedAt = time.Now()
	}
	jobs[jobID] = job
	if session, ok := r.sessions[sessionID]; ok {
		session.UpdatedAt = job.FinishedAt
		r.sessions[sessionID] = session
	}
	return job, true
}

func (r *sessionRegistry) mergedJobsLocked(sessionID string, clientJobs []JobSummary) []JobSummary {
	out := cloneJobSummaries(clientJobs)
	ids := r.jobIDsLocked(sessionID)
	for _, id := range ids {
		out = append(out, r.jobs[sessionID][id])
	}
	return out
}

func (r *sessionRegistry) jobIDsLocked(sessionID string) []int {
	jobs := r.jobs[sessionID]
	ids := make([]int, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func cloneSession(session Session) Session {
	session.Attachments = cloneAttachments(session.Attachments)
	session.SelectedContext = cloneSessionContext(session.SelectedContext)
	session.HostShells = cloneShellHandles(session.HostShells)
	session.GuestShells = cloneShellHandles(session.GuestShells)
	session.SSHShells = cloneShellHandles(session.SSHShells)
	session.Jobs = cloneJobSummaries(session.Jobs)
	return session
}

func cloneAttachments(attachments []ClientAttachment) []ClientAttachment {
	if attachments == nil {
		return nil
	}
	out := make([]ClientAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		out = append(out, cloneAttachment(attachment))
	}
	return out
}

func cloneAttachment(attachment ClientAttachment) ClientAttachment {
	if attachment.Terminal != nil {
		term := *attachment.Terminal
		attachment.Terminal = &term
	}
	return attachment
}

func cloneSessionContext(ctx *SessionContext) *SessionContext {
	if ctx == nil {
		return nil
	}
	out := *ctx
	return &out
}

func cloneJobSummaries(jobs []JobSummary) []JobSummary {
	if jobs == nil {
		return nil
	}
	out := append([]JobSummary(nil), jobs...)
	for i := range out {
		out[i].Context = strings.TrimSpace(out[i].Context)
		out[i].Command = strings.TrimSpace(out[i].Command)
		out[i].Status = strings.TrimSpace(out[i].Status)
		out[i].Error = strings.TrimSpace(out[i].Error)
		out[i].Control = strings.TrimSpace(out[i].Control)
		out[i].Logs = strings.TrimSpace(out[i].Logs)
	}
	return out
}

func cloneShellHandles(handles []ShellHandle) []ShellHandle {
	if handles == nil {
		return nil
	}
	out := append([]ShellHandle(nil), handles...)
	for i := range out {
		out[i].ID = strings.TrimSpace(out[i].ID)
		out[i].Kind = strings.TrimSpace(out[i].Kind)
		out[i].Name = strings.TrimSpace(out[i].Name)
		out[i].Context = strings.TrimSpace(out[i].Context)
		out[i].CWD = strings.TrimSpace(out[i].CWD)
		out[i].VMID = strings.TrimSpace(out[i].VMID)
		out[i].SSHHost = strings.TrimSpace(out[i].SSHHost)
		out[i].User = strings.TrimSpace(out[i].User)
		out[i].State = strings.TrimSpace(out[i].State)
	}
	return out
}

type sessionError struct {
	status int
	err    string
}

func (e sessionError) Error() string {
	return e.err
}

func writeSessionError(w http.ResponseWriter, err error) {
	if err, ok := err.(sessionError); ok {
		writeJSON(w, err.status, client.ErrorResponse{Error: err.err})
		return
	}
	writeJSON(w, http.StatusInternalServerError, client.ErrorResponse{Error: err.Error()})
}

func normalizeTerminal(term *Terminal) *Terminal {
	if term == nil {
		return nil
	}
	out := *term
	if out.Cols < 0 {
		out.Cols = 0
	}
	if out.Rows < 0 {
		out.Rows = 0
	}
	return &out
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

func decodeRequiredJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return fmt.Errorf("request body is required")
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func NewClient(state backend.DaemonState) (*client.Client, error) {
	api := backend.NewClient(state.Addr)
	if err := backend.ApplyDaemonStateAuth(api, state); err != nil {
		return nil, err
	}
	return api, nil
}
