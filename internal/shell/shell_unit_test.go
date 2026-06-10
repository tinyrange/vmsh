package shell

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"j5.nz/cc/client"
)

func TestShellCommandPassingBuildsGuestRunRequests(t *testing.T) {
	api := newRecordingShellAPI("alpine", "alpine@amd64")
	sh := newUnitShell(t, api)
	script := strings.Join([]string{
		"@alpine --vm work --arch amd64 --memory 2g --cpus 4 --no-network --nested --cwd /work --user app",
		"printf 'hello | %s' \"$USER\"",
		"@sudo whoami",
	}, "\n")

	stdout, stderr, err := runShellUnitScript(sh, script)
	if err != nil {
		t.Fatalf("run script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	if len(api.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(api.starts))
	}
	start := api.starts[0]
	if start.id != "work" {
		t.Fatalf("start id = %q, want work", start.id)
	}
	if start.req.Image != "alpine@amd64" || start.req.MemoryMB != 2048 || start.req.CPUs != 4 || !start.req.NestedVirt {
		t.Fatalf("start request = %+v", start.req)
	}
	if start.req.Network != nil {
		t.Fatalf("start network = %+v, want nil for --no-network", start.req.Network)
	}

	if len(api.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(api.runs))
	}
	first := api.runs[0]
	if first.id != "work" {
		t.Fatalf("first run id = %q, want work", first.id)
	}
	if first.req.Image != "alpine@amd64" || first.req.WorkDir != "/work" || first.req.User != "app" {
		t.Fatalf("first run context = %+v", first.req)
	}
	if first.req.MemoryMB != 2048 || first.req.CPUs != 4 || !first.req.NestedVirt {
		t.Fatalf("first run resources = %+v", first.req)
	}
	if first.req.Network != nil {
		t.Fatalf("first run network = %+v, want nil for --no-network", first.req.Network)
	}
	if len(first.req.Shares) != 1 || first.req.Shares[0].Mount != guestHostMount || !first.req.Shares[0].Writable || !first.req.Shares[0].MapOwner {
		t.Fatalf("first run shares = %+v", first.req.Shares)
	}
	if !strings.HasSuffix(first.req.Command[2], "printf 'hello | %s' \"$USER\"") {
		t.Fatalf("first run command = %#v", first.req.Command)
	}

	second := api.runs[1]
	if second.req.User != "root" {
		t.Fatalf("sudo run user = %q, want root", second.req.User)
	}
	if !strings.HasSuffix(second.req.Command[2], "whoami") {
		t.Fatalf("sudo command = %#v", second.req.Command)
	}
}

func TestGuestPersistentShellRestartsWhenIsolationChanges(t *testing.T) {
	api := newRecordingShellAPI("ubuntu")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "ubuntu"}
	sh := newUnitShell(t, api)

	var mu sync.Mutex
	var starts []client.RunRequest
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		mu.Lock()
		starts = append(starts, req)
		mu.Unlock()
		if onEvent != nil {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "__VMSH_READY__:" + req.WorkDir + "\n"}); err != nil {
				return err
			}
		}
		for input := range inputs {
			if input.Kind == "stdin_close" {
				return nil
			}
			if input.Kind == "stdin" && onEvent != nil {
				if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "__VMSH_DONE__:0:" + req.WorkDir + "\n"}); err != nil {
					return err
				}
			}
		}
		return nil
	}

	sharedCtx := commandContext{Mode: modeVM, VMID: "default", Image: "ubuntu", Network: true}
	sharedReq, err := sh.prepareGuestRunRequest(sharedCtx, ":", true, 80, 24, io.Discard)
	if err != nil {
		t.Fatalf("prepare shared run: %v", err)
	}
	shared, err := sh.guestPersistentShell(sharedCtx, sharedReq)
	if err != nil {
		t.Fatalf("start shared shell: %v", err)
	}

	isolatedCtx := sharedCtx
	isolatedCtx.Isolated = true
	isolatedReq, err := sh.prepareGuestRunRequest(isolatedCtx, ":", true, 80, 24, io.Discard)
	if err != nil {
		t.Fatalf("prepare isolated run: %v", err)
	}
	isolated, err := sh.guestPersistentShell(isolatedCtx, isolatedReq)
	if err != nil {
		t.Fatalf("start isolated shell: %v", err)
	}
	if isolated == shared {
		t.Fatalf("isolated shell reused shared persistent shell")
	}
	sh.closeSessions()

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 {
		t.Fatalf("persistent shell starts = %d, want 2", len(starts))
	}
	if len(starts[0].Shares) == 0 {
		t.Fatalf("shared shell started without host share")
	}
	if len(starts[1].Shares) != 0 {
		t.Fatalf("isolated shell shares = %+v, want none", starts[1].Shares)
	}
}

func TestIsolatedContextUsesSeparateBackendVM(t *testing.T) {
	api := newRecordingShellAPI("ubuntu")
	sh := newUnitShell(t, api)
	script := strings.Join([]string{
		"@ubuntu --vm work",
		"true",
		"@ubuntu --vm work --isolated",
		"true",
	}, "\n")

	stdout, stderr, err := runShellUnitScript(sh, script)
	if err != nil {
		t.Fatalf("run script: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if len(api.starts) != 2 {
		t.Fatalf("starts = %d, want 2", len(api.starts))
	}
	if api.starts[0].id != "work" {
		t.Fatalf("shared start id = %q, want work", api.starts[0].id)
	}
	if api.starts[0].req.Network == nil || api.starts[0].req.Network.BlockHostAccess {
		t.Fatalf("shared start network = %+v, want host access allowed", api.starts[0].req.Network)
	}
	if api.starts[1].id != "work-isolated" {
		t.Fatalf("isolated start id = %q, want work-isolated", api.starts[1].id)
	}
	if api.starts[1].req.Network == nil || !api.starts[1].req.Network.BlockHostAccess {
		t.Fatalf("isolated start network = %+v, want host access blocked", api.starts[1].req.Network)
	}
	if len(api.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(api.runs))
	}
	if api.runs[0].id != "work" || len(api.runs[0].req.Shares) == 0 {
		t.Fatalf("shared run = id %q shares %+v", api.runs[0].id, api.runs[0].req.Shares)
	}
	if api.runs[0].req.Network == nil || api.runs[0].req.Network.BlockHostAccess {
		t.Fatalf("shared run network = %+v, want host access allowed", api.runs[0].req.Network)
	}
	if api.runs[1].id != "work-isolated" || len(api.runs[1].req.Shares) != 0 {
		t.Fatalf("isolated run = id %q shares %+v", api.runs[1].id, api.runs[1].req.Shares)
	}
	if api.runs[1].req.Network == nil || !api.runs[1].req.Network.BlockHostAccess {
		t.Fatalf("isolated run network = %+v, want host access blocked", api.runs[1].req.Network)
	}
}

func TestIsolatedContextDoesNotInheritHostMappedCWD(t *testing.T) {
	api := newRecordingShellAPI("ubuntu")
	sh := newUnitShell(t, api)
	sh.context = commandContext{
		Mode:    modeVM,
		VMID:    "work",
		Image:   "ubuntu",
		CWD:     path.Join(guestHostMount, "Users/example/project"),
		Network: true,
	}

	var stdout, stderr bytes.Buffer
	if err := sh.eval("@ubuntu --isolated", &stdout, &stderr); err != nil {
		t.Fatalf("switch to isolated context: %v\nstderr:\n%s", err, stderr.String())
	}
	if sh.context.CWD != "" {
		t.Fatalf("isolated context cwd = %q, want empty until guest run resolves home", sh.context.CWD)
	}
	req, err := sh.prepareGuestRunRequest(sh.context, "pwd", false, 0, 0, io.Discard)
	if err != nil {
		t.Fatalf("prepare isolated run: %v", err)
	}
	if strings.HasPrefix(req.WorkDir, guestHostMount+"/") || req.WorkDir == guestHostMount {
		t.Fatalf("isolated workdir = %q, want non-host path", req.WorkDir)
	}
}

