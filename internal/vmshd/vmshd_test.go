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

func TestSessionAttachDetachRoutes(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := srv.registry.Create("main")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/attach", bytes.NewBufferString(`{"terminal":{"cols":120,"rows":40}}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("attach status = %d body=%s", rr.Code, rr.Body.String())
	}
	var attached AttachSessionResponse
	if err := json.NewDecoder(rr.Body).Decode(&attached); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	if attached.Attachment.ID == "" || attached.Attachment.Mode != "interactive" {
		t.Fatalf("attachment = %+v", attached.Attachment)
	}
	if attached.Attachment.Terminal == nil || attached.Attachment.Terminal.Cols != 120 || attached.Attachment.Terminal.Rows != 40 {
		t.Fatalf("attachment terminal = %+v", attached.Attachment.Terminal)
	}
	if attached.Session.State != "attached" || len(attached.Session.Attachments) != 1 {
		t.Fatalf("attached session = %+v", attached.Session)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/attach", bytes.NewBufferString(`{"mode":"interactive"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("second interactive attach status = %d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/attach", bytes.NewBufferString(`{"mode":"observer"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("observer attach status = %d body=%s", rr.Code, rr.Body.String())
	}
	var observer AttachSessionResponse
	if err := json.NewDecoder(rr.Body).Decode(&observer); err != nil {
		t.Fatalf("decode observer response: %v", err)
	}
	if observer.Attachment.Mode != "observer" || len(observer.Session.Attachments) != 2 {
		t.Fatalf("observer response = %+v", observer)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rr.Code, rr.Body.String())
	}
	var status Status
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(status.Sessions) != 1 || len(status.Sessions[0].AttachedClients) != 2 {
		t.Fatalf("status sessions = %+v", status.Sessions)
	}

	rr = httptest.NewRecorder()
	resizeTarget := "/vmsh/sessions/" + session.ID + "/attachments/" + attached.Attachment.ID + "/terminal"
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, resizeTarget, bytes.NewBufferString(`{"cols":100,"rows":32}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("resize status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resized AttachSessionResponse
	if err := json.NewDecoder(rr.Body).Decode(&resized); err != nil {
		t.Fatalf("decode resize response: %v", err)
	}
	if resized.Attachment.Terminal == nil || resized.Attachment.Terminal.Cols != 100 || resized.Attachment.Terminal.Rows != 32 {
		t.Fatalf("resized attachment = %+v", resized.Attachment)
	}

	rr = httptest.NewRecorder()
	body := `{"attachment_id":"` + attached.Attachment.ID + `"}`
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/detach", bytes.NewBufferString(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("detach status = %d body=%s", rr.Code, rr.Body.String())
	}
	var detached Session
	if err := json.NewDecoder(rr.Body).Decode(&detached); err != nil {
		t.Fatalf("decode detached session: %v", err)
	}
	if detached.State != "attached" || len(detached.Attachments) != 1 || detached.Attachments[0].ID != observer.Attachment.ID {
		t.Fatalf("detached session = %+v", detached)
	}

	rr = httptest.NewRecorder()
	body = `{"attachment_id":"` + observer.Attachment.ID + `"}`
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vmsh/sessions/"+session.ID+"/detach", bytes.NewBufferString(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("detach observer status = %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.NewDecoder(rr.Body).Decode(&detached); err != nil {
		t.Fatalf("decode detached observer session: %v", err)
	}
	if detached.State != "detached" || len(detached.Attachments) != 0 {
		t.Fatalf("fully detached session = %+v", detached)
	}
}

func TestSessionAttachRejectsBadRequests(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := srv.registry.Create("main")

	for _, tc := range []struct {
		name   string
		target string
		body   string
		status int
	}{
		{name: "missing session", target: "/vmsh/sessions/sess_missing/attach", body: `{}`, status: http.StatusNotFound},
		{name: "bad mode", target: "/vmsh/sessions/" + session.ID + "/attach", body: `{"mode":"writer"}`, status: http.StatusBadRequest},
		{name: "bad attach json", target: "/vmsh/sessions/" + session.ID + "/attach", body: `{`, status: http.StatusBadRequest},
		{name: "missing attachment id", target: "/vmsh/sessions/" + session.ID + "/detach", body: `{}`, status: http.StatusBadRequest},
		{name: "missing attachment", target: "/vmsh/sessions/" + session.ID + "/detach", body: `{"attachment_id":"attach_missing"}`, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.target, bytes.NewBufferString(tc.body)))
			if rr.Code != tc.status {
				t.Fatalf("status = %d want %d body=%s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}
}

func TestSessionTerminalUpdateRejectsBadRequests(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := srv.registry.Create("main")
	_, attachment, err := srv.registry.Attach(session.ID, AttachSessionRequest{})
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}

	for _, tc := range []struct {
		name   string
		target string
		body   string
		status int
	}{
		{name: "bad json", target: "/vmsh/sessions/" + session.ID + "/attachments/" + attachment.ID + "/terminal", body: `{`, status: http.StatusBadRequest},
		{name: "missing session", target: "/vmsh/sessions/sess_missing/attachments/" + attachment.ID + "/terminal", body: `{}`, status: http.StatusNotFound},
		{name: "missing attachment", target: "/vmsh/sessions/" + session.ID + "/attachments/attach_missing/terminal", body: `{}`, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.target, bytes.NewBufferString(tc.body)))
			if rr.Code != tc.status {
				t.Fatalf("status = %d want %d body=%s", rr.Code, tc.status, rr.Body.String())
			}
		})
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
