package vmshd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tinyrange/vmsh/internal/vmshdprotocol"
	"golang.org/x/net/websocket"
	"j5.nz/cc/client"
)

type mcpBearerTransport struct {
	token string
}

func (t mcpBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(req)
}

func TestMCPEndpointIsScopedToOwnedIsolatedVMs(t *testing.T) {
	const daemonToken = "daemon-control-token"
	var mu sync.Mutex
	var starts []client.StartInstanceRequest
	var pulls []client.PullImageRequest
	var runs []client.RunRequest
	var shutdowns []string
	streamHandler := websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.RunRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		mu.Lock()
		runs = append(runs, req)
		mu.Unlock()
		for {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil {
				return
			}
			if input.Kind == "stdin_close" {
				break
			}
		}
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("guest-output")})
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte("guest-error")})
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 7})
	})
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+daemonToken {
			writeJSON(w, http.StatusUnauthorized, client.ErrorResponse{Error: "unauthorized"})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(vmshdprotocol.HeaderProtocol) == "" {
			writeJSON(w, http.StatusUpgradeRequired, client.ErrorResponse{Error: "protocol required"})
			return
		}
		switch r.URL.Path {
		case "/vm/run/stream":
			streamHandler.ServeHTTP(w, r)
		case "/image/alpine":
			if r.Method == http.MethodGet {
				writeJSON(w, http.StatusNotFound, client.ErrorResponse{Error: "image not found"})
				return
			}
			var req client.PullImageRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode pull request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			pulls = append(pulls, req)
			mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]bool{"pulled": true})
		case "/vm/start":
			var req client.StartInstanceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode start request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			starts = append(starts, req)
			mu.Unlock()
			writeJSON(w, http.StatusOK, client.InstanceState{ID: req.ID, Status: "running", Image: req.Image})
		case "/vm/run":
			var req client.RunRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode run request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			runs = append(runs, req)
			mu.Unlock()
			if r.URL.Query().Get("stream") == "1" {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				encoder := json.NewEncoder(w)
				_ = encoder.Encode(client.ExecEvent{Kind: "stdout", Data: []byte("guest-output")})
				_ = encoder.Encode(client.ExecEvent{Kind: "stderr", Data: []byte("guest-error")})
				_ = encoder.Encode(client.ExecEvent{Kind: "exit", ExitCode: 7})
				return
			}
			writeJSON(w, http.StatusOK, client.ExecResponse{ExitCode: 7, Output: "guest-output"})
		case "/vm/shutdown":
			mu.Lock()
			shutdowns = append(shutdowns, r.URL.Query().Get("id"))
			mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()

	manager := newMCPManager(daemonToken)
	manager.SetControlURL(control.URL)
	t.Cleanup(func() { _ = manager.Close() })
	info, err := manager.Start("session-1")
	if err != nil {
		t.Fatalf("start MCP endpoint: %v", err)
	}
	credential, err := manager.MintCredential("session-1")
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}

	unauthorized, err := http.Post(info.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("unauthorized request: %v", err)
	}
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.StatusCode, http.StatusUnauthorized)
	}

	ordinary, err := http.NewRequest(http.MethodGet, control.URL+"/vm", nil)
	if err != nil {
		t.Fatal(err)
	}
	ordinary.Header.Set("Authorization", "Bearer "+credential.Token)
	ordinaryResponse, err := http.DefaultClient.Do(ordinary)
	if err != nil {
		t.Fatalf("ordinary API request: %v", err)
	}
	_ = ordinaryResponse.Body.Close()
	if ordinaryResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP token ordinary API status = %d, want %d", ordinaryResponse.StatusCode, http.StatusUnauthorized)
	}

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "vmsh-test", Version: "1"}, nil)
	session, err := mcpClient.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   info.URL,
		HTTPClient: &http.Client{Transport: mcpBearerTransport{token: credential.Token}},
	}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	wantNames := []string{
		"vm_artifact_delete", "vm_artifact_export", "vm_artifact_import", "vm_artifact_list",
		"vm_context_close", "vm_context_open", "vm_context_run", "vm_context_status", "vm_copy",
		"vm_create", "vm_exec_cancel", "vm_exec_start", "vm_exec_status", "vm_exec_wait", "vm_list", "vm_run", "vm_stop",
	}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("tools = %v, want %v", names, wantNames)
	}

	created, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_create", Arguments: map[string]any{
		"image": "alpine", "name": "worker", "memory_mb": 768, "cpus": 2,
	}})
	if err != nil {
		t.Fatalf("create VM: %v", err)
	}
	if created.IsError {
		t.Fatalf("create VM returned tool error: %#v", created.Content)
	}
	mu.Lock()
	if len(starts) != 1 {
		mu.Unlock()
		t.Fatalf("start requests = %d, want 1", len(starts))
	}
	start := starts[0]
	if len(pulls) != 1 || pulls[0].Source != "docker.io/library/alpine:latest" {
		mu.Unlock()
		t.Fatalf("pull requests = %#v", pulls)
	}
	mu.Unlock()
	if start.ID != "worker-isolated" || start.Image != "alpine" || start.MemoryMB != 768 || start.CPUs != 2 {
		t.Fatalf("start request = %#v", start)
	}
	if len(start.Shares) != 0 || start.Network == nil || !start.Network.Enabled || !start.Network.AllowInternet || !start.Network.BlockHostAccess || len(start.Network.PortForwards) != 0 {
		t.Fatalf("start request was not isolated: %#v", start)
	}

	run, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_run", Arguments: map[string]any{
		"vm_id": start.ID, "command": []string{"sh", "-c", "exit 7"}, "user": "nobody",
	}})
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if run.IsError {
		t.Fatalf("run command returned tool error: %#v", run.Content)
	}
	mu.Lock()
	if len(runs) != 1 || runs[0].ID != start.ID || runs[0].User != "nobody" || !slices.Equal(runs[0].Command, []string{"sh", "-c", "exit 7"}) {
		mu.Unlock()
		t.Fatalf("run requests = %#v", runs)
	}
	mu.Unlock()

	foreign, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_run", Arguments: map[string]any{
		"vm_id": "someone-elses-vm", "command": []string{"true"},
	}})
	if err != nil {
		t.Fatalf("foreign VM call: %v", err)
	}
	if !foreign.IsError {
		t.Fatal("foreign VM command was accepted")
	}
	mu.Lock()
	if len(runs) != 1 {
		mu.Unlock()
		t.Fatalf("foreign command reached backend: %#v", runs)
	}
	mu.Unlock()

	if err := manager.Stop("session-1"); err != nil {
		t.Fatalf("stop endpoint: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(shutdowns, start.ID) {
		t.Fatalf("shutdowns = %v, want %q", shutdowns, start.ID)
	}
}

