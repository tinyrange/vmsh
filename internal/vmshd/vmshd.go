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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
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
	Job        *JobSummary       `json:"job,omitempty"`
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
	VMRefs          []VMRef            `json:"vm_refs,omitempty"`
	HostShells      []ShellHandle      `json:"host_shells,omitempty"`
	GuestShells     []ShellHandle      `json:"guest_shells,omitempty"`
	SSHShells       []ShellHandle      `json:"ssh_shells,omitempty"`
	Jobs            []JobSummary       `json:"jobs,omitempty"`
	Copies          []CopySummary      `json:"copies,omitempty"`
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
	VMRefs          []VMRef            `json:"vm_refs,omitempty"`
	HostShells      []ShellHandle      `json:"host_shells,omitempty"`
	GuestShells     []ShellHandle      `json:"guest_shells,omitempty"`
	SSHShells       []ShellHandle      `json:"ssh_shells,omitempty"`
	Jobs            []JobSummary       `json:"jobs,omitempty"`
	Copies          []CopySummary      `json:"copies,omitempty"`
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

type VMRef struct {
	ID        string `json:"id"`
	BackendID string `json:"backend_id,omitempty"`
	Context   string `json:"context,omitempty"`
	Image     string `json:"image,omitempty"`
	Isolated  bool   `json:"isolated,omitempty"`
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

type CopySummary struct {
	ID         int       `json:"id"`
	Source     string    `json:"source"`
	Dest       string    `json:"dest"`
	Status     string    `json:"status"`
	Bytes      int64     `json:"bytes,omitempty"`
	Error      string    `json:"error,omitempty"`
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
	VMRefs          []VMRef         `json:"vm_refs,omitempty"`
	HostShells      []ShellHandle   `json:"host_shells,omitempty"`
	GuestShells     []ShellHandle   `json:"guest_shells,omitempty"`
	SSHShells       []ShellHandle   `json:"ssh_shells,omitempty"`
	Jobs            []JobSummary    `json:"jobs,omitempty"`
	Copies          []CopySummary   `json:"copies,omitempty"`
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
	hostShells map[string]ShellHandle
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
	shells   *hostShellManager

	startedAt time.Time
}

type hostJobRunner struct {
	mu      sync.Mutex
	cancels map[int]context.CancelFunc
}

type hostShellManager struct {
	mu     sync.Mutex
	shells map[string]*hostShell
}

type hostShell struct {
	sessionID   string
	cmd         *exec.Cmd
	tty         *os.File
	done        chan struct{}
	doneOnce    sync.Once
	mu          sync.Mutex
	nextSub     int
	subscribers map[int]chan []byte
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
		shells:    newHostShellManager(),
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
		s.shells.Close(session.ID)
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
		s.events.Publish(Event{Kind: "job_started", Session: &session, Job: &job})
		writeJSON(w, http.StatusOK, job)
	})
	mux.HandleFunc("DELETE /vmsh/sessions/{id}/jobs/{job}", func(w http.ResponseWriter, r *http.Request) {
		jobID, err := parseJobID(r.PathValue("job"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, client.ErrorResponse{Error: err.Error()})
			return
		}
		job, err := s.cancelHostJob(r.PathValue("id"), jobID)
		if err != nil {
			writeSessionError(w, err)
			return
		}
		session, _ := s.registry.Get(r.PathValue("id"))
		s.events.Publish(Event{Kind: "job_canceled", Session: &session, Job: &job})
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
		finished, ok := s.registry.FinishJob(sessionID, job.ID, summarizeHostJobResult(output, runErr, ctx.Err() != nil))
		if !ok {
			return
		}
		session, _ := s.registry.Get(sessionID)
		s.events.Publish(Event{Kind: "job_finished", Session: &session, Job: &finished})
	}()
	return job, nil
}

