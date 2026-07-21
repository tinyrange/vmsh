package vmshd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tinyrange/vmsh/internal/version"
	"github.com/tinyrange/vmsh/internal/vmconfig"
	"github.com/tinyrange/vmsh/internal/vmshdprotocol"
	"j5.nz/cc/client"
)

const (
	mcpMinRAMMB           = 128
	mcpDefaultCPUs        = 1
	mcpBSDDefaultBootTime = 60 * time.Second
	mcpWorkCleanupTimeout = 3 * time.Second
	mcpMaxRequestBytes    = 8 << 20
	mcpRequestReadTimeout = 30 * time.Second
)

type MCPEndpointInfo struct {
	URL       string    `json:"url"`
	Version   string    `json:"version"`
	VMs       int       `json:"vms"`
	CreatedAt time.Time `json:"created_at"`
}

type MCPCredential struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

type mcpManager struct {
	mu         sync.Mutex
	controlURL string
	token      string
	endpoints  map[string]*mcpEndpoint
	balloon    *balloonController
}

type mcpEndpoint struct {
	sessionID string
	url       string
	createdAt time.Time
	listener  net.Listener
	server    *http.Server
	control   *client.Client
	balloon   *balloonController

	mu                  sync.Mutex
	credentials         map[string]string
	vms                 map[string]mcpVM
	commands            map[string]*mcpCommand
	artifacts           map[string]*mcpArtifact
	contexts            map[string]*mcpGuestContext
	stopping            map[string]struct{}
	quarantined         map[string]struct{}
	starting            map[string]*mcpVMStart
	openingContexts     map[string]*mcpContextOpening
	artifactOps         int
	artifactWork        map[*mcpArtifactReservation]struct{}
	artifactInFlight    int64
	closed              bool
	cleanupTimeout      time.Duration
	shutdownTimeout     time.Duration
	disableCaptureRelay bool
}

type mcpVMStart struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
}

type mcpContextOpening struct {
	id     string
	vmID   string
	cancel context.CancelFunc
	done   chan struct{}
}

type mcpVM struct {
	ID                            string `json:"id"`
	Name                          string `json:"name,omitempty"`
	Image                         string `json:"image"`
	Status                        string `json:"status,omitempty"`
	MemoryMB                      uint64 `json:"memory_mb,omitempty"`
	BalloonMB                     uint64 `json:"balloon_mb,omitempty"`
	BalloonObservedTargetMB       uint64 `json:"balloon_observed_target_mb,omitempty"`
	BalloonActualMB               uint64 `json:"balloon_actual_mb,omitempty"`
	BalloonStatus                 string `json:"balloon_status,omitempty"`
	AutomaticMemory               bool   `json:"automatic_memory,omitempty"`
	BalloonPolicyInFlight         bool   `json:"balloon_policy_in_flight,omitempty"`
	BalloonPolicyError            string `json:"balloon_policy_error,omitempty"`
	BalloonPolicyLastError        string `json:"balloon_policy_last_error,omitempty"`
	BackendStatus                 string `json:"backend_status,omitempty"`
	Quarantined                   bool   `json:"quarantined,omitempty"`
	BackingBytes                  uint64 `json:"backing_bytes,omitempty"`
	BackingHighWaterBytes         uint64 `json:"backing_high_water_bytes,omitempty"`
	BackingDataBytes              uint64 `json:"backing_data_bytes,omitempty"`
	BackingDataHighWaterBytes     uint64 `json:"backing_data_high_water_bytes,omitempty"`
	BackingMetadataBytes          uint64 `json:"backing_metadata_bytes,omitempty"`
	BackingMetadataHighWaterBytes uint64 `json:"backing_metadata_high_water_bytes,omitempty"`
	BackingPhysicalBytes          uint64 `json:"backing_physical_bytes,omitempty"`
	BackingReclaimError           string `json:"backing_reclaim_error,omitempty"`
	BackingUsageStale             bool   `json:"backing_usage_stale,omitempty"`
	BackingActiveMutations        uint64 `json:"backing_active_mutations,omitempty"`
	Error                         string `json:"error,omitempty"`
	ExitReason                    string `json:"exit_reason,omitempty"`
	ExitedAt                      string `json:"exited_at,omitempty"`
}

func (vm *mcpVM) observe(state client.InstanceState) {
	vm.Status = state.Status
	vm.BackendStatus = state.Status
	vm.MemoryMB = state.MemoryMB
	vm.BalloonMB = state.BalloonMB
	vm.BalloonActualMB = state.BalloonActualMB
	vm.BalloonStatus = state.BalloonStatus
	vm.BackingBytes = state.BackingBytes
	vm.BackingHighWaterBytes = state.BackingHighWaterBytes
	vm.BackingDataBytes = state.BackingDataBytes
	vm.BackingDataHighWaterBytes = state.BackingDataHighWaterBytes
	vm.BackingMetadataBytes = state.BackingMetadataBytes
	vm.BackingMetadataHighWaterBytes = state.BackingMetadataHighWaterBytes
	vm.BackingPhysicalBytes = state.BackingPhysicalBytes
	vm.BackingReclaimError = state.BackingReclaimError
	vm.BackingUsageStale = state.BackingUsageStale
	vm.BackingActiveMutations = state.BackingActiveMutations
	vm.Error = state.Error
	vm.ExitReason = state.ExitReason
	vm.ExitedAt = state.ExitedAt
}