func TestSudoAliasExpandsAcrossVMShCommandLists(t *testing.T) {
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "alpine", Network: true}

	var stdout, stderr bytes.Buffer
	if err := sh.eval("@alias sudo=@sudo", &stdout, &stderr); err != nil {
		t.Fatalf("set sudo alias: %v", err)
	}
	if err := sh.eval("sudo first && sudo second", &stdout, &stderr); err != nil {
		t.Fatalf("run sudo alias command list: %v\nstderr:\n%s", err, stderr.String())
	}
	if len(api.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(api.runs))
	}
	for i, run := range api.runs {
		if run.req.User != "root" {
			t.Fatalf("run %d user = %q, want root", i, run.req.User)
		}
	}
	if !strings.Contains(api.runs[0].req.Command[2], "first") || !strings.Contains(api.runs[1].req.Command[2], "second") {
		t.Fatalf("commands = %#v, %#v", api.runs[0].req.Command, api.runs[1].req.Command)
	}

	api.runs = nil
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		code := 0
		if len(api.runs) == 1 {
			code = 1
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: code})
	}
	if err := sh.eval("sudo false && sudo skipped", &stdout, &stderr); err != nil {
		t.Fatalf("run short-circuit sudo alias command list: %v", err)
	}
	if len(api.runs) != 1 {
		t.Fatalf("short-circuit runs = %d, want 1", len(api.runs))
	}
	if sh.lastCode != 1 {
		t.Fatalf("lastCode = %d, want 1", sh.lastCode)
	}
}

func TestAgentCodexUsesGuestReleaseWithoutChangingGlobalCurrent(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"token":"host"}`), 0o600); err != nil {
		t.Fatalf("write host auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "state_5.sqlite"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatalf("write host state db: %v", err)
	}
	certDir := t.TempDir()
	certFile := filepath.Join(certDir, "ca.pem")
	if err := os.WriteFile(certFile, []byte("fake certs\n"), 0o644); err != nil {
		t.Fatalf("write fake CA bundle: %v", err)
	}
	t.Setenv("SSL_CERT_FILE", certFile)
	linuxRelease := makeFakeCodexRelease(t, codexHome, "9.8.7", "x86_64-unknown-linux-musl")
	darwinRelease := makeFakeCodexRelease(t, codexHome, "9.9.9", "aarch64-apple-darwin")
	currentLink := filepath.Join(codexHome, "packages", "standalone", "current")
	if err := os.Symlink(darwinRelease, currentLink); err != nil {
		t.Fatalf("create global current symlink: %v", err)
	}

	api := newRecordingShellAPI("ubuntu")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "ubuntu"}
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if !strings.Contains(req.Command[2], "uname -s") {
			t.Fatalf("unexpected non-interactive run command: %#v", req.Command)
		}
		if onEvent != nil {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "Linux\nx86_64\n"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	var agentRun client.RunRequest
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		agentRun = req
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "ubuntu"}

	var stdout, stderr bytes.Buffer
	if err := sh.eval("@agent codex --version", &stdout, &stderr); err != nil {
		t.Fatalf("run @agent codex: %v\nstderr:\n%s", err, stderr.String())
	}
	if len(api.runs) != 1 {
		t.Fatalf("platform probe runs = %d, want 1", len(api.runs))
	}
	if !agentRun.TTY || agentRun.Cols != 80 || agentRun.Rows != 24 {
		t.Fatalf("agent TTY = %t cols=%d rows=%d", agentRun.TTY, agentRun.Cols, agentRun.Rows)
	}
	if agentRun.Network == nil || !agentRun.Network.AllowInternet {
		t.Fatalf("agent network = %+v, want internet-enabled network", agentRun.Network)
	}
	_, wantWorkDir, err := guestHostPaths(sh.hostCWD)
	if err != nil {
		t.Fatalf("guest host paths: %v", err)
	}
	if agentRun.WorkDir != wantWorkDir {
		t.Fatalf("agent workdir = %q, want %q", agentRun.WorkDir, wantWorkDir)
	}
	if !hasString(agentRun.Env, "CODEX_HOME="+codexGuestHomeMount) {
		t.Fatalf("agent env = %#v, want CODEX_HOME", agentRun.Env)
	}
	if !hasString(agentRun.Env, "SSL_CERT_FILE="+path.Join(codexGuestCertMount, "ca.pem")) {
		t.Fatalf("agent env = %#v, want SSL_CERT_FILE", agentRun.Env)
	}
	var codexShare client.ShareMount
	var standaloneShare client.ShareMount
	var certShare client.ShareMount
	for _, share := range agentRun.Shares {
		if share.Mount == codexGuestHomeMount {
			codexShare = share
		}
		if share.Mount == codexGuestStandaloneMount {
			standaloneShare = share
		}
		if share.Mount == codexGuestCertMount {
			certShare = share
		}
	}
	if codexShare.Source == "" || codexShare.Source == codexHome || !codexShare.Writable || !codexShare.MapOwner {
		t.Fatalf("codex share = %+v", codexShare)
	}
	if !strings.HasPrefix(codexShare.Source, filepath.Join(codexHome, filepath.FromSlash(codexStandaloneDir), "vmsh", "agent-homes")+string(filepath.Separator)) {
		t.Fatalf("codex share source = %q, want vmsh-managed agent home", codexShare.Source)
	}
	if _, err := os.Stat(filepath.Join(codexShare.Source, "auth.json")); err != nil {
		t.Fatalf("agent auth was not seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexShare.Source, "state_5.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("agent state db stat err = %v, want not copied from host", err)
	}
	if standaloneShare.Source != filepath.Join(codexHome, filepath.FromSlash(codexStandaloneDir)) || standaloneShare.Writable {
		t.Fatalf("standalone share = %+v", standaloneShare)
	}
	if certShare.Source != certDir || certShare.Writable {
		t.Fatalf("cert share = %+v", certShare)
	}
	wantGuestBin := path.Join(codexGuestStandaloneMount, "releases", filepath.Base(linuxRelease), "bin/codex")
	if !strings.Contains(agentRun.Command[2], wantGuestBin) || !strings.Contains(agentRun.Command[2], "--version") {
		t.Fatalf("agent command = %#v, want guest binary %s and --version", agentRun.Command, wantGuestBin)
	}
	if !strings.Contains(agentRun.Command[2], path.Join(codexGuestStandaloneMount, "releases", filepath.Base(linuxRelease), "codex-resources")) {
		t.Fatalf("agent command = %#v, want bundled Codex resources on PATH", agentRun.Command)
	}
	if strings.Contains(agentRun.Command[2], "/current/") {
		t.Fatalf("agent command should not use global current symlink: %#v", agentRun.Command)
	}
	link, err := os.Readlink(currentLink)
	if err != nil {
		t.Fatalf("read global current symlink: %v", err)
	}
	if link != darwinRelease {
		t.Fatalf("global current = %q, want unchanged %q", link, darwinRelease)
	}
}

func TestAgentCodexProxyUsesHostAuthProxyWithoutCodexHomeMount(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"auth_mode":"api-key","OPENAI_API_KEY":"sk-test"}`), 0o600); err != nil {
		t.Fatalf("write host auth: %v", err)
	}
	linuxRelease := makeFakeCodexRelease(t, codexHome, "9.8.7", "x86_64-unknown-linux-musl")

	api := newRecordingShellAPI("ubuntu")
	api.instances["default-isolated"] = client.InstanceState{ID: "default-isolated", Status: "running", Image: "ubuntu"}
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		if onEvent != nil {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "Linux\nx86_64\n"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	var agentRun client.RunRequest
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		agentRun = req
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "ubuntu", Isolated: true}

	var stdout, stderr bytes.Buffer
	if err := sh.eval("@agent --proxy codex --version", &stdout, &stderr); err != nil {
		t.Fatalf("run proxied @agent codex: %v\nstderr:\n%s", err, stderr.String())
	}
	if agentRun.Network == nil || !agentRun.Network.BlockHostAccess || len(agentRun.Network.AllowedServiceProxyPorts) != 1 {
		t.Fatalf("agent network = %+v, want isolated network with one service proxy port", agentRun.Network)
	}
	proxyPort := agentRun.Network.AllowedServiceProxyPorts[0]
	if len(api.servicePorts) != 1 || api.servicePorts[0].id != "default-isolated" || api.servicePorts[0].port != proxyPort {
		t.Fatalf("service proxy port updates = %+v, want default-isolated port %d", api.servicePorts, proxyPort)
	}
	wantProxyHome := codexGuestProxyHomeDir(sh.context)
	if !hasString(agentRun.Env, "CODEX_HOME="+wantProxyHome) {
		t.Fatalf("agent env = %#v, want proxy CODEX_HOME", agentRun.Env)
	}
	var tokenValue string
	for _, env := range agentRun.Env {
		if strings.HasPrefix(env, codexAgentProxyTokenEnv+"=") {
			tokenValue = strings.TrimPrefix(env, codexAgentProxyTokenEnv+"=")
		}
	}
	if tokenValue == "" {
		t.Fatalf("agent env = %#v, want proxy token", agentRun.Env)
	}
	for _, share := range agentRun.Shares {
		if share.Mount == codexGuestHomeMount {
			t.Fatalf("proxy agent mounted Codex home: %+v", share)
		}
		if share.Source == filepath.Join(codexHome, filepath.FromSlash(codexStandaloneDir)) {
			t.Fatalf("proxy agent mounted whole standalone root: %+v", share)
		}
	}
	wantReleaseMount := path.Join(codexGuestStandaloneMount, "releases", filepath.Base(linuxRelease))
	foundRelease := false
	for _, share := range agentRun.Shares {
		if share.Mount == wantReleaseMount {
			foundRelease = true
			if share.Source != linuxRelease || share.Writable {
				t.Fatalf("release share = %+v", share)
			}
		}
	}
	if !foundRelease {
		t.Fatalf("shares = %+v, want release mount %s", agentRun.Shares, wantReleaseMount)
	}
	command := agentRun.Command[2]
	for _, want := range []string{
		fmt.Sprintf("base_url = \"http://10.42.0.100:%d/v1\"", proxyPort),
		"requires_openai_auth = false",
		"\"" + codexAgentProxyTokenHeader + "\" = \"" + codexAgentProxyTokenEnv + "\"",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("agent command = %q, want %q", command, want)
		}
	}
	if !strings.Contains(command, path.Join(wantReleaseMount, "bin/codex")) || !strings.Contains(command, "--version") {
		t.Fatalf("agent command = %q, want guest binary and --version", command)
	}
	for _, forbidden := range []string{codexGuestHomeMount, "auth.json"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("agent command = %q, should not contain %q", command, forbidden)
		}
	}
}

