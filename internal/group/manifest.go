package group

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

const ManifestVersion = 1

const maxManifestNodes = 256

type Capability string

const (
	CapabilityGroupRead        Capability = "group.read"
	CapabilityVMRead           Capability = "vm.read"
	CapabilityVMCreate         Capability = "vm.create"
	CapabilityVMControl        Capability = "vm.control"
	CapabilityVMExec           Capability = "vm.exec"
	CapabilityImageRead        Capability = "image.read"
	CapabilityImageStage       Capability = "image.stage"
	CapabilityCopy             Capability = "copy"
	CapabilityForward          Capability = "forward"
	CapabilityMigrationSend    Capability = "migration.send"
	CapabilityMigrationReceive Capability = "migration.receive"
	CapabilityHostExec         Capability = "host.exec"
)

var knownCapabilities = map[Capability]struct{}{
	CapabilityGroupRead: {}, CapabilityVMRead: {}, CapabilityVMCreate: {},
	CapabilityVMControl: {}, CapabilityVMExec: {}, CapabilityImageRead: {},
	CapabilityImageStage: {}, CapabilityCopy: {}, CapabilityForward: {},
	CapabilityMigrationSend: {}, CapabilityMigrationReceive: {}, CapabilityHostExec: {},
}

type Manifest struct {
	Version                int      `json:"version"`
	GroupID                string   `json:"group_id"`
	Name                   string   `json:"name"`
	Generation             uint64   `json:"generation"`
	MaxCertificateLifetime string   `json:"max_certificate_lifetime"`
	OwnerPublicKey         string   `json:"owner_public_key"`
	Issuers                []Issuer `json:"issuers"`
	Nodes                  []Node   `json:"nodes"`
	RevokedIssuerIDs       []string `json:"revoked_issuer_ids,omitempty"`
	RevokedNodeIDs         []string `json:"revoked_node_ids,omitempty"`
	Signature              string   `json:"signature"`
}

type Issuer struct {
	ID             string       `json:"id"`
	Kind           string       `json:"kind"`
	CertificatePEM string       `json:"certificate_pem"`
	AllowedScopes  []Capability `json:"allowed_scopes"`
}

type Node struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Address  string       `json:"address"`
	IssuerID string       `json:"issuer_id"`
	Scopes   []Capability `json:"scopes"`
}

type SecurityReason string

const (
	SecurityInvalidManifest    SecurityReason = "invalid_manifest"
	SecurityManifestConflict   SecurityReason = "manifest_conflict"
	SecurityStaleManifest      SecurityReason = "stale_manifest"
	SecurityInvalidCredential  SecurityReason = "invalid_credential"
	SecurityExpiredCredential  SecurityReason = "expired_credential"
	SecurityRevokedIdentity    SecurityReason = "revoked_identity"
	SecurityWrongGroup         SecurityReason = "wrong_group"
	SecurityWrongGeneration    SecurityReason = "wrong_generation"
	SecurityCapabilityDenied   SecurityReason = "capability_denied"
	SecurityIdentityMismatch   SecurityReason = "identity_mismatch"
	SecurityConfigurationError SecurityReason = "configuration_error"
)

type SecurityError struct {
	Reason SecurityReason
	Detail string
}

func (e *SecurityError) Error() string {
	if e == nil {
		return "group security error"
	}
	if strings.TrimSpace(e.Detail) == "" {
		return "group security: " + string(e.Reason)
	}
	return "group security: " + string(e.Reason) + ": " + e.Detail
}