func TestMCPAutoPullRejectsHostFacingSources(t *testing.T) {
	for _, image := range []string{
		"file:///etc/passwd",
		"/tmp/rootfs.simg",
		"docker-archive:/tmp/image.tar",
		"localhost:5000/private/image",
		"127.0.0.1:5000/private/image",
		"../private/image",
	} {
		if err := validateMCPAutoPullImage(image); err == nil {
			t.Errorf("validateMCPAutoPullImage(%q) accepted a host-facing source", image)
		}
	}
	for _, image := range []string{"alpine", "alpine:3.22", "library/alpine", "docker.io/library/alpine:latest"} {
		if err := validateMCPAutoPullImage(image); err != nil {
			t.Errorf("validateMCPAutoPullImage(%q): %v", image, err)
		}
	}
}

func TestMCPAsyncCommandSeparatesOutputAndCancels(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle("/vm/run/stream", websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.RunRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		if len(req.Command) != 0 && req.Command[0] == "block" {
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("ready\n")})
			close(started)
			for {
				var input client.ExecInput
				if err := websocket.JSON.Receive(ws, &input); err != nil {
					return
				}
				if input.Kind == "signal" {
					_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 143})
					return
				}
			}
		}
		var input client.ExecInput
		if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
			return
		}
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("out\n")})
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte("err\n")})
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 3})
	}))
	control := httptest.NewServer(mux)
	defer control.Close()

	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"owned": {ID: "owned"}},
		commands: make(map[string]*mcpCommand), artifacts: make(map[string]*mcpArtifact), contexts: make(map[string]*mcpGuestContext),
	}
	command, err := endpoint.startCommand(mcpRunVMInput{VMID: "owned", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	<-command.done
	result := command.snapshot(0, 0, 1024, false)
	if result.Status != "exited" || result.ExitCode == nil || *result.ExitCode != 3 || result.Stdout.Text != "out\n" || result.Stderr.Text != "err\n" {
		t.Fatalf("command result = %#v", result)
	}
	if result.Output != "" || result.OutputBase64 != "" {
		t.Fatalf("paginated command result duplicated output: %#v", result)
	}
	paged := command.snapshot(1, 0, 2, false)
	if paged.Stdout.Text != "ut" || paged.Stdout.NextOffset != 3 || paged.Stdout.TotalBytes != 4 || paged.Output != "" {
		t.Fatalf("paged command result = %#v", paged)
	}
	_, canceledPage, err := endpoint.cancelVMCommand(t.Context(), nil, mcpCommandCancelInput{
		CommandID: command.id, StdoutOffset: 1, MaxBytes: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if canceledPage.Stdout.Text != "ut" || canceledPage.Output != "" || canceledPage.OutputBase64 != "" {
		t.Fatalf("cancel page = %#v", canceledPage)
	}

	blocked, err := endpoint.startCommand(mcpRunVMInput{VMID: "owned", Command: []string{"block"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking command did not stream its first output")
	}
	deadline := time.Now().Add(time.Second)
	for blocked.snapshot(0, 0, 1024, false).Stdout.Text != "ready\n" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	blocked.requestCancel()
	select {
	case <-blocked.done:
	case <-time.After(time.Second):
		t.Fatal("canceled command was not reaped")
	}
	canceled := blocked.snapshot(0, 0, 1024, false)
	if canceled.Status != "canceled" || canceled.ExitCode == nil || *canceled.ExitCode != 130 || canceled.Stdout.Text != "ready\n" {
		t.Fatalf("canceled command result = %#v", canceled)
	}
}

func TestMCPCommandStreamsLargeStdinToEOF(t *testing.T) {
	stdin := bytes.Repeat([]byte{0x00, 0xff, 0x7f, 0x80}, 256<<10)
	received := make(chan []byte, 1)
	var largestChunk int
	mux := http.NewServeMux()
	mux.Handle("/vm/run/stream", websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.RunRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			t.Errorf("receive run request: %v", err)
			return
		}
		if len(req.Stdin) != 0 {
			t.Errorf("run request contained %d inline stdin bytes", len(req.Stdin))
			return
		}
		var data []byte
		for {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil {
				t.Errorf("receive stdin: %v", err)
				return
			}
			if input.Kind == "stdin_close" {
				break
			}
			if input.Kind != "stdin" {
				t.Errorf("input kind = %q", input.Kind)
				return
			}
			if len(input.Data) > largestChunk {
				largestChunk = len(input.Data)
			}
			data = append(data, input.Data...)
		}
		received <- data
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
	}))
	control := httptest.NewServer(mux)
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"owned": {ID: "owned"}},
		commands: make(map[string]*mcpCommand), artifacts: make(map[string]*mcpArtifact), contexts: make(map[string]*mcpGuestContext),
	}
	command, err := endpoint.startCommand(mcpRunVMInput{VMID: "owned", Command: []string{"consume"}, StdinBase64: base64.StdEncoding.EncodeToString(stdin)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-command.done:
	case <-time.After(5 * time.Second):
		t.Fatal("large stdin command did not finish")
	}
	if result := command.snapshot(0, 0, 1024, false); result.Status != "exited" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("command result = %#v", result)
	}
	select {
	case data := <-received:
		if !bytes.Equal(data, stdin) {
			t.Fatalf("received %d of %d stdin bytes", len(data), len(stdin))
		}
	default:
		t.Fatal("server did not receive stdin")
	}
	if largestChunk > mcpStdinChunkBytes {
		t.Fatalf("largest stdin chunk = %d", largestChunk)
	}
}

