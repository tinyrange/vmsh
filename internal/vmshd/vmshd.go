package vmshd

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/vmsh/internal/backend"
	"j5.nz/cc/ccvmd"
	"j5.nz/cc/client"
)

const Kind = "vmshd"

type Status struct {
	Kind      string                 `json:"kind"`
	Status    string                 `json:"status"`
	Sessions  []SessionSummary       `json:"sessions"`
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

type Session struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	State           string             `json:"state"`
	HostCWD         string             `json:"host_cwd,omitempty"`
	SelectedContext *SessionContext    `json:"selected_context,omitempty"`
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

type CreateSessionRequest struct {
	Name string `json:"name,omitempty"`
}

type UpdateSessionRequest struct {
	HostCWD         string          `json:"host_cwd,omitempty"`
	SelectedContext *SessionContext `json:"selected_context,omitempty"`
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
	sessions   map[string]Session
}

type eventHub struct {
	mu          sync.Mutex
	next        int
	subscribers map[int]chan Event
}

type Server struct {
	token    string
	registry *sessionRegistry
	events   *eventHub

	startedAt time.Time
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
		startedAt: time.Now(),
	}
}

func (s *Server) RegisterHandlers(mux *http.ServeMux, runtime ccvmd.RuntimeView) {
	mux.HandleFunc("GET /vmsh/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Status{
			Kind:      Kind,
			Status:    "ok",
			Sessions:  s.registry.List(),
			VMs:       runtimeInstanceStatuses(runtime),
			StartedAt: s.startedAt,
		})
	})
	mux.HandleFunc("GET /vmsh/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.registry.List())
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
		session, ok := s.registry.Delete(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, client.ErrorResponse{Error: "session not found"})
			return
		}
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
	if err := json.NewEncoder(w).Encode(s.events.Event(Event{Kind: "connected"})); err != nil {
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
	return &sessionRegistry{sessions: map[string]Session{}}
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: map[int]chan Event{}}
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
	for _, ch := range h.subscribers {
		select {
		case ch <- event:
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
	h.subscribers[id] = ch
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subscribers, id)
		close(ch)
	}
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
	return cloneSession(session), ok
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
	session.UpdatedAt = time.Now()
	r.sessions[id] = session
	return cloneSession(session), nil
}

func (r *sessionRegistry) Delete(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, false
	}
	session.State = "closing"
	session.UpdatedAt = time.Now()
	delete(r.sessions, id)
	return cloneSession(session), true
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
		out = append(out, SessionSummary{
			ID:              session.ID,
			Name:            session.Name,
			State:           session.State,
			HostCWD:         session.HostCWD,
			SelectedContext: cloneSessionContext(session.SelectedContext),
			AttachedClients: append([]ClientAttachment(nil), session.Attachments...),
			CreatedAt:       session.CreatedAt,
			UpdatedAt:       session.UpdatedAt,
		})
	}
	return out
}

func cloneSession(session Session) Session {
	session.Attachments = append([]ClientAttachment(nil), session.Attachments...)
	session.SelectedContext = cloneSessionContext(session.SelectedContext)
	return session
}

func cloneSessionContext(ctx *SessionContext) *SessionContext {
	if ctx == nil {
		return nil
	}
	out := *ctx
	return &out
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
