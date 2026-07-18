package vmshd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
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
	mcpMaxVMs             = 16
	mcpDefaultRAMMB       = 512
	mcpMaxRAMMB           = 4096
	mcpDefaultCPUs        = 1
	mcpMaxCPUs            = 8
	mcpWorkCleanupTimeout = 3 * time.Second
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
}

type mcpEndpoint struct {
	sessionID string
	url       string
	createdAt time.Time
	listener  net.Listener
	server    *http.Server
	control   *client.Client

	mu             sync.Mutex
	credentials    map[string]string
	vms            map[string]mcpVM
	commands       map[string]*mcpCommand
	artifacts      map[string]*mcpArtifact
	contexts       map[string]*mcpGuestContext
	stopping       map[string]struct{}
	starting       int
	closed         bool
	cleanupTimeout time.Duration
}

type mcpVM struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Image string `json:"image"`
}

func newMCPManager(token string) *mcpManager {
	return &mcpManager{token: token, endpoints: make(map[string]*mcpEndpoint)}
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
		sessionID:   sessionID,
		url:         "http://" + listener.Addr().String() + "/mcp",
		createdAt:   time.Now().UTC(),
		listener:    listener,
		control:     control,
		credentials: make(map[string]string),
		vms:         make(map[string]mcpVM),
		commands:    make(map[string]*mcpCommand),
		artifacts:   make(map[string]*mcpArtifact),
		contexts:    make(map[string]*mcpGuestContext),
		stopping:    make(map[string]struct{}),
	}
	handler := endpoint.handler()
	endpoint.server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}
	m.endpoints[sessionID] = endpoint
	m.mu.Unlock()
	go func() {
		if err := endpoint.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = endpoint.close()
		}
	}()
	return endpoint.info(), nil
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
	m.mu.Lock()
	endpoint := m.endpoints[strings.TrimSpace(sessionID)]
	delete(m.endpoints, strings.TrimSpace(sessionID))
	m.mu.Unlock()
	if endpoint == nil {
		return nil
	}
	return endpoint.close()
}

func (m *mcpManager) Close() error {
	m.mu.Lock()
	endpoints := make([]*mcpEndpoint, 0, len(m.endpoints))
	for _, endpoint := range m.endpoints {
		endpoints = append(endpoints, endpoint)
	}
	m.endpoints = make(map[string]*mcpEndpoint)
	m.mu.Unlock()
	var errs []error
	for _, endpoint := range endpoints {
		errs = append(errs, endpoint.close())
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
	Image    string `json:"image" jsonschema:"guest image to boot, for example alpine or @freebsd"`
	Name     string `json:"name,omitempty" jsonschema:"optional human-readable name unique within this MCP session"`
	MemoryMB uint64 `json:"memory_mb,omitempty" jsonschema:"guest memory in MiB, default 512 and maximum 4096"`
	CPUs     int    `json:"cpus,omitempty" jsonschema:"virtual CPU count, default 1 and maximum 8"`
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
	image = canonicalMCPBuiltinImage(image)
	if in.MemoryMB == 0 {
		in.MemoryMB = mcpDefaultRAMMB
	}
	if in.CPUs == 0 {
		in.CPUs = mcpDefaultCPUs
	}
	if in.MemoryMB > mcpMaxRAMMB || in.CPUs < 1 || in.CPUs > mcpMaxCPUs {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("requested resources exceed the MCP ceiling of %d MiB and %d CPUs", mcpMaxRAMMB, mcpMaxCPUs)
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, mcpCreateVMOutput{}, fmt.Errorf("MCP endpoint is stopped")
	}
	if len(e.vms)+e.starting >= mcpMaxVMs {
		e.mu.Unlock()
		return nil, mcpCreateVMOutput{}, fmt.Errorf("MCP session VM limit of %d reached", mcpMaxVMs)
	}
	for _, vm := range e.vms {
		if name != "" && vm.Name == name {
			e.mu.Unlock()
			return nil, mcpCreateVMOutput{}, fmt.Errorf("VM name %q is already in use", name)
		}
	}
	e.starting++
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.starting--
		e.mu.Unlock()
	}()
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
	if !isMCPBuiltinImage(image) {
		if _, err := e.control.GetImageContext(ctx, image); err != nil {
			if err := validateMCPAutoPullImage(image); err != nil {
				return nil, mcpCreateVMOutput{}, err
			}
			if err := e.control.PullImageContext(ctx, image, client.PullImageRequest{Source: mcpDockerHubSource(image)}); err != nil {
				return nil, mcpCreateVMOutput{}, fmt.Errorf("pull image: %w", err)
			}
		}
	}
	_, err = e.control.StartInstanceWithIDContext(ctx, id, client.StartInstanceRequest{
		Image: image, MemoryMB: in.MemoryMB, CPUs: in.CPUs,
		Network: vmconfig.IsolatedNetworkConfig(),
	})
	if err != nil {
		return nil, mcpCreateVMOutput{}, fmt.Errorf("create VM: %w", err)
	}
	vm := mcpVM{ID: id, Name: baseName, Image: image}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.control.ShutdownInstanceWithIDContext(shutdownCtx, id)
		return nil, mcpCreateVMOutput{}, fmt.Errorf("MCP endpoint stopped while the VM was starting")
	}
	e.vms[id] = vm
	e.mu.Unlock()
	return nil, mcpCreateVMOutput{VM: vm}, nil
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
	VMs []mcpVM `json:"vms"`
}