func TestMCPArtifactsAndPersistentContextStayInsideOwnedVMs(t *testing.T) {
	archive := testMCPArchive(t, "payload.bin", bytes.Repeat([]byte{0x00, 0xff, 't', 'a', 'r'}, 20<<10))
	var extracted []byte
	var largestInput int
	var shellExported bool
	mux := http.NewServeMux()
	streamHandler := websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.ExecRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		if slices.Equal(req.Command, []string{"/bin/sh", "-c", ":"}) {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
				return
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
			return
		}
		switch req.Kind {
		case "fs_archive":
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
				t.Errorf("archive stdin close = %#v, %v", input, err)
				return
			}
			for offset := 0; offset < len(archive); offset += 16 << 10 {
				end := offset + 16<<10
				if end > len(archive) {
					end = len(archive)
				}
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: archive[offset:end]})
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
			return
		case "fs_extract":
			for {
				var input client.ExecInput
				if err := websocket.JSON.Receive(ws, &input); err != nil {
					return
				}
				if input.Kind == "stdin_close" {
					break
				}
				if len(input.Data) > largestInput {
					largestInput = len(input.Data)
				}
				extracted = append(extracted, input.Data...)
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
			return
		}
		for {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil {
				return
			}
			if input.Kind != "stdin" {
				continue
			}
			script := string(input.Data)
			markerStart := strings.Index(script, `\036marker_`)
			markerEnd := strings.Index(script[markerStart+4:], ":%s")
			if markerStart < 0 || markerEnd < 0 {
				return
			}
			marker := script[markerStart+4 : markerStart+4+markerEnd]
			var stdout, stderr string
			if strings.Contains(script, "export ANSWER=42") {
				shellExported = true
			}
			if strings.Contains(script, "printf '%s' \"$ANSWER\"") && shellExported {
				stdout = "42"
			}
			if strings.Contains(script, "printf problem >&2") {
				stderr = "problem"
			}
			if stdout != "" {
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte(stdout)})
			}
			if stderr != "" {
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte(stderr)})
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("\x1e" + marker + ":0\x1f\n")})
		}
	})
	mux.Handle("/vm/run", streamHandler)
	mux.Handle("/vm/run/stream", streamHandler)
	control := httptest.NewServer(mux)
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"one": {ID: "one"}, "two": {ID: "two"}},
		commands: make(map[string]*mcpCommand), artifacts: make(map[string]*mcpArtifact), contexts: make(map[string]*mcpGuestContext),
	}

	_, exported, err := endpoint.exportArtifact(context.Background(), nil, mcpArtifactExportInput{VMID: "one", Path: "/project"})
	if err != nil {
		t.Fatalf("export artifact: %v", err)
	}
	if exported.Artifact.Size != int64(len(archive)) || exported.Artifact.SHA256 == "" {
		t.Fatalf("artifact metadata = %#v", exported.Artifact)
	}
	if _, _, err := endpoint.importArtifact(context.Background(), nil, mcpArtifactImportInput{ArtifactID: exported.Artifact.ID, VMID: "two", Path: "/work"}); err != nil {
		t.Fatalf("import artifact: %v", err)
	}
	if !bytes.Equal(extracted, archive) {
		t.Fatalf("extracted archive size = %d, want %d", len(extracted), len(archive))
	}
	if largestInput > mcpArtifactInputChunk {
		t.Fatalf("largest artifact input = %d, want at most %d", largestInput, mcpArtifactInputChunk)
	}

	_, opened, err := endpoint.openGuestContext(context.Background(), nil, mcpContextOpenInput{VMID: "one"})
	if err != nil {
		t.Fatalf("open context: %v", err)
	}
	if _, _, err := endpoint.runGuestContext(context.Background(), nil, mcpContextRunInput{ContextID: opened.ContextID, CommandLine: "export ANSWER=42"}); err != nil {
		t.Fatalf("export in context: %v", err)
	}
	_, result, err := endpoint.runGuestContext(context.Background(), nil, mcpContextRunInput{ContextID: opened.ContextID, CommandLine: `printf '%s' "$ANSWER"; printf problem >&2`})
	if err != nil {
		t.Fatalf("read context state: %v", err)
	}
	if result.Stdout != "42" || result.Stderr != "problem" || result.ExitCode != 0 || result.CommandStatus != "exited" || result.ContextStatus != "running" {
		t.Fatalf("context result = %#v", result)
	}
	if _, _, err := endpoint.openGuestContext(context.Background(), nil, mcpContextOpenInput{VMID: "foreign"}); err == nil {
		t.Fatal("opened a persistent context for a foreign VM")
	}
	if _, _, err := endpoint.closeGuestContext(context.Background(), nil, mcpContextStatusInput{ContextID: opened.ContextID}); err != nil {
		t.Fatalf("close context: %v", err)
	}
}