func (s *Server) cancelHostJob(sessionID string, jobID int) (JobSummary, error) {
	job, err := s.registry.RequestCancelJob(sessionID, jobID)
	if err != nil {
		return JobSummary{}, err
	}
	if job.Status == "canceling" {
		if !s.jobs.CancelOne(job.ID) {
			return JobSummary{}, sessionError{status: http.StatusConflict, err: "job is not running"}
		}
	}
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

func (r *hostJobRunner) CancelOne(id int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancel := r.cancels[id]
	if cancel == nil {
		return false
	}
	cancel()
	delete(r.cancels, id)
	return true
}

func summarizeHostJobResult(output []byte, err error, canceled bool) JobSummary {
	job := JobSummary{
		Status:     "exited",
		Logs:       boundedJobLogs(output),
		LogDropped: len(output) > maxJobLogBytes,
		FinishedAt: time.Now(),
	}
	if canceled {
		job.Status = "canceled"
		if err != nil {
			job.Error = err.Error()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			job.ExitCode = exitErr.ExitCode()
		}
		return job
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

func parseJobID(value string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid job id %q", value)
	}
	return id, nil
}

func boundedJobLogs(output []byte) string {
	if len(output) <= maxJobLogBytes {
		return string(output)
	}
	return string(output[len(output)-maxJobLogBytes:])
}

func newHostShellManager() *hostShellManager {
	return &hostShellManager{shells: map[string]*hostShell{}}
}

func (m *hostShellManager) Start(sessionID string, term *Terminal) (*hostShell, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	m.mu.Lock()
	if shell := m.shells[sessionID]; shell != nil && shell.Running() {
		m.mu.Unlock()
		_ = shell.SetSize(term)
		return shell, nil
	}
	delete(m.shells, sessionID)
	m.mu.Unlock()

	cmd := exec.Command(hostShellCommand())
	cmd.Env = append(os.Environ(), "VMSH_ACTIVE=1")
	tty, err := pty.StartWithSize(cmd, terminalWinsize(term))
	if err != nil {
		return nil, err
	}
	shell := &hostShell{
		sessionID:   sessionID,
		cmd:         cmd,
		tty:         tty,
		done:        make(chan struct{}),
		subscribers: map[int]chan []byte{},
	}
	m.mu.Lock()
	m.shells[sessionID] = shell
	m.mu.Unlock()
	go shell.readLoop()
	go func() {
		_ = cmd.Wait()
		shell.closeDone()
		m.mu.Lock()
		if m.shells[sessionID] == shell {
			delete(m.shells, sessionID)
		}
		m.mu.Unlock()
	}()
	return shell, nil
}

func (m *hostShellManager) Close(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	shell := m.shells[sessionID]
	delete(m.shells, sessionID)
	m.mu.Unlock()
	if shell == nil {
		return
	}
	shell.Close()
}

func (s *hostShell) Running() bool {
	if s == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *hostShell) SetSize(term *Terminal) error {
	if s == nil || s.tty == nil || term == nil {
		return nil
	}
	return pty.Setsize(s.tty, terminalWinsize(term))
}

func (s *hostShell) Write(data []byte) error {
	if s == nil || s.tty == nil || len(data) == 0 {
		return nil
	}
	_, err := s.tty.Write(data)
	return err
}

func (s *hostShell) Subscribe() (<-chan []byte, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSub++
	id := s.nextSub
	ch := make(chan []byte, 32)
	s.subscribers[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if sub := s.subscribers[id]; sub != nil {
			delete(s.subscribers, id)
			close(sub)
		}
	}
}

func (s *hostShell) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.tty.Read(buf)
		if n > 0 {
			s.publish(buf[:n])
		}
		if err != nil {
			s.closeDone()
			return
		}
	}
}

func (s *hostShell) publish(data []byte) {
	data = append([]byte(nil), data...)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.subscribers {
		select {
		case sub <- data:
		default:
		}
	}
}

func (s *hostShell) Close() {
	if s == nil {
		return
	}
	_ = s.tty.Close()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.closeDone()
}

func (s *hostShell) closeDone() {
	s.doneOnce.Do(func() {
		_ = s.tty.Close()
		close(s.done)
		s.mu.Lock()
		defer s.mu.Unlock()
		for id, sub := range s.subscribers {
			delete(s.subscribers, id)
			close(sub)
		}
	})
}

func terminalWinsize(term *Terminal) *pty.Winsize {
	rows := uint16(24)
	cols := uint16(80)
	if term != nil {
		if term.Rows > 0 {
			rows = uint16(term.Rows)
		}
		if term.Cols > 0 {
			cols = uint16(term.Cols)
		}
	}
	return &pty.Winsize{Rows: rows, Cols: cols}
}

