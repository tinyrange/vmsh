package vmshd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
			writeMCPBootReady(t, w, r, client.InstanceState{ID: req.ID, Status: "running", Image: req.Image})
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
	if info.Version != mcpImplementationVersion() {
		t.Fatalf("MCP endpoint version = %q, want %q", info.Version, mcpImplementationVersion())
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

	oversizedJSON := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vm_run","arguments":{"stdin":"` + strings.Repeat("x", mcpMaxRequestBytes) + `"}}}`
	oversizedRequest, err := http.NewRequest(http.MethodPost, info.URL, strings.NewReader(oversizedJSON))
	if err != nil {
		t.Fatal(err)
	}
	oversizedRequest.Header.Set("Authorization", "Bearer "+credential.Token)
	oversizedRequest.Header.Set("Content-Type", "application/json")
	oversizedResponse, err := http.DefaultClient.Do(oversizedRequest)
	if err != nil {
		t.Fatalf("oversized MCP request: %v", err)
	}
	_ = oversizedResponse.Body.Close()
	if oversizedResponse.StatusCode != http.StatusRequestEntityTooLarge && oversizedResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized MCP request status = %d, want a bounded-request rejection", oversizedResponse.StatusCode)
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
	initialized := session.InitializeResult()
	if initialized == nil || initialized.ServerInfo == nil || initialized.ServerInfo.Name != "vmsh" || initialized.ServerInfo.Version != mcpImplementationVersion() {
		t.Fatalf("MCP server identity = %#v", initialized)
	}

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
		"vm_context_close", "vm_context_exec_start", "vm_context_open", "vm_context_run", "vm_context_status", "vm_copy",
		"vm_create", "vm_exec_cancel", "vm_exec_forget", "vm_exec_start", "vm_exec_status", "vm_exec_wait", "vm_list", "vm_run", "vm_stop",
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

	tooSmall, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_create", Arguments: map[string]any{
		"image": "alpine", "name": "too-small", "memory_mb": 1,
	}})
	if err != nil {
		t.Fatalf("reject undersized VM: %v", err)
	}
	if !tooSmall.IsError {
		t.Fatal("memory_mb below the supported minimum reached the backend")
	}
	mu.Lock()
	if len(starts) != 1 {
		mu.Unlock()
		t.Fatalf("undersized VM reached backend: %#v", starts)
	}
	mu.Unlock()
	bsdMultiCPU, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_create", Arguments: map[string]any{
		"image": "@freebsd", "name": "bsd-smp", "cpus": 2,
	}})
	if err != nil {
		t.Fatalf("reject unsupported BSD CPU count: %v", err)
	}
	if !bsdMultiCPU.IsError {
		t.Fatal("unsupported built-in BSD CPU count reached the backend")
	}
	mu.Lock()
	if len(starts) != 1 {
		mu.Unlock()
		t.Fatalf("unsupported BSD CPU count reached backend: %#v", starts)
	}
	mu.Unlock()

	bsdCreated, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_create", Arguments: map[string]any{
		"image": "@netbsd", "name": "bsd-worker",
	}})
	if err != nil || bsdCreated.IsError {
		t.Fatalf("create built-in BSD VM = %#v, %v", bsdCreated, err)
	}
	mu.Lock()
	if len(starts) != 2 {
		mu.Unlock()
		t.Fatalf("start requests after BSD create = %#v", starts)
	}
	bsdStart := starts[1]
	mu.Unlock()
	if bsdStart.Image != "@netbsd" || bsdStart.MemoryMB != 0 || bsdStart.CPUs != 1 || bsdStart.TimeoutSeconds != mcpBSDDefaultBootTime.Seconds() {
		t.Fatalf("built-in BSD start request = %#v", bsdStart)
	}
	bsdRun, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_run", Arguments: map[string]any{
		"vm_id": bsdStart.ID, "command": []string{"id"},
	}})
	if err != nil || bsdRun.IsError {
		t.Fatalf("run default BSD command = %#v, %v", bsdRun, err)
	}
	mu.Lock()
	if len(runs) != 2 || runs[1].User != "root" || runs[1].WorkDir != "/" {
		mu.Unlock()
		t.Fatalf("default BSD run request = %#v", runs)
	}
	mu.Unlock()
	bsdEscalation, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_run", Arguments: map[string]any{
		"vm_id": bsdStart.ID, "command": []string{"id"}, "user": "1000:1000",
	}})
	if err != nil {
		t.Fatalf("run rejected BSD user: %v", err)
	}
	if !bsdEscalation.IsError {
		t.Fatal("built-in BSD accepted an unsupported non-root user")
	}
	mu.Lock()
	if len(runs) != 2 {
		mu.Unlock()
		t.Fatalf("rejected BSD command reached backend: %#v", runs)
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

func TestMCPCreateVMCancelsBootStreamWithoutClaimingTheVM(t *testing.T) {
	requestCanceled := make(chan struct{})
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vm/start" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Accept") != "application/x-ndjson" {
			t.Error("VM start did not request the boot event stream")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(client.BootEvent{Kind: "status", Message: "cold boot in progress"}); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: make(map[string]mcpVM),
		commands: make(map[string]*mcpCommand), contexts: make(map[string]*mcpGuestContext), stopping: make(map[string]struct{}),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := endpoint.createVM(ctx, nil, mcpCreateVMInput{Image: "@openbsd", Name: "cold-openbsd"}); err == nil {
		t.Fatal("canceled cold boot unexpectedly succeeded")
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("backend start request did not observe caller cancellation")
	}
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if len(endpoint.vms) != 0 || endpoint.starting != 0 {
		t.Fatalf("canceled boot retained MCP ownership: vms=%v starting=%d", endpoint.vms, endpoint.starting)
	}
}

func TestMCPListReportsObservedMemoryAndBackingUsage(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vm" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, []client.InstanceState{{
			ID: "one", Status: "running", MemoryMB: 4096, BalloonMB: 768,
			BackingBytes: 1024, BackingHighWaterBytes: 4096, BackingPhysicalBytes: 2048, BackingReclaimError: "disk refused reclaim",
		}})
	}))
	defer control.Close()
	endpoint := &mcpEndpoint{control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"one": {ID: "one", Image: "alpine"}}}
	_, listed, err := endpoint.listVMs(t.Context(), nil, mcpListVMsInput{})
	if err != nil || listed.ObservationError != "" || len(listed.VMs) != 1 {
		t.Fatalf("list VMs = %#v, %v", listed, err)
	}
	vm := listed.VMs[0]
	if vm.MemoryMB != 4096 || vm.BalloonMB != 768 || vm.BackingBytes != 1024 || vm.BackingHighWaterBytes != 4096 || vm.BackingPhysicalBytes != 2048 || vm.BackingReclaimError != "disk refused reclaim" {
		t.Fatalf("observed VM = %#v", vm)
	}
}

func TestMCPListKeepsQuarantineVisibleAlongsideBackendState(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []client.InstanceState{{ID: "one", Status: "running", MemoryMB: 512}})
	}))
	defer control.Close()
	endpoint := &mcpEndpoint{
		control:     client.NewClient(control.URL, nil),
		vms:         map[string]mcpVM{"one": {ID: "one", Image: "alpine"}},
		quarantined: map[string]struct{}{"one": {}},
	}
	_, listed, err := endpoint.listVMs(t.Context(), nil, mcpListVMsInput{})
	if err != nil || len(listed.VMs) != 1 {
		t.Fatalf("list VMs = %#v, %v", listed, err)
	}
	vm := listed.VMs[0]
	if vm.Status != "quarantined" || !vm.Quarantined || vm.BackendStatus != "running" {
		t.Fatalf("quarantined VM = %#v", vm)
	}
}