func newMCPManager(token string) *mcpManager {
	return &mcpManager{token: token, endpoints: make(map[string]*mcpEndpoint)}
}

func (m *mcpManager) SetBalloonController(controller *balloonController) {
	m.mu.Lock()
	m.balloon = controller
	m.mu.Unlock()
}

func (m *mcpManager) SetControlURL(rawURL string) {
	m.mu.Lock()
	m.controlURL = strings.TrimRight(strings.TrimSpace(rawURL), "/")
	m.mu.Unlock()
}

func (m *mcpManager) Start(sessionID string) (MCPEndpointInfo, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return MCPEndpointInfo{}, fmt.Errorf("session id is required")
	}
	m.mu.Lock()
	if endpoint := m.endpoints[sessionID]; endpoint != nil {
		info := endpoint.info()
		m.mu.Unlock()
		return info, nil
	}
	controlURL := m.controlURL
	if controlURL == "" {
		m.mu.Unlock()
		return MCPEndpointInfo{}, fmt.Errorf("vmshd control listener is not ready")
	}
	control := client.NewClient(controlURL, nil)
	control.SetBearerToken(m.token)
	control.SetHeader(vmshdprotocol.HeaderProtocol, fmt.Sprintf("%d", vmshdprotocol.Current))
	control.SetHeader(vmshdprotocol.HeaderMinProtocol, fmt.Sprintf("%d", vmshdprotocol.Minimum))
	control.SetHeader(vmshdprotocol.HeaderName, "vmsh-mcp")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		m.mu.Unlock()
		return MCPEndpointInfo{}, fmt.Errorf("listen for MCP: %w", err)
	}
	endpoint := &mcpEndpoint{
		sessionID:       sessionID,
		url:             "http://" + listener.Addr().String() + "/mcp",
		createdAt:       time.Now().UTC(),
		listener:        listener,
		control:         control,
		balloon:         m.balloon,
		credentials:     make(map[string]string),
		vms:             make(map[string]mcpVM),
		commands:        make(map[string]*mcpCommand),
		artifacts:       make(map[string]*mcpArtifact),
		artifactWork:    make(map[*mcpArtifactReservation]struct{}),
		contexts:        make(map[string]*mcpGuestContext),
		stopping:        make(map[string]struct{}),
		quarantined:     make(map[string]struct{}),
		starting:        make(map[string]*mcpVMStart),
		openingContexts: make(map[string]*mcpContextOpening),
	}
	handler := http.MaxBytesHandler(endpoint.handler(), mcpMaxRequestBytes)
	endpoint.server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: mcpRequestReadTimeout, IdleTimeout: 2 * time.Minute}
	m.endpoints[sessionID] = endpoint
	m.mu.Unlock()
	go func() {
		m.handleEndpointServeExit(sessionID, endpoint, endpoint.server.Serve(listener))
	}()
	return endpoint.info(), nil
}

func (m *mcpManager) handleEndpointServeExit(sessionID string, endpoint *mcpEndpoint, serveErr error) {
	if serveErr == nil || errors.Is(serveErr, http.ErrServerClosed) {
		return
	}
	closeErr := endpoint.close()
	m.mu.Lock()
	if closeErr == nil && m.endpoints[sessionID] == endpoint {
		delete(m.endpoints, sessionID)
	}
	m.mu.Unlock()
}

func (m *mcpManager) Status(sessionID string) (MCPEndpointInfo, bool) {
	m.mu.Lock()
	endpoint := m.endpoints[strings.TrimSpace(sessionID)]
	m.mu.Unlock()
	if endpoint == nil {
		return MCPEndpointInfo{}, false
	}
	return endpoint.info(), true
}

func (m *mcpManager) MintCredential(sessionID string) (MCPCredential, error) {
	m.mu.Lock()
	endpoint := m.endpoints[strings.TrimSpace(sessionID)]
	m.mu.Unlock()
	if endpoint == nil {
		return MCPCredential{}, fmt.Errorf("MCP endpoint is not running")
	}
	id, err := randomMCPID("cred")
	if err != nil {
		return MCPCredential{}, err
	}
	token, err := randomMCPSecret()
	if err != nil {
		return MCPCredential{}, err
	}
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if endpoint.closed {
		return MCPCredential{}, fmt.Errorf("MCP endpoint is stopped")
	}
	endpoint.credentials[id] = token
	return MCPCredential{ID: id, Token: token}, nil
}

func (m *mcpManager) RevokeCredential(sessionID, credentialID string) bool {
	m.mu.Lock()
	endpoint := m.endpoints[strings.TrimSpace(sessionID)]
	m.mu.Unlock()
	if endpoint == nil {
		return false
	}
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if _, ok := endpoint.credentials[credentialID]; !ok {
		return false
	}
	delete(endpoint.credentials, credentialID)
	return true
}

func (m *mcpManager) Stop(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	endpoint := m.endpoints[sessionID]
	m.mu.Unlock()
	if endpoint == nil {
		return nil
	}
	if err := endpoint.close(); err != nil {
		// Keep the closed endpoint as a host-visible recovery handle. A later
		// stop retries only the VMs whose backend absence was not confirmed.
		return err
	}
	m.mu.Lock()
	if m.endpoints[sessionID] == endpoint {
		delete(m.endpoints, sessionID)
	}
	m.mu.Unlock()
	return nil
}