func TestMCPContextOpenReportsInvalidWorkdirWithoutWaitingForShellProbe(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/vm/run", websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.ExecRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte("invalid workdir: permission denied")})
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 126})
	}))
	control := httptest.NewServer(mux)
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"one": {ID: "one"}},
		commands: make(map[string]*mcpCommand), artifacts: make(map[string]*mcpArtifact), contexts: make(map[string]*mcpGuestContext),
	}
	started := time.Now()
	_, _, err := endpoint.openGuestContext(t.Context(), nil, mcpContextOpenInput{VMID: "one", WorkDir: "/root", User: "1000:1000"})
	if err == nil {
		t.Fatal("opened context with inaccessible workdir")
	}
	if time.Since(started) > time.Second {
		t.Fatalf("invalid workdir took %s to reject", time.Since(started))
	}
}

func TestMCPContextCloseCancelsActiveShellStreamOutOfBand(t *testing.T) {
	active := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle("/vm/run", websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.ExecRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		var input client.ExecInput
		if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
			return
		}
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
	}))
	mux.Handle("/vm/run/stream", websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.RunRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		for count := 0; ; count++ {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil {
				return
			}
			if input.Kind != "stdin" {
				continue
			}
			if count == 0 {
				script := string(input.Data)
				markerStart := strings.Index(script, `\036marker_`)
				if markerStart < 0 {
					t.Errorf("probe did not contain a status marker")
					return
				}
				markerEnd := strings.Index(script[markerStart+4:], ":%s")
				if markerEnd < 0 {
					t.Errorf("probe did not contain a complete status marker")
					return
				}
				marker := script[markerStart+4 : markerStart+4+markerEnd]
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("\x1e" + marker + ":0\x1f\n")})
				continue
			}
			close(active)
		}
	}))
	control := httptest.NewServer(mux)
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"one": {ID: "one"}},
		commands: make(map[string]*mcpCommand), artifacts: make(map[string]*mcpArtifact), contexts: make(map[string]*mcpGuestContext),
	}
	_, opened, err := endpoint.openGuestContext(t.Context(), nil, mcpContextOpenInput{VMID: "one"})
	if err != nil {
		t.Fatalf("open context: %v", err)
	}
	runDone := make(chan error, 1)
	go func() {
		_, _, err := endpoint.runGuestContext(context.Background(), nil, mcpContextRunInput{ContextID: opened.ContextID, CommandLine: "sleep 60"})
		runDone <- err
	}()
	select {
	case <-active:
	case <-time.After(time.Second):
		t.Fatal("context command did not reach the shell stream")
	}
	started := time.Now()
	if _, result, err := endpoint.closeGuestContext(t.Context(), nil, mcpContextStatusInput{ContextID: opened.ContextID}); err != nil || !result.Closed {
		t.Fatalf("close context = %#v, %v", result, err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("context close took %s", time.Since(started))
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("active context call remained blocked after close")
	}
}

