package vmshd

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/vmsh/internal/trusted"
	"j5.nz/cc/ccvmd"
)

type TrustGrantRequest struct {
	VMID    string `json:"vm_id"`
	Profile string `json:"profile"`
}

type TrustGrantInfo struct {
	VMID             string            `json:"vm_id"`
	SourceGeneration uint64            `json:"source_generation"`
	Profile          string            `json:"profile"`
	ProfileDigest    string            `json:"profile_digest"`
	TargetID         string            `json:"target_id"`
	DefaultRootID    string            `json:"default_root_id"`
	ActionDeadlines  map[string]string `json:"action_deadlines,omitempty"`
	ServiceAddress   string            `json:"service_address"`
	ServicePort      int               `json:"service_port"`
	Token            string            `json:"token,omitempty"`
	GrantedAt        time.Time         `json:"granted_at"`
	Revoked          bool              `json:"revoked"`
}

type trustedGateway struct {
	grant   trusted.Grant
	info    TrustGrantInfo
	gateway *trusted.Gateway
}

type trustedManager struct {
	mu       sync.Mutex
	root     string
	gateways map[string]*trustedGateway
}

func newTrustedManager() (*trustedManager, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	root = filepath.Join(root, "vmsh", "trusted")
	if err := os.MkdirAll(filepath.Join(root, "grants"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "audit"), 0o700); err != nil {
		return nil, err
	}
	return &trustedManager{root: root, gateways: make(map[string]*trustedGateway)}, nil
}

func (m *trustedManager) grant(ctx context.Context, runtime ccvmd.RuntimeView, request TrustGrantRequest) (TrustGrantInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := safeTrustName(request.Profile); err != nil {
		return TrustGrantInfo{}, err
	}
	request.VMID = strings.TrimSpace(request.VMID)
	if request.VMID == "" {
		return TrustGrantInfo{}, fmt.Errorf("vm_id is required")
	}
	var startedAt string
	for _, state := range runtime.InstanceStatuses() {
		if state.ID == request.VMID && state.Status == "running" {
			startedAt = state.StartedAt
			break
		}
	}
	if startedAt == "" {
		return TrustGrantInfo{}, fmt.Errorf("running VM %q with a stable start identity was not found", request.VMID)
	}
	profile, err := trusted.LoadProfile(filepath.Join(m.root, "profiles", request.Profile+".json"))
	if err != nil {
		return TrustGrantInfo{}, err
	}
	handshakeTimeout, _ := time.ParseDuration(profile.HandshakeTimeout)
	generation := sourceGeneration(request.VMID, startedAt)
	token, err := trusted.NewToken()
	if err != nil {
		return TrustGrantInfo{}, err
	}
	fileID := trustFileID(request.VMID)
	grant := trusted.Grant{ID: fmt.Sprintf("%s-%x", fileID, generation), SourceVMID: request.VMID, SourceGeneration: generation, TargetID: profile.TargetID, ProfileID: profile.ID, ProfileDigest: profile.Digest, RevocationGeneration: 1, CreatedAt: time.Now().UTC()}
	auditPath := filepath.Join(m.root, "audit", grant.ID+".jsonl")
	gateway, err := trusted.ListenGateway(trusted.GatewayConfig{Profile: profile, Grant: grant, Token: token, HandshakeTimeout: handshakeTimeout, AuditPath: auditPath})
	if err != nil {
		return TrustGrantInfo{}, err
	}
	if err := runtime.AllowServiceProxyPort(ctx, request.VMID, gateway.Port()); err != nil {
		gateway.Close()
		return TrustGrantInfo{}, err
	}
	if err := trusted.SaveGrant(filepath.Join(m.root, "grants", fileID+".json"), grant); err != nil {
		gateway.Revoke()
		return TrustGrantInfo{}, err
	}
	actionDeadlines := make(map[string]string, len(profile.Actions))
	for actionID, action := range profile.Actions {
		actionDeadlines[actionID] = action.MaxDuration
	}
	info := TrustGrantInfo{VMID: request.VMID, SourceGeneration: generation, Profile: profile.ID, ProfileDigest: profile.Digest, TargetID: profile.TargetID, DefaultRootID: profile.DefaultRootID, ActionDeadlines: actionDeadlines, ServiceAddress: "10.42.0.1", ServicePort: gateway.Port(), Token: token, GrantedAt: grant.CreatedAt}
	if previous := m.gateways[request.VMID]; previous != nil {
		previous.gateway.Revoke()
	}
	m.gateways[request.VMID] = &trustedGateway{grant: grant, info: info, gateway: gateway}
	return info, nil
}

func (m *trustedManager) revoke(vmID string) (TrustGrantInfo, error) {
	vmID = strings.TrimSpace(vmID)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.gateways[vmID]
	if entry == nil {
		return TrustGrantInfo{}, fmt.Errorf("VM %q has no active trust grant", vmID)
	}
	entry.gateway.Revoke()
	entry.grant.Revoked = true
	entry.grant.RevocationGeneration++
	if err := trusted.SaveGrant(filepath.Join(m.root, "grants", trustFileID(vmID)+".json"), entry.grant); err != nil {
		return TrustGrantInfo{}, err
	}
	entry.info.Revoked = true
	entry.info.Token = ""
	return entry.info, nil
}

func (m *trustedManager) list() []TrustGrantInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]TrustGrantInfo, 0, len(m.gateways))
	for _, entry := range m.gateways {
		info := entry.info
		info.Token = ""
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].VMID < result[j].VMID })
	return result
}

func (m *trustedManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.gateways {
		entry.gateway.Revoke()
		_ = entry.gateway.Close()
	}
}

func (m *trustedManager) register(mux interface {
	HandleFunc(string, http.HandlerFunc)
}, runtime ccvmd.RuntimeView) {
	mux.HandleFunc("GET /vmsh/trust", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, m.list())
	})
	mux.HandleFunc("POST /vmsh/trust", func(w http.ResponseWriter, r *http.Request) {
		var request TrustGrantRequest
		if err := decodeRequiredJSON(r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		info, err := m.grant(r.Context(), runtime, request)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("DELETE /vmsh/trust/{vm}", func(w http.ResponseWriter, r *http.Request) {
		info, err := m.revoke(r.PathValue("vm"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, info)
	})
}

func sourceGeneration(vmID, startedAt string) uint64 {
	digest := sha256.Sum256([]byte(vmID + "\x00" + startedAt))
	return binary.BigEndian.Uint64(digest[:8])
}

func trustFileID(vmID string) string {
	digest := sha256.Sum256([]byte(vmID))
	return fmt.Sprintf("%x", digest[:16])
}

func safeTrustName(name string) error {
	if name == "" {
		return fmt.Errorf("profile is required")
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return fmt.Errorf("profile name must contain only URL-safe characters")
	}
	return nil
}
