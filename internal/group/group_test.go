package group

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"j5.nz/cc/client"
)

func TestManifestRejectsUnsignedChangeAndGenerationConflict(t *testing.T) {
	fixture := newGroupFixture(t, 1)
	tampered := fixture.manifest
	tampered.Name = "tampered"
	if err := VerifyManifest(tampered); err == nil {
		t.Fatal("unsigned manifest change was accepted")
	}

	conflict := fixture.manifest
	conflict.Name = "conflict"
	if err := SignManifest(&conflict, fixture.ownerKey); err != nil {
		t.Fatal(err)
	}
	_, err := ReconcileManifest(fixture.manifest, conflict)
	var securityError *SecurityError
	if !errors.As(err, &securityError) || securityError.Reason != SecurityManifestConflict {
		t.Fatalf("expected a manifest conflict, got %v", err)
	}
}

func TestInventoryRequiresManifestBoundMTLSAndReportsUnavailableNodes(t *testing.T) {
	fixture := newGroupFixture(t, 2)
	nodeCertificate := fixture.issue(t, "node-ca", "node", "node-one", "node-one", []Capability{CapabilityGroupRead, CapabilityVMRead}, []string{"localhost"})
	frontendCertificate := fixture.issue(t, "frontend-ca", "frontend", "frontend-one", "", []Capability{CapabilityGroupRead}, nil)

	server, err := Listen(ServerConfig{
		ListenAddress: fixture.listenAddress,
		NodeID:        "node-one",
		Manifest:      fixture.manifest,
		Certificate:   nodeCertificate,
		Provider:      func() []client.InstanceState { return []client.InstanceState{{ID: "vm-one"}} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})

	response, err := http.Get("http://" + server.Address() + inventoryPath)
	if err == nil {
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("plaintext request reached the inventory endpoint")
		}
	}

	snapshot, err := Fetch(context.Background(), ClientConfig{Manifest: fixture.manifest, Certificate: frontendCertificate, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Inventories) != 1 || snapshot.Inventories[0].Node.ID != "node-one" || len(snapshot.Inventories[0].Inventory.VMs) != 1 || snapshot.Inventories[0].Inventory.VMs[0].ID != "vm-one" {
		t.Fatalf("unexpected inventory: %#v", snapshot)
	}
	if len(snapshot.Unavailable) != 1 || snapshot.Unavailable[0] != "node-two" {
		t.Fatalf("unavailable nodes = %v, want node-two", snapshot.Unavailable)
	}
}

func TestCredentialCannotExceedIssuerScope(t *testing.T) {
	fixture := newGroupFixture(t, 1)
	_, err := fixture.issueResult(t, "frontend-ca", "frontend", "frontend-one", "", []Capability{CapabilityHostExec}, nil)
	var securityError *SecurityError
	if !errors.As(err, &securityError) || securityError.Reason != SecurityCapabilityDenied {
		t.Fatalf("expected capability denial, got %v", err)
	}
}

type groupFixture struct {
	manifest      Manifest
	ownerKey      ed25519.PrivateKey
	issuerCerts   map[string]*x509.Certificate
	issuerKeys    map[string]ed25519.PrivateKey
	listenAddress string
}

func newGroupFixture(t *testing.T, nodeCount int) *groupFixture {
	t.Helper()
	ownerPublic, ownerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = ownerPublic
	nodeCA, nodeKey := newCA(t, "node-ca")
	frontendCA, frontendKey := newCA(t, "frontend-ca")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddress := listener.Addr().String()
	listener.Close()
	manifest := Manifest{
		Version:                ManifestVersion,
		GroupID:                "test-group",
		Name:                   "test",
		Generation:             1,
		MaxCertificateLifetime: "5m",
		Issuers: []Issuer{
			{ID: "node-ca", Kind: "node", CertificatePEM: certificatePEM(nodeCA), AllowedScopes: []Capability{CapabilityGroupRead, CapabilityVMRead}},
			{ID: "frontend-ca", Kind: "frontend", CertificatePEM: certificatePEM(frontendCA), AllowedScopes: []Capability{CapabilityGroupRead}},
		},
		Nodes: []Node{{ID: "node-one", Name: "one", Address: "https://localhost:" + port(t, listenAddress), IssuerID: "node-ca", Scopes: []Capability{CapabilityGroupRead, CapabilityVMRead}}},
	}
	if nodeCount > 1 {
		manifest.Nodes = append(manifest.Nodes, Node{ID: "node-two", Name: "two", Address: "https://localhost:1", IssuerID: "node-ca", Scopes: []Capability{CapabilityGroupRead, CapabilityVMRead}})
	}
	if err := SignManifest(&manifest, ownerKey); err != nil {
		t.Fatal(err)
	}
	return &groupFixture{manifest: manifest, ownerKey: ownerKey, issuerCerts: map[string]*x509.Certificate{"node-ca": nodeCA, "frontend-ca": frontendCA}, issuerKeys: map[string]ed25519.PrivateKey{"node-ca": nodeKey, "frontend-ca": frontendKey}, listenAddress: listenAddress}
}

func (f *groupFixture) issue(t *testing.T, issuerID, kind, identityID, nodeID string, scopes []Capability, dnsNames []string) tls.Certificate {
	t.Helper()
	certificate, err := f.issueResult(t, issuerID, kind, identityID, nodeID, scopes, dnsNames)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func (f *groupFixture) issueResult(t *testing.T, issuerID, kind, identityID, nodeID string, scopes []Capability, dnsNames []string) (tls.Certificate, error) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	certificatePEM, err := IssueCredential(f.manifest, f.issuerCerts[issuerID], f.issuerKeys[issuerID], CredentialRequest{IssuerID: issuerID, Kind: kind, IdentityID: identityID, NodeID: nodeID, Scopes: scopes, PublicKey: publicKey, NotBefore: now.Add(-time.Second), NotAfter: now.Add(time.Minute), DNSNames: dnsNames})
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return TLSCertificate(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func newCA(t *testing.T, name string) (*x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, privateKey
}

func certificatePEM(certificate *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))
}

func port(t *testing.T, address string) string {
	t.Helper()
	_, value, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