func (e *mcpEndpoint) listVMs(context.Context, *mcp.CallToolRequest, mcpListVMsInput) (*mcp.CallToolResult, mcpListVMsOutput, error) {
	e.mu.Lock()
	vms := make([]mcpVM, 0, len(e.vms))
	for _, vm := range e.vms {
		vms = append(vms, vm)
	}
	e.mu.Unlock()
	sort.Slice(vms, func(i, j int) bool { return vms[i].ID < vms[j].ID })
	return nil, mcpListVMsOutput{VMs: vms}, nil
}

type mcpStopVMInput struct {
	VMID string `json:"vm_id" jsonschema:"ID returned by vm_create"`
}
type mcpStopVMOutput struct {
	Stopped bool `json:"stopped"`
}

func (e *mcpEndpoint) stopVM(ctx context.Context, _ *mcp.CallToolRequest, in mcpStopVMInput) (*mcp.CallToolResult, mcpStopVMOutput, error) {
	id, err := e.beginVMStop(in.VMID)
	if err != nil {
		return nil, mcpStopVMOutput{}, err
	}
	cleanupErr := e.cancelVMWork(ctx, id)
	if err := e.control.ShutdownInstanceWithIDContext(ctx, id); err != nil {
		e.mu.Lock()
		delete(e.stopping, id)
		e.mu.Unlock()
		return nil, mcpStopVMOutput{}, errors.Join(cleanupErr, fmt.Errorf("stop VM: %w", err))
	}
	e.mu.Lock()
	delete(e.vms, id)
	delete(e.stopping, id)
	e.mu.Unlock()
	if cleanupErr != nil {
		return nil, mcpStopVMOutput{}, fmt.Errorf("VM %q stopped but MCP cleanup was incomplete: %w", id, cleanupErr)
	}
	return nil, mcpStopVMOutput{Stopped: true}, nil
}

func (e *mcpEndpoint) ownedVMID(id string) (string, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.vms[id]; !ok {
		return "", fmt.Errorf("VM %q is not owned by this MCP session", id)
	}
	if _, ok := e.stopping[id]; ok {
		return "", fmt.Errorf("VM %q is stopping", id)
	}
	return id, nil
}

func (e *mcpEndpoint) beginVMStop(id string) (string, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.vms[id]; !ok {
		return "", fmt.Errorf("VM %q is not owned by this MCP session", id)
	}
	if _, ok := e.stopping[id]; ok {
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
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.credentials = make(map[string]string)
	commands := make([]*mcpCommand, 0, len(e.commands))
	for _, command := range e.commands {
		commands = append(commands, command)
	}
	e.commands = make(map[string]*mcpCommand)
	contexts := make([]*mcpGuestContext, 0, len(e.contexts))
	for _, guest := range e.contexts {
		contexts = append(contexts, guest)
	}
	e.artifacts = make(map[string]*mcpArtifact)
	e.contexts = make(map[string]*mcpGuestContext)
	ids := make([]string, 0, len(e.vms))
	for id := range e.vms {
		ids = append(ids, id)
	}
	e.vms = make(map[string]mcpVM)
	e.mu.Unlock()
	var errs []error
	errs = append(errs, cancelAndWaitMCPWork(context.Background(), e.workCleanupTimeout(), commands, contexts))
	if e.server != nil {
		// Stop accepting and terminate active MCP transports before cleaning up
		// their VMs. Graceful HTTP shutdown can otherwise wait indefinitely for a
		// long-lived MCP session that is itself waiting on this cleanup.
		errs = append(errs, e.server.Close())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range ids {
		errs = append(errs, e.control.ShutdownInstanceWithIDContext(ctx, id))
	}
	return errors.Join(errs...)
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
	for id, guest := range e.contexts {
		if guest.vmID == vmID {
			contexts = append(contexts, guest)
			delete(e.contexts, id)
		}
	}
	e.mu.Unlock()
	return cancelAndWaitMCPWork(ctx, e.workCleanupTimeout(), commands, contexts)
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