func SignManifest(manifest *Manifest, privateKey ed25519.PrivateKey) error {
	if manifest == nil {
		return manifestError("manifest is required")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return manifestError("owner Ed25519 private key is invalid")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	manifest.OwnerPublicKey = base64.RawURLEncoding.EncodeToString(publicKey)
	manifest.Signature = ""
	payload, err := manifestSigningPayload(*manifest)
	if err != nil {
		return err
	}
	manifest.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return VerifyManifest(*manifest)
}

func VerifyManifest(manifest Manifest) error {
	if err := validateManifestShape(manifest); err != nil {
		return err
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(manifest.OwnerPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return manifestError("owner_public_key is not an Ed25519 public key")
	}
	signature, err := base64.RawURLEncoding.DecodeString(manifest.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return manifestError("signature is not an Ed25519 signature")
	}
	payload, err := manifestSigningPayload(manifest)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return manifestError("signature verification failed")
	}
	return nil
}

func ManifestDigest(manifest Manifest) (string, error) {
	if err := VerifyManifest(manifest); err != nil {
		return "", err
	}
	payload, err := manifestSigningPayload(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func ReconcileManifest(current, candidate Manifest) (Manifest, error) {
	if err := VerifyManifest(current); err != nil {
		return Manifest{}, err
	}
	if err := VerifyManifest(candidate); err != nil {
		return Manifest{}, err
	}
	if current.GroupID != candidate.GroupID || current.OwnerPublicKey != candidate.OwnerPublicKey {
		return Manifest{}, &SecurityError{Reason: SecurityWrongGroup, Detail: "manifest owner or group does not match"}
	}
	if candidate.Generation < current.Generation {
		return Manifest{}, &SecurityError{Reason: SecurityStaleManifest, Detail: fmt.Sprintf("generation %d is older than %d", candidate.Generation, current.Generation)}
	}
	if candidate.Generation > current.Generation {
		return candidate, nil
	}
	currentDigest, _ := ManifestDigest(current)
	candidateDigest, _ := ManifestDigest(candidate)
	if currentDigest != candidateDigest {
		return Manifest{}, &SecurityError{Reason: SecurityManifestConflict, Detail: fmt.Sprintf("generation %d has different signed digests", current.Generation)}
	}
	return current, nil
}

func (m Manifest) MaxLifetime() (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(m.MaxCertificateLifetime))
	if err != nil || duration <= 0 {
		return 0, manifestError("max_certificate_lifetime must be a positive duration")
	}
	return duration, nil
}

func (m Manifest) NodeByID(id string) (Node, bool) {
	for _, node := range m.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

func (m Manifest) IssuerByID(id string) (Issuer, bool) {
	for _, issuer := range m.Issuers {
		if issuer.ID == id {
			return issuer, true
		}
	}
	return Issuer{}, false
}

func parseIssuerCertificate(issuer Issuer) (*x509.Certificate, error) {
	block, rest := pem.Decode([]byte(issuer.CertificatePEM))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, manifestError(fmt.Sprintf("issuer %q certificate_pem must contain one certificate", issuer.ID))
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, manifestError(fmt.Sprintf("issuer %q certificate: %v", issuer.ID, err))
	}
	if !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, manifestError(fmt.Sprintf("issuer %q certificate is not a certificate authority", issuer.ID))
	}
	return certificate, nil
}

func validateManifestShape(manifest Manifest) error {
	if manifest.Version != ManifestVersion {
		return manifestError(fmt.Sprintf("version %d is not supported", manifest.Version))
	}
	if err := validateIdentifier("group_id", manifest.GroupID); err != nil {
		return err
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return manifestError("name is required")
	}
	if manifest.Generation == 0 {
		return manifestError("generation must be positive")
	}
	if _, err := manifest.MaxLifetime(); err != nil {
		return err
	}
	if len(manifest.Issuers) == 0 {
		return manifestError("at least one issuer is required")
	}
	if len(manifest.Nodes) == 0 || len(manifest.Nodes) > maxManifestNodes {
		return manifestError(fmt.Sprintf("nodes must contain 1 to %d entries", maxManifestNodes))
	}
	issuerIDs := map[string]Issuer{}
	for _, issuer := range manifest.Issuers {
		if err := validateIdentifier("issuer id", issuer.ID); err != nil {
			return err
		}
		if issuer.Kind != "node" && issuer.Kind != "frontend" {
			return manifestError(fmt.Sprintf("issuer %q kind must be node or frontend", issuer.ID))
		}
		if _, exists := issuerIDs[issuer.ID]; exists {
			return manifestError(fmt.Sprintf("issuer %q is duplicated", issuer.ID))
		}
		if _, err := parseIssuerCertificate(issuer); err != nil {
			return err
		}
		if err := validateCapabilities(issuer.AllowedScopes); err != nil {
			return manifestError(fmt.Sprintf("issuer %q: %v", issuer.ID, err))
		}
		issuerIDs[issuer.ID] = issuer
	}
	nodeIDs := map[string]struct{}{}
	nodeNames := map[string]struct{}{}
	for _, node := range manifest.Nodes {
		if err := validateIdentifier("node id", node.ID); err != nil {
			return err
		}
		if _, exists := nodeIDs[node.ID]; exists {
			return manifestError(fmt.Sprintf("node %q is duplicated", node.ID))
		}
		name := strings.TrimSpace(node.Name)
		if name == "" {
			return manifestError(fmt.Sprintf("node %q name is required", node.ID))
		}
		if _, exists := nodeNames[name]; exists {
			return manifestError(fmt.Sprintf("node name %q is duplicated", name))
		}
		issuer, exists := issuerIDs[node.IssuerID]
		if !exists || issuer.Kind != "node" {
			return manifestError(fmt.Sprintf("node %q references an invalid node issuer", node.ID))
		}
		if err := validateGroupAddress(node.Address); err != nil {
			return manifestError(fmt.Sprintf("node %q address: %v", node.ID, err))
		}
		if err := validateCapabilities(node.Scopes); err != nil {
			return manifestError(fmt.Sprintf("node %q: %v", node.ID, err))
		}
		if !capabilitySubset(node.Scopes, issuer.AllowedScopes) {
			return manifestError(fmt.Sprintf("node %q scopes exceed issuer %q", node.ID, issuer.ID))
		}
		nodeIDs[node.ID] = struct{}{}
		nodeNames[name] = struct{}{}
	}
	if err := validateUniqueIdentifiers("revoked issuer", manifest.RevokedIssuerIDs); err != nil {
		return err
	}
	if err := validateUniqueIdentifiers("revoked node", manifest.RevokedNodeIDs); err != nil {
		return err
	}
	return nil
}

func validateGroupAddress(address string) error {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an https origin")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("host is required")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("wildcard addresses are not frontend destinations")
	}
	return nil
}

