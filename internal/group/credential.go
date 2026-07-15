package group

import (
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"
)

const identityScheme = "urn:vmsh:identity"

type CredentialRequest struct {
	IssuerID   string
	Kind       string
	IdentityID string
	NodeID     string
	Scopes     []Capability
	PublicKey  crypto.PublicKey
	NotBefore  time.Time
	NotAfter   time.Time
	DNSNames   []string
}

type Identity struct {
	Kind       string
	ID         string
	IssuerID   string
	NodeID     string
	Scopes     []Capability
	Generation uint64
}

func IssueCredential(manifest Manifest, issuerCertificate *x509.Certificate, issuerKey crypto.Signer, request CredentialRequest) ([]byte, error) {
	if err := VerifyManifest(manifest); err != nil {
		return nil, err
	}
	issuer, ok := manifest.IssuerByID(request.IssuerID)
	if !ok || issuer.Kind != request.Kind || slices.Contains(manifest.RevokedIssuerIDs, request.IssuerID) {
		return nil, &SecurityError{Reason: SecurityCapabilityDenied, Detail: "issuer is not authorized for the requested identity kind"}
	}
	if request.Kind != "node" && request.Kind != "frontend" {
		return nil, credentialError("identity kind must be node or frontend")
	}
	if err := validateIdentifier("identity id", request.IdentityID); err != nil {
		return nil, credentialError(err.Error())
	}
	if !capabilitySubset(request.Scopes, issuer.AllowedScopes) {
		return nil, &SecurityError{Reason: SecurityCapabilityDenied, Detail: "requested scopes exceed issuer grant"}
	}
	if request.Kind == "node" {
		node, exists := manifest.NodeByID(request.NodeID)
		if !exists || node.IssuerID != request.IssuerID || request.IdentityID != request.NodeID || !capabilitySubset(request.Scopes, node.Scopes) {
			return nil, &SecurityError{Reason: SecurityIdentityMismatch, Detail: "node credential does not match its enrollment"}
		}
	}
	maxLifetime, _ := manifest.MaxLifetime()
	if request.NotBefore.IsZero() || !request.NotAfter.After(request.NotBefore) || request.NotAfter.Sub(request.NotBefore) > maxLifetime {
		return nil, credentialError("credential lifetime is invalid or exceeds the manifest limit")
	}
	if issuerCertificate == nil || issuerKey == nil || !issuerCertificate.IsCA {
		return nil, credentialError("issuer certificate and key are required")
	}
	manifestIssuer, err := parseIssuerCertificate(issuer)
	if err != nil {
		return nil, err
	}
	if !issuerCertificate.Equal(manifestIssuer) {
		return nil, &SecurityError{Reason: SecurityIdentityMismatch, Detail: "issuer certificate is not the one in the manifest"}
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, err
	}
	identityURI, _ := url.Parse(fmt.Sprintf("%s:%s:%d:%s:%s:%s", identityScheme, manifest.GroupID, manifest.Generation, request.Kind, request.IdentityID, request.NodeID))
	capabilityURIs := make([]*url.URL, 0, len(request.Scopes)+1)
	capabilityURIs = append(capabilityURIs, identityURI)
	for _, scope := range request.Scopes {
		u, _ := url.Parse("urn:vmsh:capability:" + string(scope))
		capabilityURIs = append(capabilityURIs, u)
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if request.Kind == "node" {
		usage = append(usage, x509.ExtKeyUsageServerAuth)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: request.IdentityID},
		NotBefore:    request.NotBefore,
		NotAfter:     request.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usage,
		DNSNames:     request.DNSNames,
		URIs:         capabilityURIs,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuerCertificate, request.PublicKey, issuerKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func VerifyPeer(manifest Manifest, certificate *x509.Certificate, expectedKind, expectedNode string, requiredScopes ...Capability) (Identity, error) {
	if err := VerifyManifest(manifest); err != nil {
		return Identity{}, err
	}
	if certificate == nil {
		return Identity{}, credentialError("peer did not present a certificate")
	}
	identity, err := parseIdentity(manifest, certificate)
	if err != nil {
		return Identity{}, err
	}
	if identity.Kind != expectedKind {
		return Identity{}, &SecurityError{Reason: SecurityIdentityMismatch, Detail: "credential has the wrong identity kind"}
	}
	if expectedNode != "" && identity.NodeID != expectedNode {
		return Identity{}, &SecurityError{Reason: SecurityIdentityMismatch, Detail: "credential is for a different node"}
	}
	if !capabilitySubset(requiredScopes, identity.Scopes) {
		return Identity{}, &SecurityError{Reason: SecurityCapabilityDenied, Detail: "credential lacks a required scope"}
	}
	return identity, nil
}

func parseIdentity(manifest Manifest, certificate *x509.Certificate) (Identity, error) {
	var identity Identity
	for _, uri := range certificate.URIs {
		value := uri.String()
		if strings.HasPrefix(value, identityScheme+":") {
			if identity.ID != "" {
				return Identity{}, credentialError("credential has multiple identities")
			}
			parts := strings.Split(strings.TrimPrefix(value, identityScheme+":"), ":")
			if len(parts) != 5 {
				return Identity{}, credentialError("credential identity is malformed")
			}
			var generation uint64
			if _, err := fmt.Sscan(parts[1], &generation); err != nil {
				return Identity{}, credentialError("credential generation is malformed")
			}
			if parts[0] != manifest.GroupID {
				return Identity{}, &SecurityError{Reason: SecurityWrongGroup, Detail: "credential belongs to another group"}
			}
			if generation != manifest.Generation {
				return Identity{}, &SecurityError{Reason: SecurityWrongGeneration, Detail: "credential is from another manifest generation"}
			}
			identity = Identity{Kind: parts[2], ID: parts[3], NodeID: parts[4], Generation: generation}
		} else if strings.HasPrefix(value, "urn:vmsh:capability:") {
			capability := Capability(strings.TrimPrefix(value, "urn:vmsh:capability:"))
			if _, ok := knownCapabilities[capability]; !ok {
				return Identity{}, credentialError("credential contains an unknown capability")
			}
			identity.Scopes = append(identity.Scopes, capability)
		}
	}
	if identity.ID == "" {
		return Identity{}, credentialError("credential does not contain an identity")
	}
	for _, issuer := range manifest.Issuers {
		issuerCertificate, err := parseIssuerCertificate(issuer)
		if err == nil && certificate.CheckSignatureFrom(issuerCertificate) == nil {
			identity.IssuerID = issuer.ID
			if issuer.Kind != identity.Kind || slices.Contains(manifest.RevokedIssuerIDs, issuer.ID) || !capabilitySubset(identity.Scopes, issuer.AllowedScopes) {
				return Identity{}, &SecurityError{Reason: SecurityCapabilityDenied, Detail: "credential issuer is not authorized"}
			}
			break
		}
	}
	if identity.IssuerID == "" {
		return Identity{}, credentialError("credential is not signed by a manifest issuer")
	}
	if identity.Kind == "node" {
		node, ok := manifest.NodeByID(identity.NodeID)
		if !ok || node.ID != identity.ID || node.IssuerID != identity.IssuerID || slices.Contains(manifest.RevokedNodeIDs, node.ID) || !capabilitySubset(identity.Scopes, node.Scopes) {
			return Identity{}, &SecurityError{Reason: SecurityRevokedIdentity, Detail: "node identity is not enrolled"}
		}
	}
	return identity, nil
}

func verifyCertificateChain(manifest Manifest, certificate *x509.Certificate, intermediates []*x509.Certificate, serverName string, usages []x509.ExtKeyUsage) error {
	roots := x509.NewCertPool()
	for _, issuer := range manifest.Issuers {
		certificate, err := parseIssuerCertificate(issuer)
		if err != nil {
			return err
		}
		roots.AddCert(certificate)
	}
	pool := x509.NewCertPool()
	for _, intermediate := range intermediates {
		pool.AddCert(intermediate)
	}
	_, err := certificate.Verify(x509.VerifyOptions{Roots: roots, Intermediates: pool, DNSName: serverName, KeyUsages: usages})
	return err
}

func TLSCertificate(certPEM, keyPEM []byte) (tls.Certificate, error) {
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, &SecurityError{Reason: SecurityConfigurationError, Detail: err.Error()}
	}
	return certificate, nil
}

func credentialError(detail string) error {
	return &SecurityError{Reason: SecurityInvalidCredential, Detail: detail}
}