func TestMCPArchiveRejectsUnsupportedEntriesBeforeImport(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "project", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "project/pipe", Typeflag: tar.TypeFifo, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	err := validateMCPArchive(archive.Bytes())
	if err == nil || err.Error() != `unsupported archive entry "project/pipe" (FIFO)` {
		t.Fatalf("archive validation error = %v", err)
	}
}

func TestMCPFailedSpecialFileCopyLeavesLifecycleResponsive(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "project", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "project/pipe", Typeflag: tar.TypeFifo, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var extractCalls int
	control := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.ExecRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		switch req.Kind {
		case "fs_archive":
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
				t.Errorf("archive stdin close = %#v, %v", input, err)
				return
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: archive.Bytes()})
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
		case "fs_extract":
			extractCalls++
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte("unexpected extract")})
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 1})
		}
	}))
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil),
		vms: map[string]mcpVM{
			"source":      {ID: "source"},
			"destination": {ID: "destination"},
		},
		commands: make(map[string]*mcpCommand), artifacts: make(map[string]*mcpArtifact), contexts: make(map[string]*mcpGuestContext),
	}

	if _, _, err := endpoint.copyGuestPath(t.Context(), nil, mcpCopyInput{
		SourceVM: "source", SourcePath: "/project", DestinationVM: "destination", DestinationPath: "/project",
	}); err == nil || err.Error() != `archive guest path: unsupported archive entry "project/pipe" (FIFO)` {
		t.Fatalf("copy error = %v", err)
	}
	if extractCalls != 0 {
		t.Fatalf("destination extraction started %d time(s)", extractCalls)
	}

	done := make(chan error, 1)
	go func() {
		_, listed, err := endpoint.listVMs(t.Context(), nil, mcpListVMsInput{})
		if err == nil && len(listed.VMs) != 2 {
			err = fmt.Errorf("listed VMs = %#v", listed.VMs)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("VM lifecycle remained blocked after failed copy")
	}
}

