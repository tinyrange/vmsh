package vmshd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/backend"
	"golang.org/x/net/websocket"
)

func TestHTTPClientSessionLifecycle(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/vmsh/frontends", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, []FrontendSummary{{ID: "fe_1", Name: "vmsh", State: "open"}})
			return
		case http.MethodPost:
		default:
			t.Fatalf("frontends method = %s", r.Method)
		}
		var req RegisterFrontendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode frontend request: %v", err)
		}
		if req.Name != "vmsh" {
			t.Fatalf("frontend request = %+v", req)
		}
		writeJSON(w, http.StatusOK, FrontendSummary{ID: "fe_1", Name: req.Name, State: "open"})
	})
	mux.HandleFunc("/vmsh/frontends/fe_1", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if r.Method != http.MethodDelete {
			t.Fatalf("close frontend method = %s", r.Method)
		}
		writeJSON(w, http.StatusOK, FrontendSummary{ID: "fe_1", Name: "vmsh", State: "closed"})
	})
	mux.HandleFunc("/vmsh/sessions", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, []SessionSummary{{ID: "sess_1", Name: "main", State: "attached"}})
			return
		case http.MethodPost:
		default:
			t.Fatalf("sessions method = %s", r.Method)
		}
		var req CreateSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode create request: %v", err)
		}
		if req.Name != "main" || req.FrontendID != "fe_1" || req.Scope != "frontend" {
			t.Fatalf("create request = %+v", req)
		}
		writeJSON(w, http.StatusOK, Session{ID: "sess_1", Name: req.Name, State: "detached", Scope: req.Scope, FrontendID: req.FrontendID})
	})
	mux.HandleFunc("/vmsh/status", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if r.Method != http.MethodGet {
			t.Fatalf("status method = %s", r.Method)
		}
		writeJSON(w, http.StatusOK, Status{
			Kind:      "vmshd",
			Status:    "running",
			Frontends: []FrontendSummary{{ID: "fe_1", Name: "vmsh", State: "open"}},
			Sessions:  []SessionSummary{{ID: "sess_1", Name: "main", State: "attached", Scope: "frontend", FrontendID: "fe_1"}},
			Streams:   []StreamSummary{{ID: "terminal_stream_1", Kind: "terminal", SessionID: "sess_1"}},
			StartedAt: time.Unix(10, 0),
		})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1/attach", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		var req AttachSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode attach request: %v", err)
		}
		if req.FrontendID != "fe_1" || req.Mode != "interactive" || req.Terminal == nil || req.Terminal.Cols != 80 || req.Terminal.Rows != 24 {
			t.Fatalf("attach request = %+v", req)
		}
		writeJSON(w, http.StatusOK, AttachSessionResponse{
			Session:    Session{ID: "sess_1", Name: "main", State: "attached"},
			Attachment: ClientAttachment{ID: "attach_1", FrontendID: req.FrontendID, Mode: "interactive", Terminal: req.Terminal},
		})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, Session{ID: "sess_1", Name: "main", State: "attached", HostCWD: "/work"})
			return
		case http.MethodPatch:
		default:
			t.Fatalf("session method = %s", r.Method)
		}
		var req UpdateSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode update request: %v", err)
		}
		if req.HostCWD != "/work" || req.SelectedContext == nil || req.SelectedContext.Mode != "host" || len(req.HostShells) != 1 {
			t.Fatalf("update request = %+v", req)
		}
		writeJSON(w, http.StatusOK, Session{ID: "sess_1", Name: "main", State: "detached", HostCWD: req.HostCWD, SelectedContext: req.SelectedContext, HostShells: req.HostShells})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1/attachments/attach_1/terminal", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		var req Terminal
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode terminal request: %v", err)
		}
		if req.Cols != 100 || req.Rows != 40 {
			t.Fatalf("terminal request = %+v", req)
		}
		writeJSON(w, http.StatusOK, AttachSessionResponse{
			Session:    Session{ID: "sess_1", Name: "main", State: "attached"},
			Attachment: ClientAttachment{ID: "attach_1", Mode: "interactive", Terminal: &req},
		})
	})
	mux.HandleFunc("/vmsh/jobs", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if r.Method != http.MethodGet {
			t.Fatalf("jobs method = %s", r.Method)
		}
		writeJSON(w, http.StatusOK, []JobSummary{{ID: 1, SessionID: "sess_1", Command: "make", Status: "running"}})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1/jobs", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if r.Method != http.MethodPost {
			t.Fatalf("start job method = %s", r.Method)
		}
		var req StartHostJobRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode start job request: %v", err)
		}
		if len(req.Command) != 2 || req.Command[0] != "make" || req.Command[1] != "test" || req.Context != "host" {
			t.Fatalf("start job request = %+v", req)
		}
		writeJSON(w, http.StatusOK, JobSummary{ID: 2, SessionID: "sess_1", Command: "make test", Status: "running", Control: "vmshd"})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1/jobs/2", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if r.Method != http.MethodDelete {
			t.Fatalf("cancel job method = %s", r.Method)
		}
		writeJSON(w, http.StatusOK, JobSummary{ID: 2, SessionID: "sess_1", Command: "make test", Status: "canceling", Control: "vmshd"})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1/detach", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		var req DetachSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode detach request: %v", err)
		}
		if req.AttachmentID != "attach_1" {
			t.Fatalf("detach request = %+v", req)
		}
		writeJSON(w, http.StatusOK, Session{ID: "sess_1", Name: "main", State: "detached"})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1/persist", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		var req PersistSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode persist request: %v", err)
		}
		if req.Scope != "system" {
			t.Fatalf("persist request = %+v", req)
		}
		writeJSON(w, http.StatusOK, Session{ID: "sess_1", Name: "main", State: "attached", Scope: "system", DetachOnClose: true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "vmshd.token")
	if err := os.WriteFile(tokenPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	client, err := NewHTTPClient(backend.DaemonState{
		Addr:      strings.TrimPrefix(srv.URL, "http://"),
		TokenPath: tokenPath,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	frontend, err := client.RegisterFrontend(RegisterFrontendRequest{Name: "vmsh"})
	if err != nil {
		t.Fatalf("register frontend: %v", err)
	}
	if frontend.ID != "fe_1" || frontend.State != "open" {
		t.Fatalf("frontend = %+v", frontend)
	}
	session, err := client.CreateSession(CreateSessionRequest{Name: "main", FrontendID: frontend.ID, Scope: "frontend"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.ID != "sess_1" {
		t.Fatalf("session = %+v", session)
	}
	status, err := client.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Kind != "vmshd" || status.Status != "running" || len(status.Frontends) != 1 || len(status.Sessions) != 1 || len(status.Streams) != 1 {
		t.Fatalf("status = %+v", status)
	}
	sessions, err := client.Sessions()
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != session.ID || sessions[0].State != "attached" {
		t.Fatalf("sessions = %+v", sessions)
	}
	read, err := client.Session(session.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if read.ID != session.ID || read.HostCWD != "/work" {
		t.Fatalf("read session = %+v", read)
	}
	updated, err := client.UpdateSession(session.ID, UpdateSessionRequest{
		HostCWD:         "/work",
		SelectedContext: &SessionContext{Mode: "host", Name: "host"},
		HostShells:      []ShellHandle{{ID: "host", Kind: "host", State: "open"}},
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}
	if updated.HostCWD != "/work" || updated.SelectedContext == nil || updated.SelectedContext.Mode != "host" || len(updated.HostShells) != 1 {
		t.Fatalf("updated = %+v", updated)
	}
	attached, err := client.AttachSession(session.ID, AttachSessionRequest{
		FrontendID: frontend.ID,
		Mode:       "interactive",
		Terminal:   &Terminal{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	if attached.Attachment.ID != "attach_1" {
		t.Fatalf("attached = %+v", attached)
	}
	resized, err := client.UpdateTerminal(session.ID, attached.Attachment.ID, Terminal{Cols: 100, Rows: 40})
	if err != nil {
		t.Fatalf("update terminal: %v", err)
	}
	if resized.Attachment.Terminal == nil || resized.Attachment.Terminal.Cols != 100 {
		t.Fatalf("resized = %+v", resized)
	}
	jobs, err := client.Jobs()
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].SessionID != "sess_1" || jobs[0].Command != "make" {
		t.Fatalf("jobs = %+v", jobs)
	}
	job, err := client.StartHostJob(session.ID, StartHostJobRequest{Command: []string{"make", "test"}, Context: "host"})
	if err != nil {
		t.Fatalf("start host job: %v", err)
	}
	if job.ID != 2 || job.Status != "running" || job.Control != "vmshd" {
		t.Fatalf("started job = %+v", job)
	}
	canceled, err := client.CancelHostJob(session.ID, job.ID)
	if err != nil {
		t.Fatalf("cancel host job: %v", err)
	}
	if canceled.ID != job.ID || canceled.Status != "canceling" || canceled.Control != "vmshd" {
		t.Fatalf("canceled job = %+v", canceled)
	}
	detached, err := client.DetachSession(session.ID, DetachSessionRequest{AttachmentID: attached.Attachment.ID})
	if err != nil {
		t.Fatalf("detach session: %v", err)
	}
	if detached.State != "detached" {
		t.Fatalf("detached = %+v", detached)
	}
	persisted, err := client.PersistSession(session.ID, PersistSessionRequest{Scope: "system"})
	if err != nil {
		t.Fatalf("persist session: %v", err)
	}
	if persisted.Scope != "system" || !persisted.DetachOnClose {
		t.Fatalf("persisted = %+v", persisted)
	}
	closed, err := client.CloseFrontend(frontend.ID)
	if err != nil {
		t.Fatalf("close frontend: %v", err)
	}
	if closed.ID != frontend.ID || closed.State != "closed" {
		t.Fatalf("closed frontend = %+v", closed)
	}
}

func TestHTTPClientDialTerminalStream(t *testing.T) {
	resized := make(chan Terminal, 1)
	stdin := make(chan []byte, 1)
	mux := http.NewServeMux()
	mux.Handle("/vmsh/sessions/sess_1/attachments/attach_1/stream", websocket.Server{
		Handshake: func(_ *websocket.Config, r *http.Request) error {
			requireBearer(t, r)
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			if err := websocket.JSON.Send(ws, TerminalStreamMessage{
				Kind:   "attached",
				Stream: &StreamSummary{ID: "terminal_stream_1", Kind: "terminal", SessionID: "sess_1", AttachmentID: "attach_1"},
			}); err != nil {
				t.Errorf("send attached: %v", err)
				return
			}
			var msg TerminalStreamMessage
			if err := websocket.JSON.Receive(ws, &msg); err != nil {
				t.Errorf("receive resize: %v", err)
				return
			}
			if msg.Kind != "resize" || msg.Terminal == nil {
				t.Errorf("resize message = %+v", msg)
				return
			}
			resized <- *msg.Terminal
			if err := websocket.JSON.Receive(ws, &msg); err != nil {
				t.Errorf("receive stdin: %v", err)
				return
			}
			if msg.Kind != "stdin" || string(msg.Data) != "hello\n" {
				t.Errorf("stdin message = %+v", msg)
				return
			}
			stdin <- msg.Data
		},
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "vmshd.token")
	if err := os.WriteFile(tokenPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	client, err := NewHTTPClient(backend.DaemonState{
		Addr:      strings.TrimPrefix(srv.URL, "http://"),
		TokenPath: tokenPath,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	stream, err := client.DialTerminalStream(context.Background(), "sess_1", "attach_1")
	if err != nil {
		t.Fatalf("dial terminal stream: %v", err)
	}
	defer stream.Close()
	msg, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive attached: %v", err)
	}
	if msg.Kind != "attached" || msg.Stream == nil || msg.Stream.Kind != "terminal" || msg.Stream.AttachmentID != "attach_1" {
		t.Fatalf("attached message = %+v", msg)
	}
	if err := stream.Resize(Terminal{Cols: 120, Rows: 50}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	select {
	case got := <-resized:
		if got.Cols != 120 || got.Rows != 50 {
			t.Fatalf("resize terminal = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for resize")
	}
	if err := stream.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	select {
	case got := <-stdin:
		if string(got) != "hello\n" {
			t.Fatalf("stdin = %q", string(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stdin")
	}
}

func TestWebSocketURL(t *testing.T) {
	got, err := websocketURL("https://example.test/base?x=1", "/vmsh/events")
	if err != nil {
		t.Fatalf("websocketURL: %v", err)
	}
	if got != "wss://example.test/vmsh/events" {
		t.Fatalf("websocketURL = %q", got)
	}
	if _, err := websocketURL("unix:///tmp/vmshd.sock", "/vmsh/events"); err == nil {
		t.Fatal("unsupported websocket URL unexpectedly succeeded")
	}
}

func requireBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
}