func TestMCPCreateReturnsAutomaticMemoryPolicyState(t *testing.T) {
	balloon := newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 4096}})
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			balloon.setAutomatic(req.ID, true)
			balloon.markBalloonRequest(req.ID, 128, time.Now())
			writeMCPBootReady(t, w, r, client.InstanceState{ID: req.ID, Status: "running", Image: req.Image, MemoryMB: 4096, BalloonMB: 128, BalloonStatus: "inflating"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	endpoint := &mcpEndpoint{control: client.NewClient(control.URL, nil), balloon: balloon, vms: make(map[string]mcpVM)}
	_, created, err := endpoint.createVM(t.Context(), nil, mcpCreateVMInput{Image: "alpine", Name: "automatic"})
	if err != nil {
		t.Fatal(err)
	}
	if !created.VM.AutomaticMemory || !created.VM.BalloonPolicyInFlight {
		t.Fatalf("create response policy = %#v", created.VM)
	}
}

func TestMCPStopReapsCrashedBackendAndReleasesName(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vm/shutdown":
			http.Error(w, `no VM "dead" is running`, http.StatusConflict)
		case "/vm":
			writeJSON(w, http.StatusOK, []client.InstanceState{{ID: "dead", Status: "crashed", Error: "guest exited", ExitReason: "guest exited", ExitedAt: "2026-07-19T00:00:00Z"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	endpoint := &mcpEndpoint{
		control:     client.NewClient(control.URL, nil),
		vms:         map[string]mcpVM{"dead": {ID: "dead", Name: "reusable", Image: "alpine", Status: "crashed"}},
		commands:    make(map[string]*mcpCommand),
		contexts:    make(map[string]*mcpGuestContext),
		stopping:    make(map[string]struct{}),
		quarantined: make(map[string]struct{}),
	}
	_, output, err := endpoint.stopVM(t.Context(), nil, mcpStopVMInput{VMID: "dead"})
	if err != nil || !output.Stopped || output.PreviousState != "crashed" || output.ExitReason != "guest exited" {
		t.Fatalf("reap crashed VM = %#v, %v", output, err)
	}
	endpoint.mu.Lock()
	_, retained := endpoint.vms["dead"]
	endpoint.mu.Unlock()
	if retained {
		t.Fatal("crashed VM ownership was retained")
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
		if len(req.Command) != 0 && req.Command[0] == "exit124" {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
				return
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 124})
			return
		}
		if len(req.Command) != 0 && req.Command[0] == "timeout" {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
				return
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "timeout"})
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 124})
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
	ordinary124, err := endpoint.startCommand(mcpRunVMInput{VMID: "owned", Command: []string{"exit124"}, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	<-ordinary124.done
	ordinaryResult := ordinary124.snapshot(0, 0, 1024, false)
	if ordinaryResult.Status != "exited" || ordinaryResult.ExitCode == nil || *ordinaryResult.ExitCode != 124 {
		t.Fatalf("ordinary exit 124 result = %#v", ordinaryResult)
	}
	if _, err := endpoint.ownedVM("owned"); err != nil {
		t.Fatalf("ordinary exit 124 stopped its VM: %v", err)
	}
	timeoutCommand, err := endpoint.startCommand(mcpRunVMInput{VMID: "owned", Command: []string{"timeout"}, TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	<-timeoutCommand.done
	timeoutResult := timeoutCommand.snapshot(0, 0, 1024, false)
	if timeoutResult.Status != "timed_out" || timeoutResult.ExitCode == nil || *timeoutResult.ExitCode != 124 {
		t.Fatalf("structured timeout result = %#v", timeoutResult)
	}
	rootTimeout, err := endpoint.startCommand(mcpRunVMInput{VMID: "owned", Command: []string{"timeout"}, TimeoutSeconds: 1, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	<-rootTimeout.done
	rootTimeoutResult := rootTimeout.snapshot(0, 0, 1024, false)
	if rootTimeoutResult.Status != "termination_unconfirmed" || rootTimeoutResult.ExitCode != nil || rootTimeoutResult.ContainmentError == "" {
		t.Fatalf("privileged timeout result = %#v", rootTimeoutResult)
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

func TestMCPForgetReleasesCompletedCommandPayload(t *testing.T) {
	done := make(chan struct{})
	close(done)
	command := &mcpCommand{
		id: "finished", vmID: "one", done: done, status: "exited",
		request: client.RunRequest{Command: []string{"large"}, Env: []string{"TOKEN=value"}},
		stdin:   make([]byte, 1<<20), stdout: make([]byte, 4<<20), stderr: make([]byte, 2<<20),
	}
	endpoint := &mcpEndpoint{commands: map[string]*mcpCommand{command.id: command}}
	_, result, err := endpoint.forgetVMCommand(t.Context(), nil, mcpCommandForgetInput{CommandID: command.id})
	if err != nil || !result.Forgotten {
		t.Fatalf("forget = %#v, %v", result, err)
	}
	if _, err := endpoint.command(command.id); err == nil {
		t.Fatal("forgotten command remained addressable")
	}
	command.mu.Lock()
	defer command.mu.Unlock()
	if command.stdin != nil || command.stdout != nil || command.stderr != nil || len(command.request.Command) != 0 || len(command.request.Env) != 0 {
		t.Fatal("forgotten command retained request or stream payloads")
	}
}

func TestMCPCompletedOutputCanBeReplayedUntilForgotten(t *testing.T) {
	done := make(chan struct{})
	close(done)
	command := &mcpCommand{
		id: "delivered", vmID: "one", done: done, status: "exited",
		stdout: []byte("stdout"), stderr: []byte("stderr"), stdoutTotal: 6, stderrTotal: 6,
		request: client.RunRequest{Command: []string{"complete"}}, stdin: []byte("input"),
	}
	first := command.deliver(0, 0, 3, false)
	if first.Stdout.Text != "std" || first.Stderr.Text != "std" || command.stdout == nil {
		t.Fatalf("first page = %#v", first)
	}
	last := command.deliver(3, 3, 3, false)
	if last.Stdout.Text != "out" || last.Stderr.Text != "err" {
		t.Fatalf("last page = %#v", last)
	}
	replayed := command.deliver(0, 0, 16, false)
	if replayed.Stdout.Text != "stdout" || replayed.Stderr.Text != "stderr" || replayed.Stdout.Truncated || replayed.Stderr.Truncated {
		t.Fatalf("replayed output = %#v", replayed)
	}
	oversized := command.deliver(1<<20, 1<<20, 16, false)
	if oversized.Stdout.NextOffset != 6 || oversized.Stderr.NextOffset != 6 {
		t.Fatalf("oversized cursor = %#v", oversized)
	}
	replayed = command.deliver(0, 0, 16, false)
	if replayed.Stdout.Text != "stdout" || replayed.Stderr.Text != "stderr" {
		t.Fatalf("output after oversized cursor = %#v", replayed)
	}
}

func TestMCPCompletedReplayEvictsPayloadButKeepsObservableStatus(t *testing.T) {
	now := time.Now().UTC()
	endpoint := &mcpEndpoint{commands: make(map[string]*mcpCommand)}
	for i := 0; i < mcpCompletedOutputCount+1; i++ {
		finished := now.Add(time.Duration(i) * time.Second)
		id := fmt.Sprintf("command-%02d", i)
		endpoint.commands[id] = &mcpCommand{
			id: id, vmID: "one", status: "exited", finishedAt: &finished,
			stdout: make([]byte, 1<<20), stdoutTotal: 1 << 20,
		}
	}
	endpoint.pruneCompletedCommands(now.Add(time.Duration(mcpCompletedOutputCount+2) * time.Second))
	oldest := endpoint.commands["command-00"].snapshot(0, 0, 1024, false)
	if !oldest.OutputExpired || oldest.Status != "exited" || oldest.Stdout.TotalBytes != 1<<20 || !oldest.Stdout.Truncated {
		t.Fatalf("expired replay status = %#v", oldest)
	}
	newest := endpoint.commands[fmt.Sprintf("command-%02d", mcpCompletedOutputCount)].snapshot(0, 0, 1024, false)
	if newest.OutputExpired || newest.Stdout.NextOffset != 1024 {
		t.Fatalf("newest replay was not retained: %#v", newest)
	}
}

func TestMCPEndpointCloseShutsVMsConcurrentlyAndRetainsFailedOwnership(t *testing.T) {
	fastCalled := make(chan struct{}, 1)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vm/shutdown":
			if r.URL.Query().Get("id") == "wedged" {
				<-r.Context().Done()
				return
			}
			fastCalled <- struct{}{}
			writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
		case "/vm":
			writeJSON(w, http.StatusOK, []client.InstanceState{{ID: "wedged", Status: "running"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"wedged": {ID: "wedged"}, "fast": {ID: "fast"}},
		commands: make(map[string]*mcpCommand), contexts: make(map[string]*mcpGuestContext), stopping: make(map[string]struct{}), quarantined: make(map[string]struct{}),
		shutdownTimeout: 40 * time.Millisecond,
	}
	started := time.Now()
	if err := endpoint.close(); err == nil {
		t.Fatal("endpoint close hid the failed backend shutdown")
	}
	if time.Since(started) > time.Second {
		t.Fatalf("concurrent endpoint close took %s", time.Since(started))
	}
	select {
	case <-fastCalled:
	default:
		t.Fatal("a wedged VM prevented shutdown of another VM")
	}
	endpoint.mu.Lock()
	_, fastRetained := endpoint.vms["fast"]
	_, wedgedRetained := endpoint.vms["wedged"]
	_, quarantined := endpoint.quarantined["wedged"]
	endpoint.mu.Unlock()
	if fastRetained || !wedgedRetained || !quarantined {
		t.Fatalf("close recovery state: fast=%v wedged=%v quarantined=%v", fastRetained, wedgedRetained, quarantined)
	}
}

func TestMCPUnconfirmedTerminationIsNotReportedAsCanceled(t *testing.T) {
	started := make(chan struct{})
	control := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.RunRequest
		if websocket.JSON.Receive(ws, &req) != nil {
			return
		}
		var input client.ExecInput
		if websocket.JSON.Receive(ws, &input) != nil {
			return
		}
		close(started)
		for {
			if websocket.JSON.Receive(ws, &input) != nil {
				return
			}
		}
	}))
	defer control.Close()
	ctx, cancel := context.WithCancel(context.Background())
	command := &mcpCommand{
		id: "escaped", vmID: "one", request: client.RunRequest{Command: []string{"daemonize"}},
		inputs: make(chan client.ExecInput), cancel: cancel, done: make(chan struct{}), status: "running", startedAt: time.Now(),
	}
	go command.run(ctx, client.NewClient(control.URL, nil))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}
	command.mu.Lock()
	command.cancelRequested = true
	command.mu.Unlock()
	command.markTerminationUnconfirmed("host could not confirm descendant termination")
	cancel()
	select {
	case <-command.done:
	case <-time.After(time.Second):
		t.Fatal("command stream did not end")
	}
	result := command.snapshot(0, 0, 1024, false)
	if result.Status != "termination_unconfirmed" || result.ExitCode != nil || result.ContainmentError == "" {
		t.Fatalf("unconfirmed termination = %#v", result)
	}
}

func TestMCPPrivilegedCancellationDoesNotClaimDetachedDescendantsWereReaped(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	commandHandler := websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.RunRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
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
	})
	mux.Handle("/vm/run", commandHandler)
	mux.Handle("/vm/run/stream", commandHandler)
	control := httptest.NewServer(mux)
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"owned": {ID: "owned"}}, stopping: make(map[string]struct{}),
		commands: make(map[string]*mcpCommand), artifacts: make(map[string]*mcpArtifact), contexts: make(map[string]*mcpGuestContext),
	}
	command, err := endpoint.startCommand(mcpRunVMInput{VMID: "owned", Command: []string{"sleep", "60"}, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("privileged command did not start")
	}
	_, result, err := endpoint.cancelVMCommand(t.Context(), nil, mcpCommandCancelInput{CommandID: command.id})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "termination_unconfirmed" || result.ExitCode != nil || result.ContainmentAction == "" || result.ContainmentError == "" {
		t.Fatalf("privileged cancellation result = %#v", result)
	}
	if _, err := endpoint.ownedVM("owned"); err != nil {
		t.Fatalf("privileged cancellation stopped its VM: %v", err)
	}
}

func TestMCPCommandCancellationReportsUnresponsiveControlStream(t *testing.T) {
	command := &mcpCommand{
		id: "blocked", vmID: "owned", inputs: make(chan client.ExecInput), done: make(chan struct{}),
		status: "running", cancel: func() {}, startedAt: time.Now().UTC(),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		command.terminate()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation blocked forever on an unresponsive guest control stream")
	}
	result := command.snapshot(0, 0, 1024, false)
	if result.ContainmentAction == "" || result.ContainmentError == "" {
		t.Fatalf("unconfirmed cancellation result = %#v", result)
	}
}

func TestMCPCreateRefusesLegacyRecoveryAsAnIsolatedGuest(t *testing.T) {
	endpoint := &mcpEndpoint{vms: make(map[string]mcpVM), stopping: make(map[string]struct{})}
	if _, _, err := endpoint.createVM(t.Context(), nil, mcpCreateVMInput{Image: "vmsh-mcp-recovery-freebsd-test"}); err == nil {
		t.Fatal("legacy recovery filesystem export was presented as an isolated bootable guest")
	}
}

func TestMCPOutputUsesBinaryEncodingWhenJSONTextWouldExpand(t *testing.T) {
	data := bytes.Repeat([]byte{0}, 1024)
	chunk := commandOutputChunk(data, int64(len(data)), false, 0, len(data))
	if chunk.Text != "" || chunk.Base64 == "" {
		t.Fatalf("NUL-heavy output encoding = %#v", chunk)
	}
	plain := commandOutputChunk([]byte("ordinary text\n"), 14, false, 0, 14)
	if plain.Text != "ordinary text\n" || plain.Base64 != "" {
		t.Fatalf("plain output encoding = %#v", plain)
	}
}

func TestMCPArtifactOperationsReserveSessionMemory(t *testing.T) {
	endpoint := &mcpEndpoint{artifacts: make(map[string]*mcpArtifact)}
	reservations := make([]*mcpArtifactReservation, 0, mcpMaxArtifactOperations)
	for range mcpMaxArtifactOperations {
		reservation, err := endpoint.beginArtifactOperation(mcpMaxArtifactBytes)
		if err != nil {
			t.Fatalf("reserve bounded artifact operation: %v", err)
		}
		reservations = append(reservations, reservation)
	}
	if _, err := endpoint.beginArtifactOperation(0); err == nil {
		t.Fatal("artifact operation exceeded the session concurrency limit")
	}
	for _, reservation := range reservations {
		reservation.release()
	}
	reservation, err := endpoint.beginArtifactOperation(mcpMaxArtifactBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.release()
	if _, err := reservation.storeArtifact(strings.Repeat("x", mcpMaxArtifactNameBytes+1), "vm", "/file", []byte("data")); err == nil {
		t.Fatal("oversized retained artifact label was accepted")
	}
}

func TestMCPArchiveRejectsOversizedSparseLogicalFile(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	logicalSize := int64(mcpMaxArtifactBytes + 1)
	header := &tar.Header{
		Name: "sparse", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: tar.FormatPAX,
		PAXRecords: map[string]string{
			"VMSH.sparse.size":      strconv.FormatInt(logicalSize, 10),
			"VMSH.sparse.numblocks": "1",
			"VMSH.sparse.map":       strconv.FormatInt(logicalSize-1, 10) + ",1",
		},
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte{'x'})
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateMCPArchive(archive.Bytes()); err == nil {
		t.Fatal("oversized sparse logical file passed MCP archive validation")
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
	var lateContextOutputSent bool
	var archivedPath, extractedPath, contextWorkDir string
	mux := http.NewServeMux()
	streamHandler := websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.ExecRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		if len(req.Command) >= 3 && (strings.Contains(req.Command[2], `tail -c`) || strings.Contains(req.Command[2], `wc -c`) || strings.HasPrefix(req.Command[2], "rm -f ") || slices.Equal(req.Command[:2], []string{"/bin/rm", "-f"})) {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
				return
			}
			if len(req.Command) >= 3 && strings.Contains(req.Command[2], `wc -c`) {
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte("0\n")})
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
			return
		}
		if req.ControlFD {
			contextWorkDir = req.WorkDir
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
			archivedPath = req.Path
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
			extractedPath = req.Path
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
			markerStart := strings.LastIndex(script, `\036marker_`)
			markerEnd := strings.Index(script[markerStart+4:], ":%s")
			if markerStart < 0 || markerEnd < 0 {
				return
			}
			marker := script[markerStart+4 : markerStart+4+markerEnd]
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "control", Data: []byte("\x1e" + marker + ":begin\x1f\n")})
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
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "control", Data: []byte("\x1e" + marker + ":0\x1f\n")})
			if strings.Contains(script, "export ANSWER=42") && !lateContextOutputSent {
				lateContextOutputSent = true
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("late-from-previous\n")})
			}
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

	_, exported, err := endpoint.exportArtifact(context.Background(), nil, mcpArtifactExportInput{VMID: "one", Path: "/project "})
	if err != nil {
		t.Fatalf("export artifact: %v", err)
	}
	if exported.Artifact.Size != int64(len(archive)) || exported.Artifact.SHA256 == "" || exported.Artifact.Source != "/project " || archivedPath != "/project " {
		t.Fatalf("artifact metadata = %#v", exported.Artifact)
	}
	if _, _, err := endpoint.importArtifact(context.Background(), nil, mcpArtifactImportInput{ArtifactID: exported.Artifact.ID, VMID: "two", Path: "/work "}); err != nil {
		t.Fatalf("import artifact: %v", err)
	}
	if extractedPath != "/work " {
		t.Fatalf("artifact destination path = %q", extractedPath)
	}
	if !bytes.Equal(extracted, archive) {
		t.Fatalf("extracted archive size = %d, want %d", len(extracted), len(archive))
	}
	if largestInput > mcpArtifactInputChunk {
		t.Fatalf("largest artifact input = %d, want at most %d", largestInput, mcpArtifactInputChunk)
	}

	_, opened, err := endpoint.openGuestContext(context.Background(), nil, mcpContextOpenInput{VMID: "one", WorkDir: "/tmp/work "})
	if err != nil {
		t.Fatalf("open context: %v", err)
	}
	if contextWorkDir != "/tmp/work " {
		t.Fatalf("context workdir = %q", contextWorkDir)
	}
	if _, _, err := endpoint.runGuestContext(context.Background(), nil, mcpContextRunInput{ContextID: opened.ContextID, CommandLine: "export ANSWER=42"}); err != nil {
		t.Fatalf("export in context: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	_, result, err := endpoint.runGuestContext(context.Background(), nil, mcpContextRunInput{ContextID: opened.ContextID, CommandLine: `printf '%s' "$ANSWER"; printf problem >&2`})
	if err != nil {
		t.Fatalf("read context state: %v", err)
	}
	if result.Stdout != "42" || result.Stderr != "problem" || result.AsyncStdout != "late-from-previous\n" || result.ExitCode != 0 || result.CommandStatus != "exited" || result.ContextStatus != "running" {
		t.Fatalf("context result = %#v", result)
	}
	_, asyncResult, err := endpoint.startGuestContextCommand(context.Background(), nil, mcpContextRunInput{
		ContextID: opened.ContextID, CommandLine: `printf '%s' "$ANSWER"; printf problem >&2`,
	})
	if err != nil {
		t.Fatalf("start async context command: %v", err)
	}
	asyncCommand, err := endpoint.command(asyncResult.CommandID)
	if err != nil {
		t.Fatalf("find async context command: %v", err)
	}
	select {
	case <-asyncCommand.done:
	case <-time.After(time.Second):
		t.Fatal("async context command did not finish")
	}
	asyncResult = asyncCommand.snapshot(0, 0, mcpDefaultOutputChunk, false)
	if asyncResult.ContextID != opened.ContextID || asyncResult.Status != "exited" || asyncResult.ExitCode == nil || *asyncResult.ExitCode != 0 || asyncResult.Stdout.Text != "42" || asyncResult.Stderr.Text != "problem" {
		t.Fatalf("async context result = %#v", asyncResult)
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

func TestMCPContextFramingPreservesUserDescriptors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell and inherited file descriptors")
	}
	t.Run("system shell", func(t *testing.T) {
		testMCPContextFramingPreservesUserDescriptors(t, "/bin/sh")
	})
	t.Run("BusyBox ash", func(t *testing.T) {
		busybox, err := exec.LookPath("busybox")
		if err != nil {
			t.Skip("BusyBox is not installed")
		}
		shellPath := filepath.Join(t.TempDir(), "sh")
		if err := os.Symlink(busybox, shellPath); err != nil {
			t.Fatal(err)
		}
		testMCPContextFramingPreservesUserDescriptors(t, shellPath)
	})
}

func testMCPContextFramingPreservesUserDescriptors(t *testing.T, shellPath string) {
	t.Helper()
	controlPath := filepath.Join(t.TempDir(), "context-control")
	userFile := filepath.Join(t.TempDir(), "user-fd9")
	controlR, controlW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlR.Close()

	cmd := exec.Command("/bin/sh", "-c", mcpContextShellScriptForShell(controlPath, shellPath))
	cmd.Stdin = strings.NewReader(
		mcpContextCommandScript("marker_first", "exec 3>&-; exec 9>"+shellJoin([]string{userFile})+"; echo first >&9", controlPath) +
			mcpContextCommandScript("marker_second", "echo second >&9; echo context-still-running", controlPath) +
			mcpContextCommandScript("marker_third", "if IFS= read -r stolen; then echo unexpected-input; else echo stdin-eof; fi", controlPath),
	)
	cmd.ExtraFiles = []*os.File{controlW}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run persistent shell framing: %v; stderr=%q", err, stderr.String())
	}
	if err := controlW.Close(); err != nil {
		t.Fatal(err)
	}
	control, err := io.ReadAll(controlR)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "context-still-running\nstdin-eof\n"; got != want {
		t.Fatalf("shell output = %q, want %q", got, want)
	}
	if got, want := string(control), "\x1emarker_first:begin\x1f\n\x1emarker_first:0\x1f\n\x1emarker_second:begin\x1f\n\x1emarker_second:0\x1f\n\x1emarker_third:begin\x1f\n\x1emarker_third:0\x1f\n"; got != want {
		t.Fatalf("control frames = %q, want %q", got, want)
	}
	userData, err := os.ReadFile(userFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(userData), "first\nsecond\n"; got != want {
		t.Fatalf("persistent fd 9 output = %q, want %q", got, want)
	}
}

func TestMCPContextCaptureKeepsBackgroundOutputWithItsOriginatingCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	tempDir := t.TempDir()
	controlPath := filepath.Join(tempDir, "control")
	firstOut := filepath.Join(tempDir, "first.stdout")
	firstErr := filepath.Join(tempDir, "first.stderr")
	secondOut := filepath.Join(tempDir, "second.stdout")
	secondErr := filepath.Join(tempDir, "second.stderr")
	controlR, controlW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlR.Close()
	cmd := exec.Command("/bin/sh", "-c", mcpContextShellScriptForShell(controlPath, "/bin/sh"))
	cmd.Stdin = strings.NewReader(
		mcpContextCommandCaptureScript("marker_first", "(sleep 0.1; printf late) & printf first", controlPath, firstOut, firstErr) +
			mcpContextCommandCaptureScript("marker_second", "sleep 0.2; printf second", controlPath, secondOut, secondErr),
	)
	cmd.ExtraFiles = []*os.File{controlW}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run persistent capture shell: %v; stderr=%q", err, stderr.String())
	}
	_ = controlW.Close()
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("captured command output leaked onto the persistent stream: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	first, err := os.ReadFile(firstOut)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(secondOut)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "firstlate" || string(second) != "second" {
		t.Fatalf("capture provenance first=%q second=%q", first, second)
	}
}