func TestMCPExtractErrorIncludesGuestDiagnostic(t *testing.T) {
	archive := testMCPArchive(t, "payload", []byte("data"))
	control := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.ExecRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		for {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil {
				return
			}
			if input.Kind == "stdin_close" {
				break
			}
		}
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte("copy conflict at /work/payload")})
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 1})
	}))
	defer control.Close()
	endpoint := &mcpEndpoint{control: client.NewClient(control.URL, nil)}
	err := endpoint.extractGuestArchive(t.Context(), "destination", "/work", true, "1000:1000", archive)
	if err == nil || err.Error() != "extract guest archive exited with status 1: copy conflict at /work/payload" {
		t.Fatalf("extract error = %v", err)
	}
}

func testMCPArchive(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestMCPRevokedCredentialStopsAuthenticating(t *testing.T) {
	control := httptest.NewServer(http.NotFoundHandler())
	defer control.Close()
	manager := newMCPManager("daemon-token")
	manager.SetControlURL(control.URL)
	t.Cleanup(func() { _ = manager.Close() })
	info, err := manager.Start("session-1")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := manager.MintCredential("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !manager.RevokeCredential("session-1", credential.ID) {
		t.Fatal("credential was not revoked")
	}
	req, err := http.NewRequest(http.MethodPost, info.URL, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential.Token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestMCPOwnedVMStopsWhenShellSessionIsDeleted(t *testing.T) {
	const daemonToken = "daemon-token"
	shutdown := make(chan string, 1)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+daemonToken {
			writeJSON(w, http.StatusUnauthorized, client.ErrorResponse{Error: "unauthorized"})
			return
		}
		switch r.URL.Path {
		case "/image/alpine":
			writeJSON(w, http.StatusOK, map[string]string{"name": "alpine"})
		case "/vm/start":
			var req client.StartInstanceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode start request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writeJSON(w, http.StatusOK, client.InstanceState{ID: req.ID, Status: "running", Image: req.Image})
		case "/vm/shutdown":
			shutdown <- r.URL.Query().Get("id")
			writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()

	server := NewServer(daemonToken)
	server.mcp.SetControlURL(control.URL)
	t.Cleanup(func() { _ = server.mcp.Close() })
	session, err := server.registry.Create(CreateSessionRequest{Name: "shell"})
	if err != nil {
		t.Fatalf("create shell session: %v", err)
	}
	if _, err := server.mcp.Start(session.ID); err != nil {
		t.Fatalf("start MCP: %v", err)
	}
	server.mcp.mu.Lock()
	endpoint := server.mcp.endpoints[session.ID]
	server.mcp.mu.Unlock()
	created, _, err := endpoint.createVM(context.Background(), nil, mcpCreateVMInput{Image: "alpine"})
	if err != nil {
		t.Fatalf("create MCP VM: %v", err)
	}
	if created != nil {
		t.Fatalf("unexpected raw tool result: %#v", created)
	}

	deleting, jobIDs, ok := server.registry.BeginDelete(session.ID)
	if !ok {
		t.Fatal("begin session deletion failed")
	}
	result := server.finishSessionCleanup(sessionCleanup{Session: deleting, JobIDs: jobIDs}, nil)
	if result.State == "cleanup_failed" {
		t.Fatalf("session cleanup failed: %#v", result.Cleanup)
	}
	if _, running := server.mcp.Status(session.ID); running {
		t.Fatal("MCP endpoint survived owning shell session deletion")
	}
	select {
	case id := <-shutdown:
		if id == "" {
			t.Fatal("session cleanup sent an empty VM id")
		}
	case <-time.After(time.Second):
		t.Fatal("session cleanup did not stop the MCP-owned VM")
	}
}