func TestCodexAgentProxyForwardsWithHostAuthHeaders(t *testing.T) {
	var gotPath, gotAuth, gotGuestToken, gotAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		gotGuestToken = r.Header.Get(codexAgentProxyTokenHeader)
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-host" {
			t.Fatalf("ChatGPT-Account-ID = %q, want acct-host", got)
		}
		if got := r.Header.Get("X-OpenAI-Fedramp"); got != "true" {
			t.Fatalf("X-OpenAI-Fedramp = %q, want true", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy := &codexAgentProxy{}
	req := httptest.NewRequest(http.MethodPost, "http://guest/v1/responses?debug=1", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer guest-token")
	req.Header.Set(codexAgentProxyTokenHeader, "guest-visible-token")
	req.Header.Set("Accept-Encoding", "gzip, br, zstd")
	resp, err := proxy.forward(req, []byte(`{"input":"hello"}`), codexAgentProxyAuth{
		bearer:       "host-token",
		accountID:    "acct-host",
		fedRamp:      true,
		upstreamBase: upstream.URL,
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
	if gotPath != "/responses?debug=1" {
		t.Fatalf("upstream path = %q, want /responses?debug=1", gotPath)
	}
	if gotAuth != "Bearer host-token" {
		t.Fatalf("Authorization = %q, want host token", gotAuth)
	}
	if gotGuestToken != "" {
		t.Fatalf("guest proxy token was forwarded: %q", gotGuestToken)
	}
	if gotAcceptEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotAcceptEncoding)
	}
}

func TestCodexAgentProxyServeHTTPStreamsResponsesWithoutContentLength(t *testing.T) {
	var gotAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("upstream recorder does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: alpha\n\n")
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, "data: omega\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	oldOpenAIUpstream := codexAgentProxyOpenAIUpstream
	codexAgentProxyOpenAIUpstream = upstream.URL
	t.Cleanup(func() {
		codexAgentProxyOpenAIUpstream = oldOpenAIUpstream
	})

	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"auth_mode":"api-key","OPENAI_API_KEY":"host-key"}`), 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	proxy := &codexAgentProxy{
		token: "guest-token",
		auth:  &codexAgentProxyAuthStore{path: filepath.Join(codexHome, "auth.json"), now: time.Now},
	}
	req := httptest.NewRequest(http.MethodPost, "http://guest/v1/responses", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set(codexAgentProxyTokenHeader, "guest-token")
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	if gotAcceptEncoding != "identity" {
		t.Fatalf("upstream Accept-Encoding = %q, want identity", gotAcceptEncoding)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("response Content-Length = %q, want omitted for stream", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "data: alpha") || !strings.Contains(body, "data: omega") {
		t.Fatalf("stream body = %q", body)
	}
}

func TestCodexAgentProxyPrefersChatGPTTokensWhenAuthModeIsChatGPT(t *testing.T) {
	idToken := testCodexJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-chatgpt",
		},
	})
	auth, err := (codexAgentProxyAuthFile{
		AuthMode:     "chatgpt",
		OpenAIAPIKey: "sk-should-not-win",
		Tokens: &codexAgentProxyTokenData{
			IDToken:      json.RawMessage(strconv.Quote(idToken)),
			AccessToken:  "chatgpt-access",
			RefreshToken: "chatgpt-refresh",
		},
	}).proxyAuth()
	if err != nil {
		t.Fatalf("proxy auth: %v", err)
	}
	if auth.upstreamBase != codexAgentProxyChatGPTBase || auth.bearer != "chatgpt-access" || auth.accountID != "acct-chatgpt" {
		t.Fatalf("auth = %+v, want ChatGPT token-backed auth", auth)
	}
}

func TestCodexAgentProxyRefreshesExpiredChatGPTToken(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	oldAccess := testCodexJWT(t, map[string]any{"exp": now.Add(-time.Hour).Unix()})
	newIDToken := testCodexJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":         "acct-new",
			"chatgpt_account_is_fedramp": true,
		},
	})

	codexHome := t.TempDir()
	authPath := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(authPath, []byte(fmt.Sprintf(`{
  "auth_mode": "chatgpt",
  "tokens": {
    "id_token": %q,
    "access_token": %q,
    "refresh_token": "old-refresh"
  },
  "last_refresh": %q
}`, testCodexJWT(t, map[string]any{}), oldAccess, now.Add(-9*24*time.Hour).Format(time.RFC3339Nano))), 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body codexAgentProxyRefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode refresh request: %v", err)
		}
		if body.ClientID != codexAgentProxyClientID || body.GrantType != "refresh_token" || body.RefreshToken != "old-refresh" {
			t.Fatalf("refresh body = %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id_token":%q,"access_token":"new-access","refresh_token":"new-refresh"}`, newIDToken)
	}))
	defer refreshServer.Close()
	t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", refreshServer.URL)

	store := &codexAgentProxyAuthStore{
		path: authPath,
		now:  func() time.Time { return now },
	}
	auth, err := store.auth(context.Background(), true)
	if err != nil {
		t.Fatalf("proxy auth: %v", err)
	}
	if auth.bearer != "new-access" || auth.accountID != "acct-new" || !auth.fedRamp {
		t.Fatalf("auth = %+v, want refreshed ChatGPT auth", auth)
	}
	var saved codexAgentProxyAuthFile
	data, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("decode saved auth: %v", err)
	}
	if saved.Tokens == nil || saved.Tokens.AccessToken != "new-access" || saved.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("saved tokens = %+v", saved.Tokens)
	}
	if saved.LastRefresh != now.Format(time.RFC3339Nano) {
		t.Fatalf("last_refresh = %q, want %q", saved.LastRefresh, now.Format(time.RFC3339Nano))
	}
}