func validateIdentifier(kind, value string) error {
	if len(value) < 3 || len(value) > 128 {
		return manifestError(fmt.Sprintf("%s must contain 3 to 128 URL-safe characters", kind))
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return manifestError(fmt.Sprintf("%s must contain only URL-safe characters", kind))
	}
	return nil
}

func validateCapabilities(capabilities []Capability) error {
	seen := map[Capability]struct{}{}
	for _, capability := range capabilities {
		if _, ok := knownCapabilities[capability]; !ok {
			return fmt.Errorf("unknown capability %q", capability)
		}
		if _, ok := seen[capability]; ok {
			return fmt.Errorf("capability %q is duplicated", capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func capabilitySubset(candidate, allowed []Capability) bool {
	set := map[Capability]struct{}{}
	for _, capability := range allowed {
		set[capability] = struct{}{}
	}
	for _, capability := range candidate {
		if _, ok := set[capability]; !ok {
			return false
		}
	}
	return true
}

func validateUniqueIdentifiers(kind string, values []string) error {
	seen := map[string]struct{}{}
	for _, value := range values {
		if err := validateIdentifier(kind, value); err != nil {
			return err
		}
		if _, ok := seen[value]; ok {
			return manifestError(fmt.Sprintf("%s %q is duplicated", kind, value))
		}
		seen[value] = struct{}{}
	}
	return nil
}

func manifestSigningPayload(manifest Manifest) ([]byte, error) {
	manifest.Signature = ""
	manifest.Issuers = append([]Issuer(nil), manifest.Issuers...)
	manifest.Nodes = append([]Node(nil), manifest.Nodes...)
	manifest.RevokedIssuerIDs = append([]string(nil), manifest.RevokedIssuerIDs...)
	manifest.RevokedNodeIDs = append([]string(nil), manifest.RevokedNodeIDs...)
	for i := range manifest.Issuers {
		manifest.Issuers[i].AllowedScopes = append([]Capability(nil), manifest.Issuers[i].AllowedScopes...)
		sort.Slice(manifest.Issuers[i].AllowedScopes, func(a, b int) bool {
			return manifest.Issuers[i].AllowedScopes[a] < manifest.Issuers[i].AllowedScopes[b]
		})
	}
	for i := range manifest.Nodes {
		manifest.Nodes[i].Scopes = append([]Capability(nil), manifest.Nodes[i].Scopes...)
		sort.Slice(manifest.Nodes[i].Scopes, func(a, b int) bool { return manifest.Nodes[i].Scopes[a] < manifest.Nodes[i].Scopes[b] })
	}
	sort.Slice(manifest.Issuers, func(i, j int) bool { return manifest.Issuers[i].ID < manifest.Issuers[j].ID })
	sort.Slice(manifest.Nodes, func(i, j int) bool { return manifest.Nodes[i].ID < manifest.Nodes[j].ID })
	sort.Strings(manifest.RevokedIssuerIDs)
	sort.Strings(manifest.RevokedNodeIDs)
	payload, err := json.Marshal(manifest)
	if err != nil {
		return nil, manifestError(err.Error())
	}
	return payload, nil
}

func manifestError(detail string) error {
	return &SecurityError{Reason: SecurityInvalidManifest, Detail: detail}
}
