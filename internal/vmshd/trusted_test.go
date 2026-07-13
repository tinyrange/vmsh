package vmshd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tinyrange/vmsh/internal/trusted"
	"j5.nz/cc/client"
)

func TestTrustedManagerBindsGatewayAdmissionToRunningVMGeneration(t *testing.T) {
	root := t.TempDir()
	executablePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	executableDigest := sha256.Sum256(executable)
	profiles := filepath.Join(root, "profiles")
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := trusted.Profile{
		Version:          1,
		ID:               "development",
		Risk:             trusted.RiskDelegated,
		TargetID:         "local-host",
		HandshakeTimeout: "1s",
		DefaultRootID:    "workspace",
		Roots:            map[string]trusted.Root{"workspace": {Path: root}},
		Actions: map[string]trusted.Action{"echo": {
			Executable:       executablePath,
			ExecutableDigest: hex.EncodeToString(executableDigest[:]),
			RootIDs:          []string{"workspace"},
			ArgumentRules:    []trusted.ArgumentRule{{Position: 0, Pattern: "hello"}},
			MaxRequestBytes:  4096,
			MaxDuration:      "5s",
		}},
	}
	if err := trusted.FinalizeProfile(&profile); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiles, "development.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "grants"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "audit"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &trustedManager{root: root, gateways: make(map[string]*trustedGateway)}
	t.Cleanup(manager.close)
	var admittedVM string
	var admittedPort int
	runtimeView := fakeRuntimeView{
		statuses: []client.InstanceState{{ID: "work", Status: "running", StartedAt: "2026-07-14T00:00:00Z"}},
		allowPort: func(_ context.Context, vmID string, port int) error {
			admittedVM, admittedPort = vmID, port
			return nil
		},
	}
	info, err := manager.grant(context.Background(), runtimeView, TrustGrantRequest{VMID: "work", Profile: "development"})
	if runtime.GOOS == "windows" {
		if err == nil || admittedPort != 0 {
			t.Fatalf("Windows grant did not fail closed without ACL enforcement: info=%#v port=%d err=%v", info, admittedPort, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if admittedVM != "work" || admittedPort == 0 || info.ServicePort != admittedPort || info.SourceGeneration != sourceGeneration("work", "2026-07-14T00:00:00Z") {
		t.Fatalf("gateway admission was not bound to the running VM: info=%#v admitted=%s:%d", info, admittedVM, admittedPort)
	}
	revoked, err := manager.revoke("work")
	if err != nil {
		t.Fatal(err)
	}
	if !revoked.Revoked || revoked.Token != "" {
		t.Fatalf("revocation did not remove active credentials: %#v", revoked)
	}
}