func (m *mcpManager) Close() error {
	m.mu.Lock()
	type namedEndpoint struct {
		id       string
		endpoint *mcpEndpoint
	}
	endpoints := make([]namedEndpoint, 0, len(m.endpoints))
	for id, endpoint := range m.endpoints {
		endpoints = append(endpoints, namedEndpoint{id: id, endpoint: endpoint})
	}
	m.mu.Unlock()
	var errs []error
	for _, named := range endpoints {
		if err := named.endpoint.close(); err != nil {
			errs = append(errs, fmt.Errorf("close MCP endpoint %q: %w", named.id, err))
			continue
		}
		m.mu.Lock()
		if m.endpoints[named.id] == named.endpoint {
			delete(m.endpoints, named.id)
		}
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (e *mcpEndpoint) info() MCPEndpointInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return MCPEndpointInfo{URL: e.url, Version: mcpImplementationVersion(), VMs: len(e.vms), CreatedAt: e.createdAt}
}

func (e *mcpEndpoint) handler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "vmsh", Version: mcpImplementationVersion()}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_create", Description: "Create a regular vmsh isolated VM with internet access but no host access, filesystem shares, or port forwards."}, e.createVM)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_list", Description: "List VMs created through this MCP session."}, e.listVMs)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_run", Description: "Run any command inside a VM created through this MCP session and wait for it to finish."}, e.runVM)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_exec_start", Description: "Start a command in an MCP-owned VM and return a reconnectable command ID immediately."}, e.startVMCommand)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_exec_status", Description: "Read status and binary-safe output for a command ID returned by vm_exec_start or vm_context_exec_start."}, e.statusVMCommand)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_exec_wait", Description: "Wait briefly for a command ID returned by vm_exec_start or vm_context_exec_start."}, e.waitVMCommand)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_exec_cancel", Description: "Terminate and reap a command; canceling a persistent-context command also closes that context."}, e.cancelVMCommand)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_exec_forget", Description: "Release retained output and metadata for a completed command ID."}, e.forgetVMCommand)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_artifact_export", Description: "Archive a guest file or directory into binary-safe storage scoped to this MCP session."}, e.exportArtifact)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_artifact_import", Description: "Extract a session artifact into an MCP-owned VM without exposing the host filesystem."}, e.importArtifact)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_artifact_list", Description: "List metadata for artifacts owned by this MCP session."}, e.listArtifacts)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_artifact_delete", Description: "Delete an artifact owned by this MCP session."}, e.deleteArtifact)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_copy", Description: "Copy a file or directory directly between two MCP-owned isolated VMs."}, e.copyGuestPath)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_context_open", Description: "Open a persistent shell context in an MCP-owned VM; cwd, exports, functions, and aliases survive across calls."}, e.openGuestContext)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_context_run", Description: "Run a short command line synchronously in a persistent guest context. Use vm_context_exec_start for long-running or cancelable work."}, e.runGuestContext)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_context_exec_start", Description: "Start a persistent-context command and return immediately; use vm_exec_status/wait, while vm_context_close or vm_stop can interrupt it."}, e.startGuestContextCommand)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_context_status", Description: "Report whether a persistent guest context is still running."}, e.statusGuestContext)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_context_close", Description: "Close and reap a persistent guest shell context."}, e.closeGuestContext)
	mcp.AddTool(server, &mcp.Tool{Name: "vm_stop", Description: "Stop a VM created through this MCP session."}, e.stopVM)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if !e.authorized(r.Header.Get("Authorization")) {
			writeJSON(w, http.StatusUnauthorized, client.ErrorResponse{Error: "unauthorized"})
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
}

func mcpImplementationVersion() string {
	build := version.Current()
	base := strings.TrimSpace(build.Version)
	if base == "" {
		base = "devel"
	}
	var metadata []string
	commit := strings.TrimSpace(build.Commit)
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if commit != "" {
		metadata = append(metadata, commit)
	}
	if build.Dirty && !strings.Contains(strings.ToLower(base), "dirty") {
		metadata = append(metadata, "dirty")
	}
	if len(metadata) == 0 {
		return base
	}
	return base + "+" + strings.Join(metadata, ".")
}