func TestMCPContextCaptureDoesNotAlterUserResourceLimitsOrInitialUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	tempDir := t.TempDir()
	controlPath := filepath.Join(tempDir, "private", "control")
	firstOut := filepath.Join(tempDir, "private", "first.stdout")
	firstErr := filepath.Join(tempDir, "private", "first.stderr")
	secondOut := filepath.Join(tempDir, "private", "second.stdout")
	secondErr := filepath.Join(tempDir, "private", "second.stderr")
	thirdOut := filepath.Join(tempDir, "private", "third.stdout")
	thirdErr := filepath.Join(tempDir, "private", "third.stderr")
	userFile := filepath.Join(tempDir, "user.data")
	controlR, controlW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlR.Close()
	cmd := exec.Command("/bin/sh", "-c", "umask 022; "+mcpContextShellScriptForShell(controlPath, "/bin/sh"))
	cmd.Stdin = strings.NewReader(
		mcpContextCommandCaptureScript("marker_one", "umask; ulimit -f", controlPath, firstOut, firstErr) +
			mcpContextCommandCaptureScript("marker_two", "umask 777", controlPath, secondOut, secondErr) +
			mcpContextCommandCaptureScript("marker_three", "ulimit -f; dd if=/dev/zero of="+shellJoin([]string{userFile})+" bs=1048576 count=5 2>/dev/null", controlPath, thirdOut, thirdErr),
	)
	cmd.ExtraFiles = []*os.File{controlW}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run bounded context captures: %v; output=%q", err, output)
	}
	_ = controlW.Close()
	for _, name := range []string{firstOut, firstErr, secondOut, secondErr, thirdOut, thirdErr} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("capture %s mode = %#o", filepath.Base(name), info.Mode().Perm())
		}
	}
	first, err := os.ReadFile(firstOut)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(first))
	if len(lines) < 2 || lines[0] != "0022" {
		t.Fatalf("initial context umask/resource output = %q", first)
	}
	third, err := os.Stat(userFile)
	if err != nil {
		t.Fatal(err)
	}
	if third.Size() != 5<<20 {
		t.Fatalf("user output file was restricted to %d bytes", third.Size())
	}
}

