package vmshd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyrange/vmsh/internal/backend"
)

func TestHTTPClientSessionLifecycle(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/vmsh/sessions", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if r.Method != http.MethodPost {
			t.Fatalf("create method = %s", r.Method)
		}
		var req CreateSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode create request: %v", err)
		}
		if req.Name != "main" {
			t.Fatalf("create request = %+v", req)
		}
		writeJSON(w, http.StatusOK, Session{ID: "sess_1", Name: req.Name, State: "detached"})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1/attach", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		var req AttachSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode attach request: %v", err)
		}
		if req.Mode != "interactive" || req.Terminal == nil || req.Terminal.Cols != 80 || req.Terminal.Rows != 24 {
			t.Fatalf("attach request = %+v", req)
		}
		writeJSON(w, http.StatusOK, AttachSessionResponse{
			Session:    Session{ID: "sess_1", Name: "main", State: "attached"},
			Attachment: ClientAttachment{ID: "attach_1", Mode: "interactive", Terminal: req.Terminal},
		})
	})
	mux.HandleFunc("/vmsh/sessions/sess_1", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if r.Method != http.MethodPatch {
			t.Fatalf("update method = %s", r.Method)
		}
		var req UpdateSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode update request: %v", err)
		}
		if req.HostCWD != "/work" || req.SelectedContext == nil || req.SelectedContext.Mode != "host" {
			t.Fatalf("update request = %+v", req)
		}
		writeJSON(w, http.StatusOK, Session{ID: "sess_1", Name: "main", State: "detached", HostCWD: req.HostCWD, SelectedContext: req.SelectedContext})
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
	session, err := client.CreateSession(CreateSessionRequest{Name: "main"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.ID != "sess_1" {
		t.Fatalf("session = %+v", session)
	}
	updated, err := client.UpdateSession(session.ID, UpdateSessionRequest{
		HostCWD:         "/work",
		SelectedContext: &SessionContext{Mode: "host", Name: "host"},
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}
	if updated.HostCWD != "/work" || updated.SelectedContext == nil || updated.SelectedContext.Mode != "host" {
		t.Fatalf("updated = %+v", updated)
	}
	attached, err := client.AttachSession(session.ID, AttachSessionRequest{
		Mode:     "interactive",
		Terminal: &Terminal{Cols: 80, Rows: 24},
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
	detached, err := client.DetachSession(session.ID, DetachSessionRequest{AttachmentID: attached.Attachment.ID})
	if err != nil {
		t.Fatalf("detach session: %v", err)
	}
	if detached.State != "detached" {
		t.Fatalf("detached = %+v", detached)
	}
}

func requireBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
}