func (e *mcpEndpoint) authorized(header string) bool {
	prefix, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(prefix, "Bearer") || token == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, expected := range e.credentials {
		if len(token) == len(expected) && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

type mcpCreateVMInput struct {
	Image              string  `json:"image" jsonschema:"guest image to boot, for example alpine or @freebsd"`
	Name               string  `json:"name,omitempty" jsonschema:"optional human-readable name unique within this MCP session"`
	MemoryMB           uint64  `json:"memory_mb,omitempty" jsonschema:"guest memory in MiB, minimum 128 when specified; omitted selects host-observed automatic memory with dynamic pressure recovery"`
	CPUs               int     `json:"cpus,omitempty" jsonschema:"virtual CPU count, default 1; CPU oversubscription is allowed"`
	BootTimeoutSeconds float64 `json:"boot_timeout_seconds,omitempty" jsonschema:"bounded VM boot deadline; built-in BSD guests default to 60 seconds"`
}

type mcpCreateVMOutput struct {
	VM mcpVM `json:"vm"`
}

func (e *mcpEndpoint) createVM(ctx context.Context, _ *mcp.CallToolRequest, in mcpCreateVMInput) (*mcp.CallToolResult, mcpCreateVMOutput, error) {
	image := strings.TrimSpace(in.Image)
	name := strings.TrimSpace(in.Name)
	if image == "" {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("image is required")
	}
	if strings.HasPrefix(image, "vmsh-mcp-recovery-") {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("%q is a legacy recovery image, not an isolated guest image; inspect or delete it with the host vmsh image APIs", image)
	}
	image = canonicalMCPBuiltinImage(image)
	if in.CPUs == 0 {
		in.CPUs = mcpDefaultCPUs
	}
	if in.MemoryMB != 0 && in.MemoryMB < mcpMinRAMMB {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("memory_mb must be at least %d MiB", mcpMinRAMMB)
	}
	if in.CPUs < 1 {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("cpus must be positive")
	}
	if isMCPBuiltinBSDImage(image) && in.CPUs != 1 {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("cpus must be 1 for built-in BSD guests")
	}
	if err := validateMCPDurationSeconds("boot_timeout_seconds", in.BootTimeoutSeconds); err != nil {
		return nil, mcpCreateVMOutput{}, err
	}
	if in.BootTimeoutSeconds == 0 && isMCPBuiltinBSDImage(image) {
		in.BootTimeoutSeconds = mcpBSDDefaultBootTime.Seconds()
	}
	baseName := name
	var err error
	if baseName == "" {
		baseName, err = randomMCPID("mcp")
		if err != nil {
			return nil, mcpCreateVMOutput{}, err
		}
	} else if err := validateMCPVMName(baseName); err != nil {
		return nil, mcpCreateVMOutput{}, err
	}
	id := vmconfig.IsolatedVMID(baseName)
	startCtx, startCancel := context.WithCancel(ctx)
	start := &mcpVMStart{id: id, cancel: startCancel, done: make(chan struct{})}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		startCancel()
		return nil, mcpCreateVMOutput{}, fmt.Errorf("MCP endpoint is stopped")
	}
	for _, vm := range e.vms {
		if name != "" && vm.Name == name {
			e.mu.Unlock()
			startCancel()
			return nil, mcpCreateVMOutput{}, fmt.Errorf("VM name %q is already in use", name)
		}
	}
	if e.starting == nil {
		e.starting = make(map[string]*mcpVMStart)
	}
	if e.vms == nil {
		e.vms = make(map[string]mcpVM)
	}
	e.starting[id] = start
	e.vms[id] = mcpVM{ID: id, Name: baseName, Image: image, Status: "starting", BackendStatus: "starting"}
	e.mu.Unlock()
	backendStarted := false
	defer func() {
		startCancel()
		e.mu.Lock()
		if e.starting[id] == start {
			delete(e.starting, id)
		}
		if !backendStarted {
			delete(e.vms, id)
			delete(e.stopping, id)
			delete(e.quarantined, id)
		}
		close(start.done)
		e.mu.Unlock()
	}()
	if !isMCPBuiltinImage(image) {
		if _, err := e.control.GetImageContext(startCtx, image); err != nil {
			if err := validateMCPAutoPullImage(image); err != nil {
				return nil, mcpCreateVMOutput{}, err
			}
			if err := e.control.PullImageContext(startCtx, image, client.PullImageRequest{Source: mcpDockerHubSource(image)}); err != nil {
				return nil, mcpCreateVMOutput{}, fmt.Errorf("pull image: %w", err)
			}
		}
	}
	state, err := e.control.StartInstanceStreamWithIDContext(startCtx, id, client.StartInstanceRequest{
		Image: image, MemoryMB: in.MemoryMB, CPUs: in.CPUs,
		Network: vmconfig.IsolatedNetworkConfig(), TimeoutSeconds: in.BootTimeoutSeconds,
	}, nil)
	if err != nil {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("create VM: %w", err)
	}
	backendStarted = true
	vm := mcpVM{ID: id, Name: baseName, Image: image}
	vm.observe(state)
	e.applyBalloonPolicy(&vm)
	e.mu.Lock()
	e.vms[id] = vm
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("MCP endpoint stopped while VM %q was starting; ownership was retained for shutdown recovery", id)
	}
	vm, err = e.waitForInitialBalloon(startCtx, vm)
	if err != nil {
		return nil, mcpCreateVMOutput{}, err
	}
	return nil, mcpCreateVMOutput{VM: vm}, nil
}