func TestAgentCodexNoInstallReportsMissingGuestTarget(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	api := newRecordingShellAPI("ubuntu")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "ubuntu"}
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		if onEvent != nil {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "Linux\naarch64\n"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		t.Fatalf("@agent codex should not start without a guest release when --no-install is set")
		return nil
	}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "ubuntu"}

	var stdout, stderr bytes.Buffer
	err := sh.eval("@agent codex --no-install", &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "aarch64-unknown-linux-musl") {
		t.Fatalf("error = %v, want missing aarch64 target", err)
	}
}

func TestAgentCodexSudoRunsAsRoot(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	linuxRelease := makeFakeCodexRelease(t, codexHome, "9.8.7", "x86_64-unknown-linux-musl")
	api := newRecordingShellAPI("ubuntu")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "ubuntu"}
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent != nil {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "Linux\nx86_64\n"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	var agentRun client.RunRequest
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		agentRun = req
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "ubuntu"}

	var stdout, stderr bytes.Buffer
	if err := sh.eval("@agent --sudo codex --version", &stdout, &stderr); err != nil {
		t.Fatalf("run sudo @agent codex: %v\nstderr:\n%s", err, stderr.String())
	}
	if agentRun.User != "root" {
		t.Fatalf("agent user = %q, want root", agentRun.User)
	}
	for _, want := range []string{"HOME=/root", "USER=root", "LOGNAME=root", "CODEX_HOME=" + codexGuestHomeMount} {
		if !hasString(agentRun.Env, want) {
			t.Fatalf("agent env = %#v, want %s", agentRun.Env, want)
		}
	}
	releaseName := filepath.Base(linuxRelease)
	wantMountedRelease := path.Join(codexGuestStandaloneMount, "releases", releaseName)
	wantStagedBin := path.Join("/run/vmsh-codex", releaseName, "bin/codex")
	if !strings.Contains(agentRun.Command[2], wantMountedRelease) || !strings.Contains(agentRun.Command[2], wantStagedBin) || !strings.Contains(agentRun.Command[2], "--version") {
		t.Fatalf("agent command = %#v, want mounted release %s, staged binary %s, and --version", agentRun.Command, wantMountedRelease, wantStagedBin)
	}
}

