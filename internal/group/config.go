package group

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"j5.nz/cc/client"
)

type TLSFiles struct {
	Manifest    string `json:"manifest"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
}

type ServerFileConfig struct {
	ListenAddress string   `json:"listen_address"`
	NodeID        string   `json:"node_id"`
	TLS           TLSFiles `json:"tls"`
}

type ClientFileConfig struct {
	TLS         TLSFiles `json:"tls"`
	NodeTimeout string   `json:"node_timeout"`
}

func LoadClientConfig(path string) (ClientConfig, error) {
	contents, err := readSecurityFile(path, true)
	if err != nil {
		return ClientConfig{}, err
	}
	var fileConfig ClientFileConfig
	if err := json.Unmarshal(contents, &fileConfig); err != nil {
		return ClientConfig{}, &SecurityError{Reason: SecurityConfigurationError, Detail: err.Error()}
	}
	timeout, err := time.ParseDuration(fileConfig.NodeTimeout)
	if err != nil || timeout <= 0 {
		return ClientConfig{}, &SecurityError{Reason: SecurityConfigurationError, Detail: "node_timeout must be a positive duration chosen for this group"}
	}
	manifest, certPEM, keyPEM, err := LoadTLSFiles(fileConfig.TLS)
	if err != nil {
		return ClientConfig{}, err
	}
	certificate, err := TLSCertificate(certPEM, keyPEM)
	if err != nil {
		return ClientConfig{}, err
	}
	return ClientConfig{Manifest: manifest, Certificate: certificate, Timeout: timeout}, nil
}

func LoadServerConfig(path string, provider func() []client.InstanceState) (ServerConfig, error) {
	contents, err := readSecurityFile(path, true)
	if err != nil {
		return ServerConfig{}, err
	}
	var fileConfig ServerFileConfig
	if err := json.Unmarshal(contents, &fileConfig); err != nil {
		return ServerConfig{}, &SecurityError{Reason: SecurityConfigurationError, Detail: err.Error()}
	}
	manifest, certPEM, keyPEM, err := LoadTLSFiles(fileConfig.TLS)
	if err != nil {
		return ServerConfig{}, err
	}
	certificate, err := TLSCertificate(certPEM, keyPEM)
	if err != nil {
		return ServerConfig{}, err
	}
	if fileConfig.ListenAddress == "" || fileConfig.NodeID == "" {
		return ServerConfig{}, &SecurityError{Reason: SecurityConfigurationError, Detail: "listen_address and node_id are required"}
	}
	return ServerConfig{ListenAddress: fileConfig.ListenAddress, NodeID: fileConfig.NodeID, Manifest: manifest, Certificate: certificate, Provider: provider}, nil
}

func LoadManifest(path string) (Manifest, error) {
	contents, err := readSecurityFile(path, false)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return Manifest{}, &SecurityError{Reason: SecurityConfigurationError, Detail: err.Error()}
	}
	if err := VerifyManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func LoadTLSFiles(files TLSFiles) (Manifest, []byte, []byte, error) {
	manifest, err := LoadManifest(files.Manifest)
	if err != nil {
		return Manifest{}, nil, nil, err
	}
	certificate, err := readSecurityFile(files.Certificate, false)
	if err != nil {
		return Manifest{}, nil, nil, err
	}
	privateKey, err := readSecurityFile(files.PrivateKey, true)
	if err != nil {
		return Manifest{}, nil, nil, err
	}
	return manifest, certificate, privateKey, nil
}

func readSecurityFile(path string, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: fmt.Sprintf("security file %q must use an absolute path", path)}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: err.Error()}
	}
	if !info.Mode().IsRegular() {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: fmt.Sprintf("security file %q is not regular", path)}
	}
	if private && fileAccessibleByOthers(info) {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: fmt.Sprintf("private key %q is accessible by another user", path)}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, &SecurityError{Reason: SecurityConfigurationError, Detail: err.Error()}
	}
	return contents, nil
}