func (e *mcpEndpoint) waitForInitialBalloon(ctx context.Context, vm mcpVM) (mcpVM, error) {
	if e.balloon == nil || !e.balloon.state(vm.ID).Automatic {
		return vm, nil
	}
	wait := normalizeBalloonPolicyConfig(e.balloon.config).Convergence
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := e.control.InstanceStatusOfContext(waitCtx, vm.ID)
		if err != nil {
			return vm, fmt.Errorf("wait for VM %q initial memory safety convergence: %w; the VM remains owned and can be inspected or stopped", vm.ID, err)
		}
		vm.observe(state)
		e.balloon.adjustmentReady(state, time.Now())
		e.applyBalloonPolicy(&vm)
		e.mu.Lock()
		e.vms[vm.ID] = vm
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return vm, fmt.Errorf("MCP endpoint stopped while VM %q was waiting for initial memory safety convergence; ownership was retained for shutdown recovery", vm.ID)
		}
		if vm.BalloonStatus == "converged" && vm.BalloonMB == vm.BalloonActualMB {
			return vm, nil
		}
		if state.Status == "stopped" || state.Status == "crashed" {
			return vm, fmt.Errorf("VM %q became %s before initial memory safety convergence: %s", vm.ID, state.Status, firstNonEmpty(state.ExitReason, state.Error, "no diagnostic available"))
		}
		select {
		case <-waitCtx.Done():
			return vm, fmt.Errorf("VM %q initial memory safety target did not converge within %s; target=%d MiB actual=%d MiB status=%s; the VM remains owned and can be inspected or stopped", vm.ID, wait, vm.BalloonMB, vm.BalloonActualMB, vm.BalloonStatus)
		case <-ticker.C:
		}
	}
}

func validateMCPVMName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("VM name is required")
	}
	if len(name) > 64 || name == "host" || strings.HasSuffix(name, vmconfig.IsolatedVMSuffix) {
		return fmt.Errorf("invalid isolated VM name %q", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) {
			continue
		}
		return fmt.Errorf("invalid isolated VM name %q", name)
	}
	return nil
}

func validateMCPDurationSeconds(name string, seconds float64) error {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return fmt.Errorf("%s must be finite and non-negative", name)
	}
	// Keep the conversion below time.Duration's positive boundary. Integer
	// division deliberately leaves the sub-second tail unused rather than
	// allowing float64 rounding to wrap a valid-looking deadline negative.
	if seconds > float64(math.MaxInt64/int64(time.Second)) {
		return fmt.Errorf("%s cannot be represented as a host duration", name)
	}
	return nil
}

func canonicalMCPBuiltinImage(image string) string {
	switch strings.ToLower(strings.TrimSpace(image)) {
	case "freebsd", "@freebsd":
		return "@freebsd"
	case "openbsd", "@openbsd":
		return "@openbsd"
	case "netbsd", "@netbsd":
		return "@netbsd"
	default:
		return strings.TrimSpace(image)
	}
}

func isMCPBuiltinImage(image string) bool {
	switch canonicalMCPBuiltinImage(image) {
	case "@freebsd", "@openbsd", "@netbsd":
		return true
	default:
		return false
	}
}

func validateMCPAutoPullImage(image string) error {
	image = strings.TrimSpace(image)
	if image == "" || strings.ContainsAny(image, "\\\t\r\n ") || strings.Contains(image, "://") ||
		strings.HasPrefix(image, "/") || strings.HasPrefix(image, "./") || strings.Contains(image, "../") || strings.Contains(image, "/..") {
		return fmt.Errorf("image %q is not cached and is not a safe Docker Hub reference", image)
	}
	for _, r := range image {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-/:@", r) {
			continue
		}
		return fmt.Errorf("image %q is not cached and is not a safe Docker Hub reference", image)
	}
	name := image
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	lastSlash := strings.LastIndexByte(name, '/')
	lastColon := strings.LastIndexByte(name, ':')
	if lastColon > lastSlash {
		name = name[:lastColon]
	}
	first, _, hasSlash := strings.Cut(name, "/")
	if hasSlash && (strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost") && first != "docker.io" {
		return fmt.Errorf("image %q is not cached; MCP auto-pull is limited to Docker Hub", image)
	}
	return nil
}

func mcpDockerHubSource(image string) string {
	image = strings.TrimSpace(image)
	if strings.HasPrefix(image, "docker.io/") {
		return image
	}
	name := image
	digest := ""
	if at := strings.IndexByte(name, '@'); at >= 0 {
		digest = name[at:]
		name = name[:at]
	}
	lastSlash := strings.LastIndexByte(name, '/')
	lastColon := strings.LastIndexByte(name, ':')
	hasTag := lastColon > lastSlash
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	source := "docker.io/" + name + digest
	if !hasTag && digest == "" {
		source += ":latest"
	}
	return source
}

type mcpListVMsInput struct{}
type mcpListVMsOutput struct {
	VMs                   []mcpVM `json:"vms"`
	ImplementationVersion string  `json:"implementation_version"`
	ObservationError      string  `json:"observation_error,omitempty"`
}

func (e *mcpEndpoint) listVMs(ctx context.Context, _ *mcp.CallToolRequest, _ mcpListVMsInput) (*mcp.CallToolResult, mcpListVMsOutput, error) {
	states, observationErr := e.control.InstanceStatusesContext(ctx)
	observed := make(map[string]client.InstanceState, len(states))
	for _, state := range states {
		observed[state.ID] = state
	}
	e.mu.Lock()
	vms := make([]mcpVM, 0, len(e.vms))
	for _, vm := range e.vms {
		_, quarantined := e.quarantined[vm.ID]
		if vm.Status == "" {
			vm.Status = "running"
		}
		if state, ok := observed[vm.ID]; ok {
			vm.observe(state)
		} else if observationErr == nil {
			vm.Status = "absent"
			vm.BackendStatus = "absent"
			vm.ExitReason = "backend no longer owns the VM"
		}
		vm.Quarantined = quarantined
		if quarantined {
			vm.Status = "quarantined"
		}
		e.applyBalloonPolicy(&vm)
		e.vms[vm.ID] = vm
		vms = append(vms, vm)
	}
	e.mu.Unlock()
	sort.Slice(vms, func(i, j int) bool { return vms[i].ID < vms[j].ID })
	out := mcpListVMsOutput{VMs: vms, ImplementationVersion: mcpImplementationVersion()}
	if observationErr != nil {
		out.ObservationError = conciseCommandError(observationErr)
	}
	return nil, out, nil
}