func TestPrepareCodexAgentHomeSeedsOnlySafeCodexData(t *testing.T) {
	codexHome := t.TempDir()
	for name, data := range map[string]string{
		"auth.json":           `{"token":"host"}`,
		"config.toml":         "model = \"gpt-5.5\"\n",
		"session_index.jsonl": "{}\n",
		"state_5.sqlite":      "not sqlite",
		"goals_1.sqlite":      "sqlite data",
	} {
		if err := os.WriteFile(filepath.Join(codexHome, name), []byte(data), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(codexHome, "sessions", "2026"), 0o700); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "sessions", "2026", "session.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	ctx := commandContext{Mode: modeVM, VMID: "work", Image: "ubuntu", User: defaultGuestUser}
	agentHome, err := prepareCodexAgentHome(codexHome, ctx, "x86_64-unknown-linux-musl")
	if err != nil {
		t.Fatalf("prepare agent home: %v", err)
	}
	for _, name := range []string{"auth.json", "config.toml", "session_index.jsonl", filepath.Join("sessions", "2026", "session.jsonl")} {
		if _, err := os.Stat(filepath.Join(agentHome, name)); err != nil {
			t.Fatalf("seeded %s stat: %v", name, err)
		}
	}
	for _, name := range []string{"state_5.sqlite", "goals_1.sqlite"} {
		if _, err := os.Stat(filepath.Join(agentHome, name)); !os.IsNotExist(err) {
			t.Fatalf("mutable db %s stat err = %v, want not copied", name, err)
		}
	}
	link, err := os.Readlink(filepath.Join(agentHome, "packages", "standalone"))
	if err != nil {
		t.Fatalf("read standalone symlink: %v", err)
	}
	if link != codexGuestStandaloneMount {
		t.Fatalf("standalone symlink = %q, want %q", link, codexGuestStandaloneMount)
	}
}

func TestTrustCodexAgentProjectAppendsPrivateProjectTrust(t *testing.T) {
	agentHome := t.TempDir()
	configPath := filepath.Join(agentHome, "config.toml")
	if err := os.WriteFile(configPath, []byte("model = \"gpt-5.5\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := trustCodexAgentProject(agentHome, "/root"); err != nil {
		t.Fatalf("trust project: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "[projects.\"/root\"]\ntrust_level = \"trusted\"") {
		t.Fatalf("config = %q, want trusted root project", string(data))
	}
	if err := trustCodexAgentProject(agentHome, "/root"); err != nil {
		t.Fatalf("trust project again: %v", err)
	}
	data, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config again: %v", err)
	}
	if strings.Count(string(data), "[projects.\"/root\"]") != 1 {
		t.Fatalf("config = %q, want one root project table", string(data))
	}
}

func TestEnsureHostCodexReleaseDownloadsLatestForMissingGuestTarget(t *testing.T) {
	codexHome := t.TempDir()
	target := "x86_64-unknown-linux-musl"
	assetName := "codex-package-" + target + ".tar.gz"
	archiveBytes := fakeCodexPackageArchive(t)
	archiveDigest := sha256Hex(archiveBytes)
	checksumBytes := []byte(archiveDigest + "  " + assetName + "\n")
	checksumDigest := sha256Hex(checksumBytes)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/openai/codex/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"tag_name": "rust-v1.2.3",
				"assets": [
					{"name": %q, "browser_download_url": %q, "digest": %q},
					{"name": "codex-package_SHA256SUMS", "browser_download_url": %q, "digest": %q}
				]
			}`, assetName, server.URL+"/download/"+assetName, "sha256:"+archiveDigest, server.URL+"/download/codex-package_SHA256SUMS", "sha256:"+checksumDigest)
		case "/download/" + assetName:
			_, _ = w.Write(archiveBytes)
		case "/download/codex-package_SHA256SUMS":
			_, _ = w.Write(checksumBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	oldAPI := codexGitHubAPI
	oldClient := codexHTTPClient
	codexGitHubAPI = server.URL + "/repos/openai/codex"
	codexHTTPClient = server.Client()
	t.Cleanup(func() {
		codexGitHubAPI = oldAPI
		codexHTTPClient = oldClient
	})

	release, err := ensureHostCodexRelease(codexHome, target, codexAgentOptions{Release: "latest"}, io.Discard)
	if err != nil {
		t.Fatalf("ensure host Codex release: %v", err)
	}
	if release.Version != "1.2.3" || release.Target != target {
		t.Fatalf("release = %+v", release)
	}
	if !isExecutable(filepath.Join(release.ReleaseDir, "bin", "codex")) {
		t.Fatalf("installed codex binary is not executable")
	}
	if _, err := os.Lstat(filepath.Join(codexHome, "packages", "standalone", "current")); !os.IsNotExist(err) {
		t.Fatalf("global current symlink err = %v, want not created", err)
	}
	if release.CodexGuestBin != path.Join(codexGuestStandaloneMount, "releases", "1.2.3-"+target, "bin/codex") {
		t.Fatalf("guest binary = %q", release.CodexGuestBin)
	}
}

func TestCodexGuestTargetMapsLinuxArchitectures(t *testing.T) {
	tests := []struct {
		osName  string
		machine string
		want    string
		wantErr bool
	}{
		{osName: "Linux", machine: "aarch64", want: "aarch64-unknown-linux-musl"},
		{osName: "Linux", machine: "arm64", want: "aarch64-unknown-linux-musl"},
		{osName: "Linux", machine: "x86_64", want: "x86_64-unknown-linux-musl"},
		{osName: "Darwin", machine: "arm64", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.osName+"/"+tt.machine, func(t *testing.T) {
			got, err := codexGuestTarget(tt.osName, tt.machine)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("codexGuestTarget returned nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("codexGuestTarget: %v", err)
			}
			if got != tt.want {
				t.Fatalf("target = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompletionsUseCachedImagesOptionsAndHostMappedPaths(t *testing.T) {
	api := newRecordingShellAPI("alpine", "ubuntu")
	sh := newUnitShell(t, api)
	for _, dir := range []string{
		filepath.Join(sh.rootCache, "images", "alpine"),
		filepath.Join(sh.rootCache, "images", "ubuntu"),
		filepath.Join(sh.rootCache, "images", "blobs"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create image cache dir: %v", err)
		}
	}
	if err := os.Mkdir(filepath.Join(sh.hostCWD, "alpha dir"), 0o755); err != nil {
		t.Fatalf("create host dir: %v", err)
	}

	c := newVMSHCompleter(sh)
	candidates, replaceLen, kind := c.CompleteWithKind([]rune("@al"), len("@al"))
	if kind != completionAt || replaceLen != len("@al") || !hasString(candidates, "pine") {
		t.Fatalf("@ image completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
	if hasString(candidates, "blobs") {
		t.Fatalf("internal image cache dir completed: %q", candidates)
	}

	candidates, replaceLen, kind = c.CompleteWithKind([]rune("@alpine --n"), len("@alpine --n"))
	if kind != completionOption || replaceLen != len("--n") || !hasString(candidates, "etwork") || !hasString(candidates, "ested") {
		t.Fatalf("option completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
	candidates, replaceLen, kind = c.CompleteWithKind([]rune("@agent --pr"), len("@agent --pr"))
	if kind != completionOption || replaceLen != len("--pr") || !hasString(candidates, "oxy") {
		t.Fatalf("agent option completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
	candidates, _, _ = c.CompleteWithKind([]rune("@alpine --pr"), len("@alpine --pr"))
	if hasString(candidates, "oxy") {
		t.Fatalf("non-agent option completion included proxy: %q", candidates)
	}

	candidates, replaceLen, kind = c.CompleteWithKind([]rune("@rmi al"), len("@rmi al"))
	if kind != completionAt || replaceLen != len("al") || !reflect.DeepEqual(candidates, []string{"pine"}) {
		t.Fatalf("@rmi completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}

	_, guestCWD, err := guestHostPaths(sh.hostCWD)
	if err != nil {
		t.Fatalf("guest host paths: %v", err)
	}
	sh.context = commandContext{Mode: modeVM, VMID: "vm", Image: "alpine", CWD: guestCWD}
	candidates, replaceLen, kind = c.CompleteWithKind([]rune("cat a"), len("cat a"))
	if kind != completionPath || replaceLen != len("a") || !hasString(candidates, "lpha\\ dir/") {
		t.Fatalf("host-mapped path completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
}

func TestCompletionsUseCurrentCommandSegmentAndGuestCommands(t *testing.T) {
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "vmtool\n"}); err != nil {
			return err
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "alpine"}
	c := newVMSHCompleter(sh)

	line := []rune("echo ok && vm")
	candidates, replaceLen, kind := c.CompleteWithKind(line, len(line))
	if kind != completionCommand || replaceLen != len("vm") || !hasString(candidates, "tool") {
		t.Fatalf("guest command completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
	if len(api.runs) != 1 || api.runs[0].id != "default" {
		t.Fatalf("guest command completion runs = %+v", api.runs)
	}

	line = []rune("printf x | @host ec")
	candidates, replaceLen, kind = c.CompleteWithKind(line, len(line))
	if kind != completionCommand || replaceLen != len("ec") || !hasString(candidates, "ho") {
		t.Fatalf("host command completion candidates=%q replace=%d kind=%q", candidates, replaceLen, kind)
	}
}

func TestPromptCWDColorDistinguishesContextStorage(t *testing.T) {
	sh := newUnitShell(t, newRecordingShellAPI("alpine"))
	sh.context = commandContext{Mode: modeHost}
	if got := sh.promptCWDColor(sh.hostCWD); got != colorCyan {
		t.Fatalf("host cwd color = %q", got)
	}

	sh.context = commandContext{Mode: modeVM, Image: "alpine"}
	if got := sh.promptCWDColor(guestHostMount + "/tmp"); got != colorCyan {
		t.Fatalf("shared cwd color = %q", got)
	}
	if got := sh.promptCWDColor("/tmp"); got != colorYellow {
		t.Fatalf("guest-local cwd color = %q", got)
	}

	sh.context.Isolated = true
	if got := sh.promptCWDColor("/tmp"); got != colorMagenta {
		t.Fatalf("isolated cwd color = %q", got)
	}
}

func TestHostCommandInterruptIsNotFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host interrupt test uses POSIX shell commands")
	}
	session, err := startPersistentHostShell(t.TempDir(), nil, 80, 24, "")
	if err != nil {
		t.Fatalf("start persistent host shell: %v", err)
	}
	t.Cleanup(session.close)

	var interrupted atomic.Bool
	errCh := make(chan error, 1)
	go func() {
		errCh <- session.run("sleep 30", io.Discard, io.Discard, func() (func(), error) {
			go func() {
				time.Sleep(100 * time.Millisecond)
				interrupted.Store(true)
				_, _ = session.tty.Write([]byte{0x03})
			}()
			return func() {}, nil
		})
	}()

	select {
	case err := <-errCh:
		if interrupted.Load() && err != nil && sessionLastCode(err) < 0 {
			err = persistentShellExit{code: 130}
		}
		if sessionLastCode(err) != 130 {
			t.Fatalf("interrupted host command error = %v, code %d; want 130", err, sessionLastCode(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("interrupted host command did not return")
	}
}

func TestPersistentHostShellRunsShortCommandsAndPipelines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent host shell requires a Unix PTY")
	}
	sh := newUnitShell(t, newRecordingShellAPI())

	if err := sh.runHost("printf short > short.txt", io.Discard, io.Discard); err != nil {
		t.Fatalf("run short command: %v", err)
	}
	shortData, err := os.ReadFile(filepath.Join(sh.hostCWD, "short.txt"))
	if err != nil {
		t.Fatalf("read short command output: %v", err)
	}
	if string(shortData) != "short" {
		t.Fatalf("short command wrote %q", string(shortData))
	}

	if err := sh.runHost(`printf 'alpha\nbeta\n' | grep beta > pipeline.txt`, io.Discard, io.Discard); err != nil {
		t.Fatalf("run pipeline command: %v", err)
	}
	pipelineData, err := os.ReadFile(filepath.Join(sh.hostCWD, "pipeline.txt"))
	if err != nil {
		t.Fatalf("read pipeline output: %v", err)
	}
	if string(pipelineData) != "beta\n" {
		t.Fatalf("pipeline command wrote %q", string(pipelineData))
	}
}

func TestPersistentHostShellCanReadForwardedInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent host shell requires a Unix PTY")
	}
	dir := t.TempDir()
	session, err := startPersistentHostShell(dir, nil, 80, 24, "")
	if err != nil {
		t.Fatalf("start persistent host shell: %v", err)
	}
	t.Cleanup(session.close)

	err = session.run("read value; printf '%s' \"$value\" > input.txt", io.Discard, io.Discard, func() (func(), error) {
		go func() {
			time.Sleep(50 * time.Millisecond)
			_, _ = session.tty.Write([]byte("from-stdin\n"))
		}()
		return func() {}, nil
	})
	if err != nil {
		t.Fatalf("run persistent command with forwarded input: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "input.txt"))
	if err != nil {
		t.Fatalf("read forwarded input output: %v", err)
	}
	if string(data) != "from-stdin" {
		t.Fatalf("forwarded input wrote %q", string(data))
	}
}

func TestPersistentHostShellStreamsPartialOutputBeforeCompletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent host shell requires a Unix PTY")
	}
	session, err := startPersistentHostShell(t.TempDir(), nil, 80, 24, "")
	if err != nil {
		t.Fatalf("start persistent host shell: %v", err)
	}
	t.Cleanup(session.close)

	stdout := newNotifyWriter("partial")
	errCh := make(chan error, 1)
	go func() {
		errCh <- session.run(`printf '\160\141\162\164\151\141\154'; sleep 1; printf done`, stdout, io.Discard, nil)
	}()

	select {
	case <-stdout.seen:
	case err := <-errCh:
		t.Fatalf("command returned before streaming partial output: %v", err)
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("partial output was not streamed before command completion")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("persistent command failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("persistent command did not finish")
	}
	if !strings.Contains(stdout.String(), "partialdone") {
		t.Fatalf("streamed output = %q", stdout.String())
	}
}

func TestPersistentHostShellMarkerScannerHandlesSplitMarker(t *testing.T) {
	p := &persistentHostShell{}
	out, _, _, done := p.consumeOutputChunk("frame")
	if out != "frame" || done {
		t.Fatalf("first chunk out=%q done=%t", out, done)
	}
	out, _, _, done = p.consumeOutputChunk("__VMSH_")
	if out != "" || done {
		t.Fatalf("marker prefix chunk out=%q done=%t", out, done)
	}
	out, code, cwd, done := p.consumeOutputChunk("DONE__:7:/tmp\r\n")
	if out != "" || !done || code != 7 || cwd != "/tmp" {
		t.Fatalf("marker completion out=%q done=%t code=%d cwd=%q", out, done, code, cwd)
	}
}

func TestPipelineParsingHandlesShellOperatorsAndQuotedPipes(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantOK  bool
		want    []string
		wantErr string
	}{
		{
			name:   "quoted pipes split only at real pipeline",
			line:   `printf 'a|b' | grep 'a|b' | wc -l`,
			wantOK: true,
			want:   []string{`printf 'a|b'`, `grep 'a|b'`, `wc -l`},
		},
		{
			name:   "double pipe is shell operator",
			line:   `false || printf fallback`,
			wantOK: false,
		},
		{
			name:   "escaped pipe is literal",
			line:   `printf \|`,
			wantOK: false,
		},
		{
			name:    "empty segment",
			line:    `printf x | | cat`,
			wantOK:  true,
			wantErr: "pipeline segment is empty",
		},
		{
			name:   "unfinished quote without pipeline is normal shell input",
			line:   `printf 'unterminated`,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := splitPipelineLine(tt.line)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				if ok != tt.wantOK {
					t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
				}
				return
			}
			if err != nil {
				t.Fatalf("split pipeline: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("segments = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestHostPipelineExecutionHandlesQuotedPipesAndShellOr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host pipeline test uses POSIX shell commands")
	}
	sh := newUnitShell(t, newRecordingShellAPI())
	var stdout, stderr bytes.Buffer

	err := sh.eval(`printf 'a|b\nskip\n' | grep 'a|b' | wc -l`, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run quoted host pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "1" {
		t.Fatalf("pipeline stdout = %q, want line count 1", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	err = sh.eval(`false || printf fallback`, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run shell OR command: %v\nstderr:\n%s", err, stderr.String())
	}
	if stdout.String() != "fallback" {
		t.Fatalf("OR stdout = %q, want fallback", stdout.String())
	}
}

func TestPlainGuestPipelineRunsInsideGuestShell(t *testing.T) {
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		t.Fatalf("plain guest pipeline used vmsh streaming path")
		return nil
	}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "alpine", Network: true}

	var stdout, stderr bytes.Buffer
	if err := sh.eval(`printf 'alpha\nbeta\n' | grep beta`, &stdout, &stderr); err != nil {
		t.Fatalf("run plain guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if len(api.runs) != 1 {
		t.Fatalf("guest runs = %d, want one shell command", len(api.runs))
	}
	if !strings.Contains(api.runs[0].req.Command[2], "| grep beta") {
		t.Fatalf("guest command = %#v", api.runs[0].req.Command)
	}
}

func TestMixedPipelineStreamsHostInputToGuestAndGuestOutputToHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mixed pipeline test uses POSIX host commands")
	}
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	guestStdin := make(chan []byte, 1)
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		data, closeEvents := drainExecInputStream(inputs)
		if closeEvents != 0 {
			t.Fatalf("pipeline input sent explicit stdin_close events = %d, want channel EOF", closeEvents)
		}
		guestStdin <- data
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	sh := newUnitShell(t, api)

	var stdout, stderr bytes.Buffer
	if err := sh.eval(`printf guest-data | @alpine cat`, &stdout, &stderr); err != nil {
		t.Fatalf("run host-to-guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	select {
	case stdin := <-guestStdin:
		if string(stdin) != "guest-data" {
			t.Fatalf("guest stdin = %q, want guest-data", string(stdin))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("guest stdin was not drained")
	}

	stdout.Reset()
	stderr.Reset()
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "guest-output\nother\n"}); err != nil {
			return err
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	if err := sh.eval(`@alpine printf ignored | @host grep guest-output`, &stdout, &stderr); err != nil {
		t.Fatalf("run guest-to-host pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if stdout.String() != "guest-output\n" {
		t.Fatalf("guest-to-host stdout = %q", stdout.String())
	}
}

func TestGuestPipelineStreamsStdinForGuestStages(t *testing.T) {
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "default", Image: "alpine", Network: true}
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		command := ""
		if len(req.Command) > 2 {
			command = req.Command[2]
		}
		if strings.Contains(command, "printf 'script-from-guest'") {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "script-from-guest"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		data, closeEvents := drainExecInputStream(inputs)
		if closeEvents != 0 {
			return fmt.Errorf("pipeline input sent explicit stdin_close events = %d", closeEvents)
		}
		if string(data) != "script-from-guest" {
			return fmt.Errorf("guest stdin = %q", string(data))
		}
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent != nil {
			if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "guest-stage-ok\n"}); err != nil {
				return err
			}
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	if err := sh.eval(`printf 'script-from-guest' | @alpine sh`, &stdout, &stderr); err != nil {
		t.Fatalf("run guest-to-guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if stdout.String() != "guest-stage-ok\n" {
		t.Fatalf("guest pipeline stdout = %q", stdout.String())
	}
	if len(api.runs) != 2 {
		t.Fatalf("guest runs = %d, want 2", len(api.runs))
	}

	api.runs = nil
	stdout.Reset()
	stderr.Reset()
	api.runStream = func(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent == nil {
			return nil
		}
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		data, closeEvents := drainExecInputStream(inputs)
		if len(data) != 0 || closeEvents != 0 {
			return fmt.Errorf("empty pipeline data=%d closeEvents=%d", len(data), closeEvents)
		}
		api.runs = append(api.runs, recordedRun{id: id, req: req})
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	if err := sh.eval(`true | @alpine cat`, &stdout, &stderr); err != nil {
		t.Fatalf("run empty guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	if len(api.runs) != 2 {
		t.Fatalf("empty guest pipeline runs = %d, want 2", len(api.runs))
	}
	if len(api.runs[1].req.Stdin) != 0 {
		t.Fatalf("empty guest pipeline stdin = %q", string(api.runs[1].req.Stdin))
	}
}

func TestGuestPipelineStreamsLargeInputInChunks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("large pipeline test uses POSIX host commands")
	}
	api := newRecordingShellAPI("alpine")
	api.instances["default"] = client.InstanceState{ID: "default", Status: "running", Image: "alpine"}
	chunkCount := make(chan int, 1)
	byteCount := make(chan int, 1)
	api.runInteractive = func(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
		total := 0
		chunks := 0
		for input := range inputs {
			if input.Kind != "stdin" {
				continue
			}
			chunks++
			if len(input.Data) > 0 {
				total += len(input.Data)
			} else {
				total += len(input.Input)
			}
		}
		chunkCount <- chunks
		byteCount <- total
		if onEvent != nil {
			return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
		}
		return nil
	}
	sh := newUnitShell(t, api)

	var stdout, stderr bytes.Buffer
	if err := sh.eval(`dd if=/dev/zero bs=1024 count=128 2>/dev/null | @alpine wc -c`, &stdout, &stderr); err != nil {
		t.Fatalf("run large host-to-guest pipeline: %v\nstderr:\n%s", err, stderr.String())
	}
	select {
	case got := <-byteCount:
		if got != 128*1024 {
			t.Fatalf("streamed bytes = %d, want %d", got, 128*1024)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("guest stdin was not drained")
	}
	if chunks := <-chunkCount; chunks < 2 {
		t.Fatalf("streamed chunks = %d, want multiple chunks", chunks)
	}
}

func TestCopyEndpointResolutionAndGuestHostPathSafety(t *testing.T) {
	api := newRecordingShellAPI("alpine", "ubuntu")
	sh := newUnitShell(t, api)
	sh.context = commandContext{Mode: modeVM, VMID: "vm", Image: "alpine"}
	_, guestCWD, err := guestHostPaths(sh.hostCWD)
	if err != nil {
		t.Fatalf("guest host paths: %v", err)
	}

	guest, err := sh.parseCopyEndpoint("@:notes.txt")
	if err != nil {
		t.Fatalf("parse current guest endpoint: %v", err)
	}
	if guest.ctx.Mode != modeVM || guest.path != path.Join(guestCWD, "notes.txt") {
		t.Fatalf("current guest endpoint = %+v", guest)
	}

	host, err := sh.parseCopyEndpoint("@host:relative.txt")
	if err != nil {
		t.Fatalf("parse host endpoint: %v", err)
	}
	if host.ctx.Mode != modeHost || host.path != filepath.Join(sh.hostCWD, "relative.txt") {
		t.Fatalf("host endpoint = %+v", host)
	}

	ubuntu, err := sh.parseCopyEndpoint("@ubuntu:~/result.txt")
	if err != nil {
		t.Fatalf("parse named guest endpoint: %v", err)
	}
	if ubuntu.ctx.Image != "ubuntu" || ubuntu.path != "/home/ubuntu/result.txt" {
		t.Fatalf("named guest endpoint = %+v", ubuntu)
	}

	if _, err := sh.parseCopyEndpoint("@ubuntu"); err == nil || !strings.Contains(err.Error(), "must use @target:path") {
		t.Fatalf("parse malformed endpoint error = %v", err)
	}
	if hostPath, ok := guestHostPathToHost(sh.hostCWD, "/tmp/file"); ok || hostPath != "" {
		t.Fatalf("non-host guest path mapped to %q", hostPath)
	}
}

func TestExtractTarToHostRejectsTraversal(t *testing.T) {
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatalf("create dst: %v", err)
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: int64(len("nope"))}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write([]byte("nope")); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	err := extractTarToHost(bytes.NewReader(archive.Bytes()), dst, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe tar path") {
		t.Fatalf("extract traversal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal file exists or stat failed unexpectedly: %v", err)
	}
}

func newUnitShell(t *testing.T, api *recordingShellAPI) *shellState {
	t.Helper()
	hostCWD := t.TempDir()
	rootCache := t.TempDir()
	sh := &shellState{
		api:        api,
		context:    defaultContext("default", "", false),
		hostCWD:    hostCWD,
		rootCache:  rootCache,
		imageCache: map[string]bool{},
		vmRunning:  map[string]bool{},
		contextCWD: map[string]string{},
		promptOut:  io.Discard,
		env:        map[string]string{},
		aliases:    map[string]string{},
		confirmPull: func(string, io.Writer) (bool, error) {
			return false, nil
		},
		confirmVMRestart: func(string, io.Writer) (bool, error) {
			return true, nil
		},
	}
	sh.completion = newVMSHCompleter(sh)
	return sh
}

func runShellUnitScript(sh *shellState, script string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := sh.evalScriptLines(strings.NewReader(script), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

type notifyWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	target string
	seen   chan struct{}
	once   sync.Once
}

func newNotifyWriter(target string) *notifyWriter {
	return &notifyWriter{target: target, seen: make(chan struct{})}
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.target) {
		w.once.Do(func() {
			close(w.seen)
		})
	}
	return n, err
}

func (w *notifyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type recordingShellAPI struct {
	images         map[string]client.ImageState
	instances      map[string]client.InstanceState
	starts         []recordedStart
	runs           []recordedRun
	execs          []recordedExec
	forwards       []recordedForward
	servicePorts   []recordedServiceProxyPort
	deleted        []string
	runStream      func(context.Context, string, client.RunRequest, func(client.ExecEvent) error) error
	runInteractive func(string, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
}

type recordedStart struct {
	id  string
	req client.StartInstanceRequest
}

type recordedRun struct {
	id  string
	req client.RunRequest
}

type recordedExec struct {
	id  string
	req client.ExecRequest
}

type recordedForward struct {
	id      string
	forward client.PortForward
}

type recordedServiceProxyPort struct {
	id   string
	port int
}

func newRecordingShellAPI(images ...string) *recordingShellAPI {
	api := &recordingShellAPI{
		images:    map[string]client.ImageState{},
		instances: map[string]client.InstanceState{},
	}
	for _, image := range images {
		api.images[image] = client.ImageState{Name: image, Status: "ready"}
	}
	return api
}

func (a *recordingShellAPI) HealthCheck() error { return nil }

func (a *recordingShellAPI) Capabilities() (client.CapabilitiesResponse, error) {
	return client.CapabilitiesResponse{VMSupported: true, SupportsNestedVirt: true}, nil
}

func (a *recordingShellAPI) GetImage(name string) (client.ImageState, error) {
	if image, ok := a.images[name]; ok {
		return image, nil
	}
	return client.ImageState{}, fmt.Errorf("image %q not found", name)
}

func (a *recordingShellAPI) PullImageStream(name string, req client.PullImageRequest, onEvent func(client.ProgressEvent) error) error {
	a.images[name] = client.ImageState{Name: name, Source: req.Source, Status: "ready"}
	return nil
}

func (a *recordingShellAPI) DeleteImage(name string) error {
	a.deleted = append(a.deleted, name)
	delete(a.images, name)
	return nil
}

func (a *recordingShellAPI) SaveInstanceImage(id string, req client.SaveImageRequest) (client.ImageState, error) {
	state := client.ImageState{Name: req.Name, Source: "vm:" + id, Status: "ready"}
	a.images[req.Name] = state
	return state, nil
}

func (a *recordingShellAPI) StartInstanceStreamWithID(id string, req client.StartInstanceRequest, onEvent func(client.BootEvent) error) (client.InstanceState, error) {
	a.starts = append(a.starts, recordedStart{id: id, req: req})
	state := client.InstanceState{ID: id, Status: "running", Image: req.Image, MemoryMB: req.MemoryMB, CPUs: req.CPUs, NestedVirt: req.NestedVirt}
	a.instances[id] = state
	if onEvent != nil {
		if err := onEvent(client.BootEvent{Kind: "ready", State: state}); err != nil {
			return client.InstanceState{}, err
		}
	}
	return state, nil
}

func (a *recordingShellAPI) ShutdownInstanceWithID(id string) error {
	a.instances[id] = client.InstanceState{ID: id, Status: "stopped"}
	return nil
}

func (a *recordingShellAPI) InstanceStatusOf(id string) (client.InstanceState, error) {
	if state, ok := a.instances[id]; ok {
		return state, nil
	}
	return client.InstanceState{ID: id, Status: "stopped"}, nil
}

func (a *recordingShellAPI) InstanceStatuses() ([]client.InstanceState, error) {
	var states []client.InstanceState
	for _, state := range a.instances {
		states = append(states, state)
	}
	return states, nil
}

func (a *recordingShellAPI) AddPortForwardTo(id string, forward client.PortForward) error {
	a.forwards = append(a.forwards, recordedForward{id: id, forward: forward})
	return nil
}

func (a *recordingShellAPI) AllowServiceProxyPortTo(id string, port int) error {
	a.servicePorts = append(a.servicePorts, recordedServiceProxyPort{id: id, port: port})
	return nil
}

func (a *recordingShellAPI) CreateWatchdogLease(req client.WatchdogLeaseRequest) (client.WatchdogLeaseResponse, error) {
	return client.WatchdogLeaseResponse{LeaseID: "lease", TimeoutSeconds: req.TimeoutSeconds}, nil
}

func (a *recordingShellAPI) FeedWatchdogLease(string) error { return nil }

func (a *recordingShellAPI) ReleaseWatchdogLease(string) error { return nil }

func (a *recordingShellAPI) RunStreamIn(id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
	return a.RunStreamInContext(context.Background(), id, req, onEvent)
}

func (a *recordingShellAPI) RunStreamInContext(ctx context.Context, id string, req client.RunRequest, onEvent func(client.ExecEvent) error) error {
	if a.runStream != nil {
		return a.runStream(ctx, id, req, onEvent)
	}
	a.runs = append(a.runs, recordedRun{id: id, req: req})
	if onEvent != nil {
		if err := onEvent(client.ExecEvent{Kind: "stdout", Output: "guest-output\n"}); err != nil {
			return err
		}
		if err := onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0}); err != nil {
			return err
		}
	}
	return nil
}

func (a *recordingShellAPI) RunInteractiveStreamIn(id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	return a.RunInteractiveStreamInContext(context.Background(), id, req, inputs, onEvent)
}

func (a *recordingShellAPI) RunInteractiveStreamInContext(ctx context.Context, id string, req client.RunRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	if a.runInteractive != nil {
		return a.runInteractive(id, req, inputs, onEvent)
	}
	return a.RunStreamInContext(ctx, id, req, onEvent)
}

func (a *recordingShellAPI) ExecStreamIn(id string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	a.execs = append(a.execs, recordedExec{id: id, req: req})
	if onEvent != nil {
		return onEvent(client.ExecEvent{Kind: "exit", ExitCode: 0})
	}
	return nil
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func testCodexJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal jwt header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func makeFakeCodexRelease(t *testing.T, codexHome, version, target string) string {
	t.Helper()
	releaseDir := filepath.Join(codexHome, "packages", "standalone", "releases", version+"-"+target)
	for _, dir := range []string{
		filepath.Join(releaseDir, "bin"),
		filepath.Join(releaseDir, "codex-path"),
		filepath.Join(releaseDir, "codex-resources"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create fake Codex release dir: %v", err)
		}
	}
	for _, file := range []string{
		filepath.Join(releaseDir, "bin", "codex"),
		filepath.Join(releaseDir, "codex-path", "rg"),
		filepath.Join(releaseDir, "codex-resources", "bwrap"),
	} {
		if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write fake Codex release file: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "codex-package.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write fake Codex package metadata: %v", err)
	}
	link := filepath.Join(releaseDir, "codex")
	if err := os.Symlink(filepath.Join("bin", "codex"), link); err != nil {
		if err := os.WriteFile(link, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write fake Codex command: %v", err)
		}
	}
	return releaseDir
}

func fakeCodexPackageArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	files := map[string][]byte{
		"codex-package.json":     []byte("{}\n"),
		"bin/codex":              []byte("#!/bin/sh\n"),
		"codex-path/rg":          []byte("#!/bin/sh\n"),
		"codex-resources/bwrap":  []byte("#!/bin/sh\n"),
		"codex-resources/config": []byte("fake\n"),
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := files[name]
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}
		if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, "config") {
			header.Mode = 0o644
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar data: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func drainExecInputStream(inputs <-chan client.ExecInput) ([]byte, int) {
	var data bytes.Buffer
	closeEvents := 0
	for input := range inputs {
		switch input.Kind {
		case "stdin":
			if len(input.Data) > 0 {
				data.Write(input.Data)
			} else {
				data.WriteString(input.Input)
			}
		case "stdin_close":
			closeEvents++
		}
	}
	return data.Bytes(), closeEvents
}
