package group

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"j5.nz/cc/client"
)

const (
	ProtocolVersion     = 1
	protocolPath        = "/vmsh/group/protocol"
	inventoryPath       = "/vmsh/group/inventory"
	maxInventoryBody    = 8 << 20
	serverHeaderTimeout = 15 * time.Second
)

type ProtocolInfo struct {
	Version      int          `json:"version"`
	MinVersion   int          `json:"min_version"`
	GroupID      string       `json:"group_id"`
	Generation   uint64       `json:"generation"`
	NodeID       string       `json:"node_id"`
	Capabilities []Capability `json:"capabilities"`
}

type Inventory struct {
	Protocol   ProtocolInfo           `json:"protocol"`
	CapturedAt time.Time              `json:"captured_at"`
	VMs        []client.InstanceState `json:"vms"`
}

type NodeInventory struct {
	Node      Node      `json:"node"`
	Inventory Inventory `json:"inventory"`
}

type Snapshot struct {
	GroupID     string          `json:"group_id"`
	Generation  uint64          `json:"generation"`
	Inventories []NodeInventory `json:"inventories"`
	Unavailable []string        `json:"unavailable"`
}

type ServerConfig struct {
	ListenAddress string
	NodeID        string
	Manifest      Manifest
	Certificate   tls.Certificate
	Provider      func() []client.InstanceState
}

type Server struct {
	http     *http.Server
	listener net.Listener
	done     chan struct{}
	once     sync.Once
}

func Listen(config ServerConfig) (*Server, error) {
	if err := VerifyManifest(config.Manifest); err != nil {
		return nil, err
	}
	node, ok := config.Manifest.NodeByID(config.NodeID)
	if !ok {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: "server node is not in the manifest"}
	}
	leaf, err := leafCertificate(config.Certificate)
	if err != nil {
		return nil, err
	}
	if _, err := VerifyPeer(config.Manifest, leaf, "node", node.ID, CapabilityGroupRead); err != nil {
		return nil, err
	}
	if config.Provider == nil {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: "inventory provider is required"}
	}
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{config.Certificate},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return credentialError("frontend certificate is required")
			}
			if err := verifyCertificateChain(config.Manifest, state.PeerCertificates[0], state.PeerCertificates[1:], "", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}); err != nil {
				return credentialError(err.Error())
			}
			_, err := VerifyPeer(config.Manifest, state.PeerCertificates[0], "frontend", "", CapabilityGroupRead)
			return err
		},
		SessionTicketsDisabled: true,
	}
	protocol := ProtocolInfo{Version: ProtocolVersion, MinVersion: ProtocolVersion, GroupID: config.Manifest.GroupID, Generation: config.Manifest.Generation, NodeID: node.ID, Capabilities: []Capability{CapabilityGroupRead, CapabilityVMRead}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocolPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, protocol)
	})
	mux.HandleFunc("GET "+inventoryPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Inventory{Protocol: protocol, CapturedAt: time.Now().UTC(), VMs: config.Provider()})
	})
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: serverHeaderTimeout}
	server := &Server{http: httpServer, listener: tls.NewListener(listener, tlsConfig), done: make(chan struct{})}
	go func() {
		defer close(server.done)
		_ = httpServer.Serve(server.listener)
	}()
	return server, nil
}

func (s *Server) Address() string { return s.listener.Addr().String() }

func (s *Server) Close(ctx context.Context) error {
	var err error
	s.once.Do(func() { err = s.http.Shutdown(ctx) })
	return err
}

type ClientConfig struct {
	Manifest    Manifest
	Certificate tls.Certificate
	Timeout     time.Duration
}