func (e *mcpEndpoint) applyBalloonPolicy(vm *mcpVM) {
	if e == nil || e.balloon == nil || vm == nil {
		return
	}
	policy := e.balloon.state(vm.ID)
	vm.BalloonObservedTargetMB = vm.BalloonMB
	vm.AutomaticMemory = policy.Automatic
	vm.BalloonPolicyInFlight = policy.InFlight
	vm.BalloonPolicyError = policy.DegradedReason
	vm.BalloonPolicyLastError = policy.LastFailure
	if policy.Automatic && policy.InFlight {
		vm.BalloonMB = policy.TargetMB
		if vm.BalloonActualMB < vm.BalloonMB {
			vm.BalloonStatus = "inflating"
		} else if vm.BalloonActualMB > vm.BalloonMB {
			vm.BalloonStatus = "deflating"
		} else {
			vm.BalloonStatus = "converged"
		}
	} else if policy.Automatic && vm.BackendStatus != "running" && vm.BackendStatus != "starting" && vm.BackendStatus != "stopping" {
		vm.BalloonMB = policy.TargetMB
		vm.BalloonActualMB = policy.ActualMB
		vm.BalloonStatus = policy.Status
	}
}

type mcpStopVMInput struct {
	VMID string `json:"vm_id" jsonschema:"ID returned by vm_create"`
}
type mcpStopVMOutput struct {
	Stopped       bool   `json:"stopped"`
	PreviousState string `json:"previous_state,omitempty"`
	ExitReason    string `json:"exit_reason,omitempty"`
}

func (e *mcpEndpoint) stopVM(ctx context.Context, _ *mcp.CallToolRequest, in mcpStopVMInput) (*mcp.CallToolResult, mcpStopVMOutput, error) {
	id, err := e.beginVMStop(in.VMID)
	if err != nil {
		return nil, mcpStopVMOutput{}, err
	}
	cleanupErr := e.cancelVMWork(ctx, id)
	if err := e.control.ShutdownInstanceWithIDContext(ctx, id); err != nil {
		state, terminal, observationErr := e.observeTerminalVM(ctx, id)
		if observationErr == nil && terminal {
			e.reapOwnedVM(id)
			if cleanupErr != nil {
				return nil, mcpStopVMOutput{}, fmt.Errorf("VM %q was already %s and was reaped, but MCP cleanup was incomplete: %w", id, terminalVMState(state), cleanupErr)
			}
			return nil, mcpStopVMOutput{Stopped: true, PreviousState: terminalVMState(state), ExitReason: state.ExitReason}, nil
		}
		e.mu.Lock()
		if e.quarantined == nil {
			e.quarantined = make(map[string]struct{})
		}
		e.quarantined[id] = struct{}{}
		e.mu.Unlock()
		return nil, mcpStopVMOutput{}, errors.Join(cleanupErr, fmt.Errorf("stop VM: %w", err), observationErr)
	}
	e.reapOwnedVM(id)
	if cleanupErr != nil {
		return nil, mcpStopVMOutput{}, fmt.Errorf("VM %q stopped but MCP cleanup was incomplete: %w", id, cleanupErr)
	}
	return nil, mcpStopVMOutput{Stopped: true}, nil
}

func (e *mcpEndpoint) observeTerminalVM(ctx context.Context, id string) (client.InstanceState, bool, error) {
	states, err := e.control.InstanceStatusesContext(ctx)
	if err != nil {
		return client.InstanceState{}, false, fmt.Errorf("observe VM after failed stop: %w", err)
	}
	for _, state := range states {
		if state.ID != id {
			continue
		}
		return state, state.Status == "stopped" || state.Status == "crashed", nil
	}
	return client.InstanceState{ID: id, Status: "absent", ExitReason: "backend no longer owns the VM"}, true, nil
}

func terminalVMState(state client.InstanceState) string {
	if state.Status == "" {
		return "absent"
	}
	return state.Status
}

func (e *mcpEndpoint) reapOwnedVM(id string) {
	e.mu.Lock()
	delete(e.vms, id)
	delete(e.stopping, id)
	delete(e.quarantined, id)
	for commandID, command := range e.commands {
		if command.vmID == id {
			command.releasePayload()
			delete(e.commands, commandID)
		}
	}
	for contextID, guest := range e.contexts {
		if guest.vmID == id {
			delete(e.contexts, contextID)
		}
	}
	e.mu.Unlock()
	if e.balloon != nil {
		e.balloon.forget(id)
	}
}

func (e *mcpEndpoint) ownedVMID(id string) (string, error) {
	vm, err := e.ownedVM(id)
	return vm.ID, err
}