func TestMCPGuestWorkDirPreservesNonemptyBytes(t *testing.T) {
	linuxVM := mcpVM{Image: "alpine"}
	if got := mcpGuestWorkDir(linuxVM, " "); got != " " {
		t.Fatalf("whitespace workdir = %q", got)
	}
	if got := mcpGuestWorkDir(linuxVM, ""); got != "/home/cc" {
		t.Fatalf("default Linux workdir = %q", got)
	}
}

func TestMCPContextPreservesOutputAccountingTimeoutBytesAndPrivateFraming(t *testing.T) {
	const largeBytes = 5 << 20
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
		if len(req.Command) >= 3 && strings.Contains(req.Command[2], `wc -c`) {
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte("0\n")})
		}
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
	}))
	mux.Handle("/vm/run/stream", websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var req client.RunRequest
		if err := websocket.JSON.Receive(ws, &req); err != nil {
			return
		}
		if !req.ControlFD {
			t.Error("persistent shell did not request a private control fd")
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
			markerStart := strings.LastIndex(script, `\036marker_`)
			if markerStart < 0 {
				t.Error("context script omitted its status marker")
				return
			}
			markerEnd := strings.Index(script[markerStart+4:], ":%s")
			if markerEnd < 0 {
				t.Error("context script contained an incomplete status marker")
				return
			}
			marker := script[markerStart+4 : markerStart+4+markerEnd]
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "control", Data: []byte("\x1e" + marker + ":begin\x1f\n")})
			switch {
			case strings.Contains(script, "large-context-output"):
				chunk := bytes.Repeat([]byte{'x'}, 1<<20)
				for sent := 0; sent < largeBytes; sent += len(chunk) {
					_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: chunk})
				}
			case strings.Contains(script, "printf() { :; }"):
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("function-defined\n")})
			case strings.Contains(script, "fd3-closed"):
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("fd3-closed\n")})
			case strings.Contains(script, "before-timeout"):
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: []byte("before-timeout\n")})
				continue
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "control", Data: []byte("\x1e" + marker + ":0\x1f\n")})
		}
	}))
	control := httptest.NewServer(mux)
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"one": {ID: "one", Image: "alpine"}},
		commands: make(map[string]*mcpCommand), contexts: make(map[string]*mcpGuestContext), stopping: make(map[string]struct{}),
	}
	_, opened, err := endpoint.openGuestContext(t.Context(), nil, mcpContextOpenInput{VMID: "one"})
	if err != nil {
		t.Fatalf("open context: %v", err)
	}
	_, started, err := endpoint.startGuestContextCommand(t.Context(), nil, mcpContextRunInput{ContextID: opened.ContextID, CommandLine: "large-context-output"})
	if err != nil {
		t.Fatalf("start large context output: %v", err)
	}
	largeCommand, err := endpoint.command(started.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-largeCommand.done:
	case <-time.After(3 * time.Second):
		t.Fatal("large context output did not finish")
	}
	large := largeCommand.snapshot(0, 0, mcpMaxOutputChunk, false)
	if large.Status != "exited" || large.Stdout.TotalBytes != largeBytes || !large.Stdout.Truncated || large.Stdout.NextOffset != mcpMaxCommandStreamBytes {
		t.Fatalf("large context output accounting = %#v", large)
	}
	_, framed, err := endpoint.runGuestContext(t.Context(), nil, mcpContextRunInput{
		ContextID: opened.ContextID, CommandLine: "printf() { :; }; echo function-defined", TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("run with colliding shell function: %v", err)
	}
	if framed.CommandStatus != "exited" || framed.ContextStatus != "running" || framed.Stdout != "function-defined\n" || framed.StdoutTotalBytes != int64(len(framed.Stdout)) {
		t.Fatalf("function-safe context result = %#v", framed)
	}
	_, fdClosed, err := endpoint.runGuestContext(t.Context(), nil, mcpContextRunInput{
		ContextID: opened.ContextID, CommandLine: "exec 3>&-; echo fd3-closed", TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("run after closing fd 3: %v", err)
	}
	if fdClosed.CommandStatus != "exited" || fdClosed.ContextStatus != "running" || fdClosed.ExitCode != 0 || fdClosed.Stdout != "fd3-closed\n" {
		t.Fatalf("fd-3-safe context result = %#v", fdClosed)
	}
	_, timedStart, err := endpoint.startGuestContextCommand(t.Context(), nil, mcpContextRunInput{
		ContextID: opened.ContextID, CommandLine: "echo before-timeout; sleep 20", TimeoutSeconds: 0.05,
	})
	if err != nil {
		t.Fatalf("start timed context command: %v", err)
	}
	timedCommand, err := endpoint.command(timedStart.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-timedCommand.done:
	case <-time.After(time.Second):
		t.Fatal("timed context command did not settle")
	}
	timed := timedCommand.snapshot(0, 0, mcpDefaultOutputChunk, false)
	if timed.Status != "timed_out" || timed.ExitCode == nil || *timed.ExitCode != 124 || timed.Stdout.Text != "before-timeout\n" || timed.Stdout.TotalBytes != int64(len("before-timeout\n")) {
		t.Fatalf("timed context output = %#v", timed)
	}
}

func TestMCPAsyncContextCommandsAllowSequentialLifecycleInterruption(t *testing.T) {
	active := make(chan struct{}, 2)
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
		if len(req.Command) >= 3 && strings.Contains(req.Command[2], `wc -c`) {
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stderr", Data: []byte("0\n")})
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
				markerStart := strings.LastIndex(script, `\036marker_`)
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
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "control", Data: []byte("\x1e" + marker + ":begin\x1f\n")})
				_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "control", Data: []byte("\x1e" + marker + ":0\x1f\n")})
				continue
			}
			active <- struct{}{}
		}
	}))
	mux.HandleFunc("/vm/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
	})
	control := httptest.NewServer(mux)
	defer control.Close()
	endpoint := &mcpEndpoint{
		control: client.NewClient(control.URL, nil), credentials: map[string]string{"test": "token"}, vms: map[string]mcpVM{"one": {ID: "one"}},
		commands: make(map[string]*mcpCommand), artifacts: make(map[string]*mcpArtifact), contexts: make(map[string]*mcpGuestContext),
	}
	_, opened, err := endpoint.openGuestContext(t.Context(), nil, mcpContextOpenInput{VMID: "one"})
	if err != nil {
		t.Fatalf("open context: %v", err)
	}
	mcpServer := httptest.NewServer(endpoint.handler())
	defer mcpServer.Close()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "vmsh-lifecycle-test", Version: "1"}, nil)
	session, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   mcpServer.URL + "/mcp",
		HTTPClient: &http.Client{Transport: mcpBearerTransport{token: "token"}},
	}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer session.Close()
	startedCommand, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_context_exec_start", Arguments: map[string]any{
		"context_id": opened.ContextID, "command_line": "sleep 60",
	}})
	if err != nil || startedCommand.IsError {
		t.Fatalf("start context command = %#v, %v", startedCommand, err)
	}
	startedOutput := structuredToolOutput[mcpCommandOutput](t, startedCommand)
	if startedOutput.ContextID != opened.ContextID || startedOutput.CommandID == "" || startedOutput.Status != "running" {
		t.Fatalf("started context command = %#v", startedOutput)
	}
	select {
	case <-active:
	case <-time.After(time.Second):
		t.Fatal("context command did not reach the shell stream")
	}
	started := time.Now()
	closed, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_context_close", Arguments: map[string]any{
		"context_id": opened.ContextID,
	}})
	if err != nil || closed.IsError {
		t.Fatalf("close context = %#v, %v", closed, err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("context close took %s", time.Since(started))
	}
	status, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_exec_status", Arguments: map[string]any{
		"command_id": startedOutput.CommandID,
	}})
	if err != nil || status.IsError {
		t.Fatalf("read context command after close = %#v, %v", status, err)
	}
	statusOutput := structuredToolOutput[mcpCommandOutput](t, status)
	if statusOutput.Status != "canceled" || statusOutput.ExitCode == nil || *statusOutput.ExitCode != 130 {
		t.Fatalf("context command after close = %#v", statusOutput)
	}

	_, opened, err = endpoint.openGuestContext(t.Context(), nil, mcpContextOpenInput{VMID: "one"})
	if err != nil {
		t.Fatalf("open second context: %v", err)
	}
	startedCommand, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_context_exec_start", Arguments: map[string]any{
		"context_id": opened.ContextID, "command_line": "sleep 60",
	}})
	if err != nil || startedCommand.IsError {
		t.Fatalf("start second context command = %#v, %v", startedCommand, err)
	}
	startedOutput = structuredToolOutput[mcpCommandOutput](t, startedCommand)
	if startedOutput.ContextID != opened.ContextID || startedOutput.CommandID == "" || startedOutput.Status != "running" {
		t.Fatalf("started second context command = %#v", startedOutput)
	}
	select {
	case <-active:
	case <-time.After(time.Second):
		t.Fatal("second context command did not reach the shell stream")
	}
	started = time.Now()
	stopped, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_stop", Arguments: map[string]any{"vm_id": "one"}})
	if err != nil || stopped.IsError {
		t.Fatalf("stop VM = %#v, %v", stopped, err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("VM stop took %s", time.Since(started))
	}
	status, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_exec_status", Arguments: map[string]any{
		"command_id": startedOutput.CommandID,
	}})
	if err != nil || !status.IsError {
		t.Fatalf("VM stop retained context command metadata = %#v, %v", status, err)
	}
}