func Fetch(ctx context.Context, config ClientConfig) (Snapshot, error) {
	if err := VerifyManifest(config.Manifest); err != nil {
		return Snapshot{}, err
	}
	if config.Timeout <= 0 {
		return Snapshot{}, &SecurityError{Reason: SecurityConfigurationError, Detail: "a fact-based positive node timeout is required"}
	}
	leaf, err := leafCertificate(config.Certificate)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := VerifyPeer(config.Manifest, leaf, "frontend", "", CapabilityGroupRead); err != nil {
		return Snapshot{}, err
	}
	type result struct {
		node      Node
		inventory Inventory
		err       error
	}
	results := make(chan result, len(config.Manifest.Nodes))
	for _, node := range config.Manifest.Nodes {
		go func(node Node) {
			nodeContext, cancel := context.WithTimeout(ctx, config.Timeout)
			defer cancel()
			inventory, err := fetchNode(nodeContext, config.Manifest, config.Certificate, node)
			results <- result{node: node, inventory: inventory, err: err}
		}(node)
	}
	snapshot := Snapshot{GroupID: config.Manifest.GroupID, Generation: config.Manifest.Generation}
	for range config.Manifest.Nodes {
		result := <-results
		if result.err != nil {
			snapshot.Unavailable = append(snapshot.Unavailable, result.node.ID)
			continue
		}
		snapshot.Inventories = append(snapshot.Inventories, NodeInventory{Node: result.node, Inventory: result.inventory})
	}
	sort.Slice(snapshot.Inventories, func(i, j int) bool { return snapshot.Inventories[i].Node.ID < snapshot.Inventories[j].Node.ID })
	sort.Strings(snapshot.Unavailable)
	if len(snapshot.Inventories) == 0 {
		return snapshot, fmt.Errorf("no group nodes were reachable")
	}
	return snapshot, nil
}

func fetchNode(ctx context.Context, manifest Manifest, certificate tls.Certificate, node Node) (Inventory, error) {
	base, err := url.Parse(node.Address)
	if err != nil {
		return Inventory{}, err
	}
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{certificate},
		ServerName:         base.Hostname(),
		InsecureSkipVerify: true, // The complete manifest-bound verification is performed below.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return credentialError("node certificate is required")
			}
			if err := verifyCertificateChain(manifest, state.PeerCertificates[0], state.PeerCertificates[1:], base.Hostname(), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}); err != nil {
				return credentialError(err.Error())
			}
			_, err := VerifyPeer(manifest, state.PeerCertificates[0], "node", node.ID, CapabilityGroupRead)
			return err
		},
		SessionTicketsDisabled: true,
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	protocol, err := getJSON[ProtocolInfo](ctx, httpClient, strings.TrimRight(node.Address, "/")+protocolPath)
	if err != nil {
		return Inventory{}, err
	}
	if protocol.Version < ProtocolVersion || protocol.MinVersion > ProtocolVersion || protocol.GroupID != manifest.GroupID || protocol.Generation != manifest.Generation || protocol.NodeID != node.ID {
		return Inventory{}, errors.New("node protocol is incompatible with the signed manifest")
	}
	inventory, err := getJSON[Inventory](ctx, httpClient, strings.TrimRight(node.Address, "/")+inventoryPath)
	if err != nil {
		return Inventory{}, err
	}
	if inventory.Protocol.Version != protocol.Version || inventory.Protocol.MinVersion != protocol.MinVersion || inventory.Protocol.GroupID != protocol.GroupID || inventory.Protocol.Generation != protocol.Generation || inventory.Protocol.NodeID != protocol.NodeID {
		return Inventory{}, errors.New("node inventory protocol changed during the request")
	}
	return inventory, nil
}

func getJSON[T any](ctx context.Context, client *http.Client, address string) (T, error) {
	var value T
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return value, err
	}
	response, err := client.Do(request)
	if err != nil {
		return value, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return value, fmt.Errorf("group node returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxInventoryBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	return value, nil
}

func leafCertificate(certificate tls.Certificate) (*x509.Certificate, error) {
	if certificate.Leaf != nil {
		return certificate.Leaf, nil
	}
	if len(certificate.Certificate) == 0 {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: "TLS certificate is empty"}
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: err.Error()}
	}
	return leaf, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