func (e *mcpEndpoint) ownedVM(id string) (mcpVM, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	vm, ok := e.vms[id]
	if !ok {
		return mcpVM{}, fmt.Errorf("VM %q is not owned by this MCP session", id)
	}
	if _, ok := e.stopping[id]; ok {
		return mcpVM{}, fmt.Errorf("VM %q is stopping", id)
	}
	if vm.Status == "stopped" || vm.Status == "crashed" || vm.Status == "absent" {
		reason := vm.ExitReason
		if reason == "" {
			reason = vm.Error
		}
		return mcpVM{}, fmt.Errorf("VM %q is %s and must be reaped with vm_stop before it can be used: %s", id, vm.Status, reason)
	}
	return vm, nil
}

func isMCPBuiltinBSDImage(image string) bool {
	switch canonicalMCPBuiltinImage(image) {
	case "@freebsd", "@openbsd", "@netbsd":
		return true
	default:
		return false
	}
}

func mcpGuestUser(vm mcpVM, requested string) (string, error) {
	user := strings.TrimSpace(requested)
	if user == "" {
		if isMCPBuiltinBSDImage(vm.Image) {
			return "root", nil
		}
		return "1000:1000", nil
	}
	if isMCPBuiltinBSDImage(vm.Image) && !isMCPRootUser(user) {
		return "", fmt.Errorf("guest user %q is unsupported for %s; only root is currently available", user, vm.Image)
	}
	return user, nil
}

func isMCPRootUser(user string) bool {
	identity, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(user)), ":")
	// Effective UID 0 remains fully privileged regardless of its primary GID.
	// Group syntax is an execution preference, not a containment guarantee.
	return identity == "root" || identity == "0"
}

func mcpGuestWorkDir(vm mcpVM, requested string) string {
	if requested != "" {
		return requested
	}
	if isMCPBuiltinBSDImage(vm.Image) {
		return "/"
	}
	return "/home/cc"
}

func (e *mcpEndpoint) beginVMStop(id string) (string, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.vms[id]; !ok {
		return "", fmt.Errorf("VM %q is not owned by this MCP session", id)
	}
	if _, ok := e.stopping[id]; ok {
		if _, quarantined := e.quarantined[id]; quarantined {
			delete(e.quarantined, id)
			return id, nil
		}
		return "", fmt.Errorf("VM %q is already stopping", id)
	}
	if e.stopping == nil {
		e.stopping = make(map[string]struct{})
	}
	e.stopping[id] = struct{}{}
	return id, nil
}

func (e *mcpEndpoint) close() error {
	e.mu.Lock()
	firstClose := !e.closed
	e.closed = true
	commands := make([]*mcpCommand, 0, len(e.commands))
	contexts := make([]*mcpGuestContext, 0, len(e.contexts))
	openings := make([]*mcpContextOpening, 0, len(e.openingContexts))
	starts := make([]*mcpVMStart, 0, len(e.starting))
	artifactWork := make([]*mcpArtifactReservation, 0, len(e.artifactWork))
	for _, start := range e.starting {
		starts = append(starts, start)
	}
	for operation := range e.artifactWork {
		artifactWork = append(artifactWork, operation)
	}
	for _, command := range e.commands {
		commands = append(commands, command)
	}
	for _, guest := range e.contexts {
		contexts = append(contexts, guest)
	}
	for _, opening := range e.openingContexts {
		openings = append(openings, opening)
	}
	if firstClose {
		e.credentials = make(map[string]string)
		e.artifacts = make(map[string]*mcpArtifact)
	}
	e.mu.Unlock()
	var errs []error
	for _, start := range starts {
		start.cancel()
	}
	for _, operation := range artifactWork {
		operation.cancel()
	}
	for _, opening := range openings {
		opening.cancel()
	}
	cleanupErr := cancelAndWaitMCPWork(context.Background(), e.workCleanupTimeout(), commands, contexts)
	errs = append(errs, cleanupErr)
	errs = append(errs, waitMCPContextOpenings(context.Background(), e.workCleanupTimeout(), openings))
	e.mu.Lock()
	for _, command := range commands {
		select {
		case <-command.done:
			if e.commands[command.id] == command {
				command.releasePayload()
				delete(e.commands, command.id)
			}
		default:
		}
	}
	for _, guest := range contexts {
		select {
		case <-guest.completion():
			if e.contexts[guest.id] == guest {
				delete(e.contexts, guest.id)
			}
		default:
		}
	}
	e.mu.Unlock()
	if firstClose && e.server != nil {
		// Stop accepting and terminate active MCP transports before cleaning up
		// their VMs. Graceful HTTP shutdown can otherwise wait indefinitely for a
		// long-lived MCP session that is itself waiting on this cleanup.
		errs = append(errs, e.server.Close())
	}
	if len(starts) != 0 {
		deadline := time.NewTimer(e.workCleanupTimeout())
		for _, start := range starts {
			select {
			case <-start.done:
			case <-deadline.C:
				errs = append(errs, fmt.Errorf("VM %q start did not stop before endpoint cleanup deadline; ownership was retained for retry", start.id))
				goto startsDone
			}
		}
		if !deadline.Stop() {
			select {
			case <-deadline.C:
			default:
			}
		}
	}
startsDone:
	artifactCleanupTimedOut := false
	if len(artifactWork) != 0 {
		deadline := time.NewTimer(e.workCleanupTimeout())
		for _, operation := range artifactWork {
			select {
			case <-operation.done:
			case <-deadline.C:
				errs = append(errs, fmt.Errorf("%d artifact operation(s) did not stop before endpoint cleanup deadline; VM ownership was retained for retry", len(artifactWork)))
				artifactCleanupTimedOut = true
				goto artifactsDone
			}
		}
		if !deadline.Stop() {
			select {
			case <-deadline.C:
			default:
			}
		}
	}
artifactsDone:
	if artifactCleanupTimedOut {
		return errors.Join(errs...)
	}
	e.mu.Lock()
	ids := make([]string, 0, len(e.vms))
	if e.stopping == nil {
		e.stopping = make(map[string]struct{})
	}
	for id := range e.vms {
		ids = append(ids, id)
		e.stopping[id] = struct{}{}
	}
	e.mu.Unlock()
	errs = append(errs, e.shutdownOwnedVMs(ids))
	return errors.Join(errs...)
}