func TestMCPAsyncContextStartRacesWithCloseAndStop(t *testing.T) {
	shutdown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vm/shutdown" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
	}))
	defer shutdown.Close()

	for i := 0; i < 64; i++ {
		t.Run(fmt.Sprintf("close-%d", i), func(t *testing.T) {
			guest := inertMCPGuestContext("context", "one")
			endpoint := &mcpEndpoint{
				vms: map[string]mcpVM{"one": {ID: "one"}}, commands: make(map[string]*mcpCommand),
				contexts: map[string]*mcpGuestContext{guest.id: guest}, stopping: make(map[string]struct{}),
			}
			startResult := make(chan mcpCommandOutput, 1)
			startErr := make(chan error, 1)
			go func() {
				_, result, err := endpoint.startGuestContextCommand(t.Context(), nil, mcpContextRunInput{ContextID: guest.id, CommandLine: "sleep 60"})
				startResult <- result
				startErr <- err
			}()
			_, closed, closeErr := endpoint.closeGuestContext(t.Context(), nil, mcpContextStatusInput{ContextID: guest.id})
			if closeErr != nil || !closed.Closed {
				t.Fatalf("close context = %#v, %v", closed, closeErr)
			}
			result, err := <-startResult, <-startErr
			if err == nil {
				assertMCPCommandCanceled(t, endpoint, result.CommandID)
			}
			assertNoRunningMCPCommands(t, endpoint)
		})

		t.Run(fmt.Sprintf("stop-%d", i), func(t *testing.T) {
			guest := inertMCPGuestContext("context", "one")
			endpoint := &mcpEndpoint{
				control: client.NewClient(shutdown.URL, nil), vms: map[string]mcpVM{"one": {ID: "one"}},
				commands: make(map[string]*mcpCommand), contexts: map[string]*mcpGuestContext{guest.id: guest}, stopping: make(map[string]struct{}),
			}
			startResult := make(chan mcpCommandOutput, 1)
			startErr := make(chan error, 1)
			go func() {
				_, result, err := endpoint.startGuestContextCommand(t.Context(), nil, mcpContextRunInput{ContextID: guest.id, CommandLine: "sleep 60"})
				startResult <- result
				startErr <- err
			}()
			_, stopped, stopErr := endpoint.stopVM(t.Context(), nil, mcpStopVMInput{VMID: "one"})
			if stopErr != nil || !stopped.Stopped {
				t.Fatalf("stop VM = %#v, %v", stopped, stopErr)
			}
			result, err := <-startResult, <-startErr
			if err == nil {
				assertMCPCommandCanceledOrReaped(t, endpoint, result.CommandID)
			}
			assertNoRunningMCPCommands(t, endpoint)
		})
	}
}

