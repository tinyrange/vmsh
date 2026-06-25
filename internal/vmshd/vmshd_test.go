package vmshd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"j5.nz/cc/client"
)

func TestTokenFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmshd.token")
	token, err := writeTokenFile(path)
	if err != nil {
		t.Fatalf("write token: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("token mode = %o, want 600", got)
		}
	}
}

func TestAuthenticateRequiresBearerToken(t *testing.T) {
	srv := NewServer("secret")
	handler := srv.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, tc := range []struct {
		name   string
		header string
		status int
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Basic secret", status: http.StatusUnauthorized},
		{name: "wrong token", header: "Bearer nope", status: http.StatusUnauthorized},
		{name: "valid", header: "Bearer secret", status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d", rr.Code, tc.status)
			}
		})
	}
}

func TestStatusRoute(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	runtime := fakeRuntimeView{statuses: []client.InstanceState{{ID: "vm1", Status: "running"}}}
	srv.RegisterHandlers(mux, runtime)
	session := srv.registry.Create("main")

	req := httptest.NewRequest(http.MethodGet, "/vmsh/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rr.Code, rr.Body.String())
	}
	var status Status
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Kind != Kind || status.Status != "ok" || len(status.Sessions) != 1 {
		t.Fatalf("status = %+v", status)
	}
	if status.Sessions[0].ID != session.ID || status.Sessions[0].Name != "main" || status.Sessions[0].State != "detached" {
		t.Fatalf("status sessions = %+v, want session %+v", status.Sessions, session)
	}
	if len(status.VMs) != 1 || status.VMs[0].ID != "vm1" || status.VMs[0].Status != "running" {
		t.Fatalf("status VMs = %+v", status.VMs)
	}
}

func TestSessionRoutesCreateListReadAndDelete(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions", bytes.NewBufferString(`{"name":"main"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created Session
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	if created.ID == "" || created.Name != "main" || created.State != "detached" {
		t.Fatalf("created session = %+v", created)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/sessions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rr.Code, rr.Body.String())
	}
	var sessions []SessionSummary
	if err := json.NewDecoder(rr.Body).Decode(&sessions); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != created.ID || sessions[0].Name != "main" {
		t.Fatalf("sessions = %+v, want created %+v", sessions, created)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/sessions/"+created.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("read status = %d body=%s", rr.Code, rr.Body.String())
	}
	var read Session
	if err := json.NewDecoder(rr.Body).Decode(&read); err != nil {
		t.Fatalf("decode read session: %v", err)
	}
	if read.ID != created.ID || read.Name != created.Name {
		t.Fatalf("read session = %+v, want %+v", read, created)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/vmsh/sessions/"+created.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	var deleted Session
	if err := json.NewDecoder(rr.Body).Decode(&deleted); err != nil {
		t.Fatalf("decode deleted session: %v", err)
	}
	if deleted.ID != created.ID || deleted.State != "closing" {
		t.Fatalf("deleted session = %+v", deleted)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/sessions/"+created.ID, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("read deleted status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateSessionRejectsBadJSON(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions", bytes.NewBufferString("{")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

type fakeRuntimeView struct {
	statuses []client.InstanceState
}

func (f fakeRuntimeView) InstanceStatuses() []client.InstanceState {
	return f.statuses
}