func (e *mcpEndpoint) shutdownOwnedVMs(ids []string) error {
	type result struct {
		id  string
		err error
	}
	results := make(chan result, len(ids))
	for _, id := range ids {
		go func(id string) {
			ctx, cancel := context.WithTimeout(context.Background(), e.vmShutdownTimeout())
			err := e.control.ShutdownInstanceWithIDContext(ctx, id)
			cancel()
			if err != nil {
				observeCtx, observeCancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, terminal, observeErr := e.observeTerminalVM(observeCtx, id)
				observeCancel()
				if terminal && observeErr == nil {
					err = nil
				} else {
					err = errors.Join(fmt.Errorf("shutdown VM %q: %w", id, err), observeErr)
				}
			}
			results <- result{id: id, err: err}
		}(id)
	}
	var errs []error
	for range ids {
		result := <-results
		if result.err == nil {
			e.reapOwnedVM(result.id)
			continue
		}
		e.mu.Lock()
		if e.quarantined == nil {
			e.quarantined = make(map[string]struct{})
		}
		e.quarantined[result.id] = struct{}{}
		e.mu.Unlock()
		errs = append(errs, result.err)
	}
	return errors.Join(errs...)
}

func (e *mcpEndpoint) vmShutdownTimeout() time.Duration {
	if e.shutdownTimeout > 0 {
		return e.shutdownTimeout
	}
	return 10 * time.Second
}

func (e *mcpEndpoint) cancelVMWork(ctx context.Context, vmID string) error {
	e.mu.Lock()
	commands := make([]*mcpCommand, 0)
	for _, command := range e.commands {
		if command.vmID == vmID {
			commands = append(commands, command)
		}
	}
	contexts := make([]*mcpGuestContext, 0)
	for _, guest := range e.contexts {
		if guest.vmID == vmID {
			contexts = append(contexts, guest)
		}
	}
	openings := make([]*mcpContextOpening, 0)
	for _, opening := range e.openingContexts {
		if opening.vmID == vmID {
			openings = append(openings, opening)
		}
	}
	e.mu.Unlock()
	for _, opening := range openings {
		opening.cancel()
	}
	err := cancelAndWaitMCPWork(ctx, e.workCleanupTimeout(), commands, contexts)
	err = errors.Join(err, waitMCPContextOpenings(ctx, e.workCleanupTimeout(), openings))
	e.mu.Lock()
	for _, guest := range contexts {
		select {
		case <-guest.completion():
			if e.contexts[guest.id] == guest {
				delete(e.contexts, guest.id)
			}
		default:
		}
	}
	e.mu.Unlock()
	return err
}

func waitMCPContextOpenings(ctx context.Context, timeout time.Duration, openings []*mcpContextOpening) error {
	if len(openings) == 0 {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, opening := range openings {
		select {
		case <-opening.done:
		case <-waitCtx.Done():
			return fmt.Errorf("context %q opening for VM %q did not stop before cleanup deadline: %w", opening.id, opening.vmID, waitCtx.Err())
		}
	}
	return nil
}

func (e *mcpEndpoint) workCleanupTimeout() time.Duration {
	if e.cleanupTimeout > 0 {
		return e.cleanupTimeout
	}
	return mcpWorkCleanupTimeout
}

func cancelAndWaitMCPWork(ctx context.Context, timeout time.Duration, commands []*mcpCommand, contexts []*mcpGuestContext) error {
	for _, command := range commands {
		command.requestCancel()
	}
	for _, guest := range contexts {
		guest.stop()
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, guest := range contexts {
		select {
		case <-guest.done:
		case <-waitCtx.Done():
			return incompleteMCPWorkError(waitCtx.Err(), commands, contexts)
		}
	}
	for _, command := range commands {
		select {
		case <-command.done:
		case <-waitCtx.Done():
			return incompleteMCPWorkError(waitCtx.Err(), commands, contexts)
		}
	}
	return nil
}

func incompleteMCPWorkError(cause error, commands []*mcpCommand, contexts []*mcpGuestContext) error {
	var pending []string
	for _, guest := range contexts {
		select {
		case <-guest.done:
		default:
			pending = append(pending, "context "+guest.id)
		}
	}
	for _, command := range commands {
		select {
		case <-command.done:
		default:
			pending = append(pending, "command "+command.id)
		}
	}
	if len(pending) == 0 {
		pending = append(pending, "work completion")
	}
	return fmt.Errorf("MCP cleanup did not finish for %s: %w", strings.Join(pending, ", "), cause)
}

func randomMCPID(prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}

func randomMCPSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