func hostShellCommand() string {
	if runtime.GOOS == "windows" {
		return firstNonEmpty(os.Getenv("COMSPEC"), "cmd.exe")
	}
	return firstNonEmpty(os.Getenv("SHELL"), "/bin/sh")
}

func currentWorkingDirectory() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
	sendMu := &sync.Mutex{}
	send := func(msg TerminalStreamMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return websocket.JSON.Send(ws, msg)
	}
	if err := send(TerminalStreamMessage{Kind: "attached", Stream: &stream}); err != nil {
		return
	}
	shell, err := s.shells.Start(session.ID, attachment.Terminal)
	if err != nil {
		_ = send(TerminalStreamMessage{Kind: "error"})
		return
	}
	s.registry.SetDaemonHostShell(session.ID, ShellHandle{
		ID:      "host",
		Kind:    "host",
		Name:    "host",
		Context: "host",
		CWD:     firstNonEmpty(strings.TrimSpace(session.HostCWD), currentWorkingDirectory()),
		State:   "open",
	})
	go func(sessionID, cwd string) {
		<-shell.done
		s.registry.SetDaemonHostShell(sessionID, ShellHandle{
			ID:      "host",
			Kind:    "host",
			Name:    "host",
			Context: "host",
			CWD:     cwd,
			State:   "closed",
		})
	}(session.ID, firstNonEmpty(strings.TrimSpace(session.HostCWD), currentWorkingDirectory()))
	output, unsubscribe := shell.Subscribe()
	defer unsubscribe()
	sendErr := make(chan error, 1)
	go func() {
		for data := range output {
			if err := send(TerminalStreamMessage{Kind: "data", Data: data}); err != nil {
				sendErr <- err
				return
			}
		}
	}()
	for {
		var msg TerminalStreamMessage
		select {
		case <-sendErr:
			return
		default:
		}
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			return
		}
		switch strings.TrimSpace(msg.Kind) {
		case "resize":
			if msg.Terminal == nil {
				_ = send(TerminalStreamMessage{Kind: "error"})
				continue
			}
			updated, updatedAttachment, err := s.registry.UpdateTerminal(session.ID, attachment.ID, *msg.Terminal)
			if err != nil {
				_ = send(TerminalStreamMessage{Kind: "error"})
				continue
			}
			session = updated
			attachment = updatedAttachment
			_ = shell.SetSize(attachment.Terminal)
			s.events.Publish(Event{Kind: "terminal_updated", Session: &session, Attachment: &attachment})
		case "stdin", "data":
			if len(msg.Data) > 0 {
				if err := shell.Write(msg.Data); err != nil {
					_ = send(TerminalStreamMessage{Kind: "error"})
					return
				}
			}
		case "close":
			return
		default:
			_ = send(TerminalStreamMessage{Kind: "error"})
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
	return &sessionRegistry{sessions: map[string]Session{}, jobs: map[string]map[int]JobSummary{}, hostShells: map[string]ShellHandle{}}
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
	session.HostShells = r.mergedHostShellsLocked(session.ID, session.HostShells)
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
	session.HostShells = r.mergedHostShellsLocked(session.ID, session.HostShells)
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
	session.VMRefs = cloneVMRefs(req.VMRefs)
	session.HostShells = cloneShellHandles(req.HostShells)
	session.GuestShells = cloneShellHandles(req.GuestShells)
	session.SSHShells = cloneShellHandles(req.SSHShells)
	session.Jobs = cloneJobSummaries(req.Jobs)
	session.Copies = cloneCopySummaries(req.Copies)
	session.HostShells = r.mergedHostShellsLocked(id, session.HostShells)
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
	session.HostShells = r.mergedHostShellsLocked(id, session.HostShells)
	session.State = "closing"
	session.UpdatedAt = time.Now()
	jobIDs := r.jobIDsLocked(id)
	delete(r.sessions, id)
	delete(r.jobs, id)
	delete(r.hostShells, id)
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
		session.HostShells = r.mergedHostShellsLocked(id, session.HostShells)
		out = append(out, SessionSummary{
			ID:              session.ID,
			Name:            session.Name,
			State:           session.State,
			HostCWD:         session.HostCWD,
			SelectedContext: cloneSessionContext(session.SelectedContext),
			VMRefs:          cloneVMRefs(session.VMRefs),
			HostShells:      cloneShellHandles(session.HostShells),
			GuestShells:     cloneShellHandles(session.GuestShells),
			SSHShells:       cloneShellHandles(session.SSHShells),
			Jobs:            cloneJobSummaries(session.Jobs),
			Copies:          cloneCopySummaries(session.Copies),
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

func (r *sessionRegistry) RequestCancelJob(sessionID string, jobID int) (JobSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessionID = strings.TrimSpace(sessionID)
	if _, ok := r.sessions[sessionID]; !ok {
		return JobSummary{}, sessionError{status: http.StatusNotFound, err: "session not found"}
	}
	jobs := r.jobs[sessionID]
	if jobs == nil {
		return JobSummary{}, sessionError{status: http.StatusNotFound, err: "job not found"}
	}
	job, ok := jobs[jobID]
	if !ok {
		return JobSummary{}, sessionError{status: http.StatusNotFound, err: "job not found"}
	}
	if job.Control != "vmshd" {
		return JobSummary{}, sessionError{status: http.StatusBadRequest, err: "job is not daemon-owned"}
	}
	if job.Status != "running" && job.Status != "canceling" {
		return job, nil
	}
	job.Status = "canceling"
	jobs[jobID] = job
	if session, ok := r.sessions[sessionID]; ok {
		session.UpdatedAt = time.Now()
		r.sessions[sessionID] = session
	}
	return job, nil
}

func (r *sessionRegistry) SetDaemonHostShell(sessionID string, handle ShellHandle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessionID = strings.TrimSpace(sessionID)
	if _, ok := r.sessions[sessionID]; !ok {
		return
	}
	handle.ID = firstNonEmpty(strings.TrimSpace(handle.ID), "host")
	handle.Kind = firstNonEmpty(strings.TrimSpace(handle.Kind), "host")
	handle.Name = strings.TrimSpace(handle.Name)
	handle.Context = firstNonEmpty(strings.TrimSpace(handle.Context), "host")
	handle.CWD = strings.TrimSpace(handle.CWD)
	handle.State = firstNonEmpty(strings.TrimSpace(handle.State), "open")
	r.hostShells[sessionID] = handle
	session := r.sessions[sessionID]
	session.UpdatedAt = time.Now()
	r.sessions[sessionID] = session
}

func (r *sessionRegistry) mergedJobsLocked(sessionID string, clientJobs []JobSummary) []JobSummary {
	out := cloneJobSummaries(clientJobs)
	ids := r.jobIDsLocked(sessionID)
	for _, id := range ids {
		out = append(out, r.jobs[sessionID][id])
	}
	return out
}

func (r *sessionRegistry) mergedHostShellsLocked(sessionID string, clientHandles []ShellHandle) []ShellHandle {
	out := cloneShellHandles(clientHandles)
	handle, ok := r.hostShells[sessionID]
	if !ok {
		return out
	}
	for i := range out {
		if out[i].ID == handle.ID && out[i].Kind == handle.Kind {
			out[i] = handle
			return out
		}
	}
	return append(out, handle)
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
	session.VMRefs = cloneVMRefs(session.VMRefs)
	session.HostShells = cloneShellHandles(session.HostShells)
	session.GuestShells = cloneShellHandles(session.GuestShells)
	session.SSHShells = cloneShellHandles(session.SSHShells)
	session.Jobs = cloneJobSummaries(session.Jobs)
	session.Copies = cloneCopySummaries(session.Copies)
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

func cloneVMRefs(refs []VMRef) []VMRef {
	if refs == nil {
		return nil
	}
	return append([]VMRef(nil), refs...)
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

func cloneCopySummaries(copies []CopySummary) []CopySummary {
	if copies == nil {
		return nil
	}
	out := append([]CopySummary(nil), copies...)
	for i := range out {
		out[i].Source = strings.TrimSpace(out[i].Source)
		out[i].Dest = strings.TrimSpace(out[i].Dest)
		out[i].Status = strings.TrimSpace(out[i].Status)
		out[i].Error = strings.TrimSpace(out[i].Error)
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
