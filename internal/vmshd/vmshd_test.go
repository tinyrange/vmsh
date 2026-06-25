package vmshd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
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
	updated, err := srv.registry.Update(session.ID, UpdateSessionRequest{
		HostCWD:         "/work",
		SelectedContext: &SessionContext{Mode: "host", Name: "host", Short: "host", Source: "host"},
		HostShells:      []ShellHandle{{ID: "host", Kind: "host", Name: "host", CWD: "/work", State: "open"}},
		Jobs:            []JobSummary{{ID: 1, Command: "sleep 1", Status: "running", StartedAt: time.Now()}},
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}

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
	if status.Sessions[0].HostCWD != updated.HostCWD || status.Sessions[0].SelectedContext == nil || status.Sessions[0].SelectedContext.Mode != "host" {
		t.Fatalf("status session metadata = %+v, want %+v", status.Sessions[0], updated)
	}
	if len(status.Sessions[0].Jobs) != 1 || status.Sessions[0].Jobs[0].Command != "sleep 1" {
		t.Fatalf("status session jobs = %+v", status.Sessions[0].Jobs)
	}
	if len(status.Sessions[0].HostShells) != 1 || status.Sessions[0].HostShells[0].CWD != "/work" {
		t.Fatalf("status host shells = %+v", status.Sessions[0].HostShells)
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
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, "/vmsh/sessions/"+created.ID, bytes.NewBufferString(`{"host_cwd":"/work","selected_context":{"mode":"vm","name":"dev","short":"vm:dev","source":"docker:debian","vm":"dev","image":"debian","cwd":"/repo","user":"root","isolated":true},"host_shells":[{"id":"host","kind":"host","name":"host","cwd":"/work","state":"open"}],"guest_shells":[{"id":"dev","kind":"guest","name":"dev","context":"vm:dev","cwd":"/repo","vm":"dev","user":"root","state":"open"}],"ssh_shells":[{"id":"ssh","kind":"ssh","name":"app","context":"ssh:app","cwd":"/srv","ssh_host":"app","user":"me","state":"open"}],"jobs":[{"id":1,"context":"vm:dev","command":"make","status":"running","control":"vm:dev","logs":"@jobs logs 1","started_at":"2026-06-25T00:00:00Z"}]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", rr.Code, rr.Body.String())
	}
	var updated Session
	if err := json.NewDecoder(rr.Body).Decode(&updated); err != nil {
		t.Fatalf("decode updated session: %v", err)
	}
	if updated.ID != created.ID || updated.HostCWD != "/work" || updated.SelectedContext == nil || updated.SelectedContext.Mode != "vm" || updated.SelectedContext.VMID != "dev" || updated.SelectedContext.CWD != "/repo" || !updated.SelectedContext.Isolated {
		t.Fatalf("updated session = %+v", updated)
	}
	if len(updated.Jobs) != 1 || updated.Jobs[0].Command != "make" || updated.Jobs[0].Status != "running" {
		t.Fatalf("updated jobs = %+v", updated.Jobs)
	}
	if len(updated.HostShells) != 1 || len(updated.GuestShells) != 1 || len(updated.SSHShells) != 1 {
		t.Fatalf("updated shell handles host=%+v guest=%+v ssh=%+v", updated.HostShells, updated.GuestShells, updated.SSHShells)
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
	if sessions[0].HostCWD != updated.HostCWD || sessions[0].SelectedContext == nil || sessions[0].SelectedContext.Short != "vm:dev" {
		t.Fatalf("session summary metadata = %+v, want %+v", sessions[0], updated)
	}
	if len(sessions[0].Jobs) != 1 || sessions[0].Jobs[0].Logs != "@jobs logs 1" {
		t.Fatalf("session summary jobs = %+v", sessions[0].Jobs)
	}
	if len(sessions[0].GuestShells) != 1 || sessions[0].GuestShells[0].VMID != "dev" {
		t.Fatalf("session summary guest shells = %+v", sessions[0].GuestShells)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/jobs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("jobs status = %d body=%s", rr.Code, rr.Body.String())
	}
	var jobs []JobSummary
	if err := json.NewDecoder(rr.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].SessionID != created.ID || jobs[0].Command != "make" {
		t.Fatalf("jobs = %+v", jobs)
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
	if read.HostCWD != updated.HostCWD || read.SelectedContext == nil || read.SelectedContext.Name != "dev" {
		t.Fatalf("read session metadata = %+v, want %+v", read, updated)
	}
	if len(read.Jobs) != 1 || read.Jobs[0].Context != "vm:dev" {
		t.Fatalf("read session jobs = %+v", read.Jobs)
	}
	if len(read.SSHShells) != 1 || read.SSHShells[0].SSHHost != "app" {
		t.Fatalf("read session ssh shells = %+v", read.SSHShells)
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

func TestDaemonOwnedHostJobRunsDetachedAndUpdatesSessionState(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := srv.registry.Create("main")
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	body := fmt.Sprintf(`{"command":[%q,"-test.run=TestDaemonHostJobHelper","--"],"env":["VMSHD_TEST_HOST_JOB=1","VMSHD_TEST_VALUE=ok"],"context":"host"}`, os.Args[0])
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/vmsh/sessions/"+session.ID+"/jobs", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new job request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start job status = %d", resp.StatusCode)
	}
	var started JobSummary
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("decode started job: %v", err)
	}
	if started.ID == 0 || started.SessionID != session.ID || started.Status != "running" || started.Context != "host" {
		t.Fatalf("started job = %+v", started)
	}

	jobs := getJobsFromServer(t, httpSrv.URL, "secret")
	if len(jobs) != 1 || jobs[0].ID != started.ID || jobs[0].Status != "running" {
		t.Fatalf("running jobs = %+v", jobs)
	}
	read, ok := srv.registry.Get(session.ID)
	if !ok || read.State != "detached" || len(read.Jobs) != 1 || read.Jobs[0].ID != started.ID {
		t.Fatalf("detached session jobs = %+v ok=%v", read, ok)
	}

	requireEventually(t, func() bool {
		jobs := getJobsFromServer(t, httpSrv.URL, "secret")
		return len(jobs) == 1 && jobs[0].Status == "exited" && jobs[0].FinishedAt.After(jobs[0].StartedAt) && strings.Contains(jobs[0].Logs, "daemon-job:ok")
	})
}

func TestDaemonHostJobHelper(t *testing.T) {
	if os.Getenv("VMSHD_TEST_HOST_JOB") != "1" {
		return
	}
	time.Sleep(100 * time.Millisecond)
	fmt.Fprintf(os.Stdout, "daemon-job:%s\n", os.Getenv("VMSHD_TEST_VALUE"))
	os.Exit(0)
}

func TestTerminalAttachmentStreamTracksActiveStreamAndResize(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	session := srv.registry.Create("main")
	_, attachment, err := srv.registry.Attach(session.ID, AttachSessionRequest{Terminal: &Terminal{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	target := strings.Replace(httpSrv.URL, "http://", "ws://", 1) + "/vmsh/sessions/" + session.ID + "/attachments/" + attachment.ID + "/stream"
	cfg, err := websocket.NewConfig(target, httpSrv.URL)
	if err != nil {
		t.Fatalf("websocket config: %v", err)
	}
	cfg.Header.Set("Authorization", "Bearer secret")
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("dial stream: %v", err)
	}
	defer ws.Close()

	var attached TerminalStreamMessage
	if err := websocket.JSON.Receive(ws, &attached); err != nil {
		t.Fatalf("receive attached message: %v", err)
	}
	if attached.Kind != "attached" || attached.Stream == nil || attached.Stream.Kind != "terminal" || attached.Stream.SessionID != session.ID || attached.Stream.AttachmentID != attachment.ID {
		t.Fatalf("attached message = %+v", attached)
	}

	status := getStatusFromServer(t, httpSrv.URL, "secret")
	if len(status.Streams) != 1 || status.Streams[0].Kind != "terminal" || status.Streams[0].AttachmentID != attachment.ID {
		t.Fatalf("status streams = %+v", status.Streams)
	}

	if err := websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "resize", Terminal: &Terminal{Cols: 100, Rows: 40}}); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	requireEventually(t, func() bool {
		read, ok := srv.registry.Get(session.ID)
		return ok && len(read.Attachments) == 1 && read.Attachments[0].Terminal != nil && read.Attachments[0].Terminal.Cols == 100 && read.Attachments[0].Terminal.Rows == 40
	})

	if err := websocket.JSON.Send(ws, TerminalStreamMessage{Kind: "close"}); err != nil {
		t.Fatalf("send close: %v", err)
	}
	_ = ws.Close()
	requireEventually(t, func() bool {
		return len(getStatusFromServer(t, httpSrv.URL, "secret").Streams) == 0
	})
}

func TestEventStreamPublishesSessionEvents(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/vmsh/events", nil)
	if err != nil {
		t.Fatalf("new event request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	first := readEvent(t, scanner)
	if first.Kind != "connected" || first.ID == "" || first.At.IsZero() {
		t.Fatalf("connected event = %+v", first)
	}

	createReq, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/vmsh/sessions", bytes.NewBufferString(`{"name":"main"}`))
	if err != nil {
		t.Fatalf("new create request: %v", err)
	}
	createReq.Header.Set("Authorization", "Bearer secret")
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d", createResp.StatusCode)
	}

	event := readEvent(t, scanner)
	if event.Kind != "session_created" || event.ID == "" || event.Session == nil || event.Session.Name != "main" || event.Session.State != "detached" {
		t.Fatalf("session created event = %+v", event)
	}
}

func TestStatusReportsActiveEventStreams(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	httpSrv := httptest.NewServer(srv.Authenticate(mux))
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/vmsh/events", nil)
	if err != nil {
		t.Fatalf("new event request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	connected := readEvent(t, scanner)

	status := getStatusFromServer(t, httpSrv.URL, "secret")
	if len(status.Streams) != 1 {
		t.Fatalf("streams = %+v", status.Streams)
	}
	stream := status.Streams[0]
	if stream.ID == "" || stream.Kind != "events" || stream.State != "open" || stream.ConnectedAt.IsZero() || stream.LastEventID != connected.ID {
		t.Fatalf("stream summary = %+v, connected=%+v", stream, connected)
	}

	cancel()
	_ = resp.Body.Close()
	requireEventually(t, func() bool {
		return len(getStatusFromServer(t, httpSrv.URL, "secret").Streams) == 0
	})
}

func TestEventStreamRequiresAuth(t *testing.T) {
	srv := NewServer("secret")
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux, nil)
	handler := srv.Authenticate(mux)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vmsh/events", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("event stream status = %d", rr.Code)
	}
}

func getStatusFromServer(t *testing.T, baseURL, token string) Status {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/vmsh/status", nil)
	if err != nil {
		t.Fatalf("new status request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", resp.StatusCode)
	}
	var status Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return status
}

func getJobsFromServer(t *testing.T, baseURL, token string) []JobSummary {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/vmsh/jobs", nil)
	if err != nil {
		t.Fatalf("new jobs request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get jobs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jobs status = %d", resp.StatusCode)
	}
	var jobs []JobSummary
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	return jobs
}

func requireEventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func readEvent(t *testing.T, scanner *bufio.Scanner) Event {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("scan event: %v", scanner.Err())
	}
	var event Event
	if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
		t.Fatalf("decode event %q: %v", scanner.Text(), err)
	}
	return event
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