func TestMCPCommandStartIsRejectedOnceVMStopBegins(t *testing.T) {
	endpoint := &mcpEndpoint{
		vms: map[string]mcpVM{"one": {ID: "one"}}, commands: make(map[string]*mcpCommand),
		stopping: map[string]struct{}{"one": {}},
	}
	_, _, err := endpoint.startVMCommand(t.Context(), nil, mcpRunVMInput{VMID: "one", Command: []string{"true"}})
	if err == nil {
		t.Fatal("started a command after VM stop began")
	}
	assertNoRunningMCPCommands(t, endpoint)
}

func TestMCPContextAdmissionPrunesClosedContextsWithoutArbitraryCeiling(t *testing.T) {
	endpoint := &mcpEndpoint{contexts: make(map[string]*mcpGuestContext)}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("context-%d", i)
		endpoint.contexts[id] = &mcpGuestContext{id: id, done: make(chan struct{})}
	}
	if err := endpoint.reserveGuestContext(); err != nil {
		t.Fatalf("arbitrary context count prevented admission: %v", err)
	}
	endpoint.releaseGuestContextReservation()
	close(endpoint.contexts["context-0"].done)
	if err := endpoint.reserveGuestContext(); err != nil {
		t.Fatalf("reserve after a closed context became prunable: %v", err)
	}
	endpoint.releaseGuestContextReservation()
	if _, ok := endpoint.contexts["context-0"]; ok {
		t.Fatal("closed context was not pruned during admission")
	}
}

func TestMCPLifecycleReportsIncompleteCleanup(t *testing.T) {
	t.Run("context close", func(t *testing.T) {
		guest := &mcpGuestContext{id: "wedged", vmID: "one", done: make(chan struct{}), cancel: func() {}}
		endpoint := &mcpEndpoint{
			vms: map[string]mcpVM{"one": {ID: "one"}}, commands: make(map[string]*mcpCommand),
			contexts: map[string]*mcpGuestContext{guest.id: guest}, cleanupTimeout: 10 * time.Millisecond,
		}
		_, closed, err := endpoint.closeGuestContext(t.Context(), nil, mcpContextStatusInput{ContextID: guest.id})
		if err == nil || closed.Closed {
			t.Fatalf("wedged context close = %#v, %v", closed, err)
		}
	})

	t.Run("VM stop", func(t *testing.T) {
		shutdownCalled := make(chan struct{}, 1)
		control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			shutdownCalled <- struct{}{}
			writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
		}))
		defer control.Close()
		guest := &mcpGuestContext{id: "wedged", vmID: "one", done: make(chan struct{}), cancel: func() {}}
		endpoint := &mcpEndpoint{
			control: client.NewClient(control.URL, nil), vms: map[string]mcpVM{"one": {ID: "one"}},
			commands: make(map[string]*mcpCommand), contexts: map[string]*mcpGuestContext{guest.id: guest},
			stopping: make(map[string]struct{}), cleanupTimeout: 10 * time.Millisecond,
		}
		_, stopped, err := endpoint.stopVM(t.Context(), nil, mcpStopVMInput{VMID: "one"})
		if err == nil || stopped.Stopped {
			t.Fatalf("wedged VM stop = %#v, %v", stopped, err)
		}
		select {
		case <-shutdownCalled:
		default:
			t.Fatal("VM shutdown was skipped after cleanup timed out")
		}
	})
}

func inertMCPGuestContext(id, vmID string) *mcpGuestContext {
	done := make(chan struct{})
	return &mcpGuestContext{
		id: id, vmID: vmID, inputs: make(chan client.ExecInput), events: make(chan client.ExecEvent), done: done,
		cancel: func() { close(done) },
	}
}

func assertMCPCommandCanceled(t *testing.T, endpoint *mcpEndpoint, commandID string) {
	t.Helper()
	command, err := endpoint.command(commandID)
	if err != nil {
		t.Fatalf("find raced command: %v", err)
	}
	select {
	case <-command.done:
	case <-time.After(time.Second):
		t.Fatal("raced command did not settle")
	}
	result := command.snapshot(0, 0, mcpDefaultOutputChunk, false)
	if result.Status != "canceled" || result.ExitCode == nil || *result.ExitCode != 130 {
		t.Fatalf("raced command = %#v", result)
	}
}

func assertMCPCommandCanceledOrReaped(t *testing.T, endpoint *mcpEndpoint, commandID string) {
	t.Helper()
	command, err := endpoint.command(commandID)
	if err != nil {
		// A successful VM stop reaps all command metadata after canceling the
		// command. The concurrent start may observe success just before that
		// final reap, so absence is also a completed cleanup outcome.
		return
	}
	select {
	case <-command.done:
	case <-time.After(time.Second):
		t.Fatal("raced command did not settle")
	}
	result := command.snapshot(0, 0, mcpDefaultOutputChunk, false)
	if result.Status != "canceled" || result.ExitCode == nil || *result.ExitCode != 130 {
		t.Fatalf("raced command = %#v", result)
	}
}

func assertNoRunningMCPCommands(t *testing.T, endpoint *mcpEndpoint) {
	t.Helper()
	endpoint.mu.Lock()
	commands := make([]*mcpCommand, 0, len(endpoint.commands))
	for _, command := range endpoint.commands {
		commands = append(commands, command)
	}
	endpoint.mu.Unlock()
	for _, command := range commands {
		if status := command.snapshot(0, 0, mcpDefaultOutputChunk, false).Status; status == "running" {
			t.Fatalf("command %q remained running", command.id)
		}
	}
}

func structuredToolOutput[T any](t *testing.T, result *mcp.CallToolResult) T {
	t.Helper()
	output, err := decodeStructuredToolOutput[T](result)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func decodeStructuredToolOutput[T any](result *mcp.CallToolResult) (T, error) {
	var output T
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return output, fmt.Errorf("encode structured tool output: %w", err)
	}
	if err := json.Unmarshal(encoded, &output); err != nil {
		return output, fmt.Errorf("decode structured tool output: %w", err)
	}
	return output, nil
}

func TestMCPKVMContextFanout(t *testing.T) {
	endpoint, token := os.Getenv("VMSH_MCP_INTEGRATION_URL"), os.Getenv("VMSH_MCP_INTEGRATION_TOKEN")
	if endpoint == "" || token == "" {
		t.Skip("set VMSH_MCP_INTEGRATION_URL and VMSH_MCP_INTEGRATION_TOKEN for live KVM coverage")
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "vmsh-context-fanout-test", Version: "1"}, nil)
	session, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: mcpBearerTransport{token: token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	created, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_create", Arguments: map[string]any{
		"image": "alpine", "name": "context-fanout",
	}})
	if err != nil || created.IsError {
		t.Fatalf("create VM = %#v, %v", created, err)
	}
	vm := structuredToolOutput[mcpCreateVMOutput](t, created).VM
	defer func() {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_stop", Arguments: map[string]any{"vm_id": vm.ID}})
	}()

	const count = 32
	contexts := make([]string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	openedAt := time.Now()
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opened, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_context_open", Arguments: map[string]any{"vm_id": vm.ID}})
			if err != nil || opened.IsError {
				errs <- fmt.Errorf("open context %d: result=%#v error=%v", i, opened, err)
				return
			}
			info, err := decodeStructuredToolOutput[mcpContextInfo](opened)
			if err != nil {
				errs <- fmt.Errorf("decode context %d: %w", i, err)
				return
			}
			contexts[i] = info.ContextID
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	openElapsed := time.Since(openedAt)
	t.Logf("opened %d contexts in %s", count, openElapsed)
	if openElapsed > 15*time.Second {
		t.Fatalf("opening %d independent contexts took %s", count, openElapsed)
	}

	errs = make(chan error, count)
	closedAt := time.Now()
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			closed, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_context_close", Arguments: map[string]any{"context_id": contexts[i]}})
			if err != nil || closed.IsError {
				errs <- fmt.Errorf("close context %d: result=%#v error=%v", i, closed, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	closeElapsed := time.Since(closedAt)
	t.Logf("closed %d contexts in %s", count, closeElapsed)
	if closeElapsed > 15*time.Second {
		t.Fatalf("closing %d independent contexts took %s", count, closeElapsed)
	}

	escaped, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_exec_start", Arguments: map[string]any{
		"vm_id": vm.ID, "user": "root", "command": []string{"sh", "-c", `echo $$ >/sys/fs/cgroup/cgroup.procs; setsid sh -c 'nohup sh -c '"'"'trap "" TERM HUP INT; sleep 300'"'"' </dev/null >/tmp/vmsh-escaped.log 2>&1 & echo $! >/tmp/vmsh-escaped.pid' </dev/null >/dev/null 2>&1; echo ready; sleep 300`},
	}})
	if err != nil || escaped.IsError {
		t.Fatalf("start privileged escape = %#v, %v", escaped, err)
	}
	escapedCommand := structuredToolOutput[mcpCommandOutput](t, escaped)
	_, _ = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_exec_wait", Arguments: map[string]any{
		"command_id": escapedCommand.CommandID, "wait_seconds": 1,
	}})
	canceled, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_exec_cancel", Arguments: map[string]any{
		"command_id": escapedCommand.CommandID,
	}})
	if err != nil || canceled.IsError {
		t.Fatalf("cancel privileged escape = %#v, %v", canceled, err)
	}
	canceledOutput := structuredToolOutput[mcpCommandOutput](t, canceled)
	if canceledOutput.Status != "termination_unconfirmed" || canceledOutput.ExitCode != nil || canceledOutput.ContainmentError == "" {
		t.Fatalf("privileged escape cancellation = %#v", canceledOutput)
	}
	_, _ = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_run", Arguments: map[string]any{
		"vm_id": vm.ID, "user": "root", "command": []string{"sh", "-c", `test ! -f /tmp/vmsh-escaped.pid || kill -KILL "$(cat /tmp/vmsh-escaped.pid)" 2>/dev/null || true`},
	}})
}

func TestMCPKVMAutomaticHeadroomAndContextCaptureRecovery(t *testing.T) {
	endpoint, token := os.Getenv("VMSH_MCP_INTEGRATION_URL"), os.Getenv("VMSH_MCP_INTEGRATION_TOKEN")
	if endpoint == "" || token == "" {
		t.Skip("set VMSH_MCP_INTEGRATION_URL and VMSH_MCP_INTEGRATION_TOKEN for live KVM coverage")
	}
	hostMemory, err := (systemMemoryObserver{}).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "vmsh-memory-capture-test", Version: "1"}, nil)
	session, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: mcpBearerTransport{token: token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	created, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_create", Arguments: map[string]any{
		"image": "alpine", "name": "memory-capture-recovery",
	}})
	if err != nil || created.IsError {
		t.Fatalf("create VM = %#v, %v", created, err)
	}
	vm := structuredToolOutput[mcpCreateVMOutput](t, created).VM
	defer func() {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_stop", Arguments: map[string]any{"vm_id": vm.ID}})
	}()
	reserve := hostMemory.TotalMB * defaultReservePercent / 100
	safelyBackable := uint64(0)
	if hostMemory.AvailableMB > reserve {
		safelyBackable = hostMemory.AvailableMB - reserve
	}
	usable := vm.MemoryMB - min(vm.MemoryMB, vm.BalloonMB)
	if !vm.AutomaticMemory || usable > safelyBackable+512 {
		t.Fatalf("automatic admission memory=%d balloon=%d usable=%d host-safe=%d policy=%#v", vm.MemoryMB, vm.BalloonMB, usable, safelyBackable, vm)
	}

	opened, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_context_open", Arguments: map[string]any{"vm_id": vm.ID}})
	if err != nil || opened.IsError {
		t.Fatalf("open context = %#v, %v", opened, err)
	}
	contextID := structuredToolOutput[mcpContextInfo](t, opened).ContextID
	defer func() {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "vm_context_close", Arguments: map[string]any{"context_id": contextID}})
	}()
	produced, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_context_run", Arguments: map[string]any{
		"context_id": contextID, "command_line": "dd if=/dev/zero bs=1M count=32 2>/dev/null", "timeout_seconds": 30,
	}})
	if err != nil || produced.IsError {
		t.Fatalf("produce context output = %#v, %v", produced, err)
	}
	output := structuredToolOutput[mcpContextRunOutput](t, produced)
	if output.CommandStatus != "exited" || output.ExitCode != 0 || output.StdoutTotalBytes != 32<<20 || !output.StdoutTruncated {
		t.Fatalf("bulk context output = %#v", output)
	}
	checked, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_run", Arguments: map[string]any{
		"vm_id": vm.ID, "command": []string{"sh", "-c", `find /var/tmp -maxdepth 2 -type f \( -name '*.stdout' -o -name '*.stderr' \) -path '/var/tmp/.vmsh-*/*' -print`},
	}})
	if err != nil || checked.IsError {
		t.Fatalf("inspect captures = %#v, %v", checked, err)
	}
	checkOutput := structuredToolOutput[mcpCommandOutput](t, checked)
	if checkOutput.ExitCode == nil || *checkOutput.ExitCode != 0 || checkOutput.Stdout.TotalBytes != 0 {
		t.Fatalf("completed capture files were retained: %#v", checkOutput)
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
			writeMCPBootReady(t, w, r, client.InstanceState{ID: req.ID, Status: "running", Image: req.Image})
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

func writeMCPBootReady(t *testing.T, w http.ResponseWriter, r *http.Request, state client.InstanceState) {
	t.Helper()
	if r.Header.Get("Accept") != "application/x-ndjson" {
		t.Error("VM start did not request the boot event stream")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(client.BootEvent{Kind: "ready", State: state}); err != nil {
		t.Errorf("encode boot event: %v", err)
	}
}
