package trusted

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEvaluateRejectsProfileMutationAndPathEscape(t *testing.T) {
	profile, grant := testProfile(t)
	request := testRequest(profile, grant)
	request.RelativeCWD = ".."
	if _, err := Evaluate(profile, &grant, request, time.Now()); !IsDenial(err, DeniedWorkingDirectory) {
		t.Fatalf("expected working-directory denial, got %v", err)
	}

	profile.Actions["echo"] = Action{Executable: profile.Actions["echo"].Executable, RootIDs: []string{"workspace"}, MaxRequestBytes: 4096, MaxDuration: "5s", AllowTrailingArgs: true}
	request.RelativeCWD = "."
	if _, err := Evaluate(profile, &grant, request, time.Now()); !IsDenial(err, DeniedProfile) {
		t.Fatalf("expected mutated-profile denial, got %v", err)
	}
}

func TestTrustedHelperProcess(t *testing.T) {
	if os.Getenv("VMSH_TRUSTED_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, os.Args[len(os.Args)-1])
	os.Exit(0)
}

func TestGatewayExecutesStructuredActionAndRejectsReplay(t *testing.T) {
	profile, grant := testProfile(t)
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	gateway, err := ListenGateway(GatewayConfig{Profile: profile, Grant: grant, Token: token, HandshakeTimeout: time.Second, AuditPath: auditPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gateway.Close() })

	request := testRequest(profile, grant)
	request.SourceVMID = "spoofed"
	events := callGateway(t, gateway.Port(), Envelope{Token: token, Request: request})
	if len(events) < 3 || events[0].Kind != "accepted" || events[len(events)-1].Kind != "exit" || events[len(events)-1].ExitCode == nil || *events[len(events)-1].ExitCode != 0 {
		t.Fatalf("unexpected event sequence: %#v", events)
	}
	var output []byte
	for _, event := range events {
		if event.Kind == "output" && event.Stream == "stdout" {
			output = append(output, event.Data...)
		}
	}
	if string(output) != "hello\n" {
		t.Fatalf("stdout = %q, want exact echo bytes", output)
	}

	replayed := callGateway(t, gateway.Port(), Envelope{Token: token, Request: request})
	if len(replayed) != 1 || replayed[0].Error == nil || replayed[0].Error.Reason != DeniedReplay {
		t.Fatalf("replay was not rejected structurally: %#v", replayed)
	}
	gateway.Revoke()
	request.CallID = "call-two"
	request.Sequence++
	revoked := callGateway(t, gateway.Port(), Envelope{Token: token, Request: request})
	if len(revoked) != 1 || revoked[0].Error == nil || revoked[0].Error.Reason != DeniedRevoked {
		t.Fatalf("revoked gateway accepted a new call: %#v", revoked)
	}
	info, err := os.Stat(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("audit permissions = %o, want owner-only", info.Mode().Perm())
	}
}

func testProfile(t *testing.T) (Profile, Grant) {
	t.Helper()
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
	profile := Profile{
		Version:          1,
		ID:               "development",
		Risk:             RiskDelegated,
		TargetID:         "local-host",
		HandshakeTimeout: "1s",
		Roots:            map[string]Root{"workspace": {Path: root}},
		DefaultRootID:    "workspace",
		Actions: map[string]Action{"echo": {
			Executable:       executablePath,
			ExecutableDigest: hex.EncodeToString(executableDigest[:]),
			RootIDs:          []string{"workspace"},
			ArgumentRules:    []ArgumentRule{{Position: 0, Pattern: "-test.run=TestTrustedHelperProcess"}, {Position: 1, Pattern: "--"}, {Position: 2, Pattern: "hello"}},
			Environment:      map[string]string{"VMSH_TRUSTED_HELPER": "1"},
			MaxRequestBytes:  4096,
			MaxDuration:      "5s",
		}},
	}
	if err := FinalizeProfile(&profile); err != nil {
		t.Fatal(err)
	}
	grant := Grant{ID: "grant-one", SourceVMID: "vm-one", SourceGeneration: 1, TargetID: profile.TargetID, ProfileID: profile.ID, ProfileDigest: profile.Digest, RevocationGeneration: 1, CreatedAt: time.Now()}
	return profile, grant
}

func testRequest(profile Profile, grant Grant) Request {
	return Request{Version: ProtocolVersion, CallID: "call-one", Sequence: 1, SourceVMID: grant.SourceVMID, SourceGeneration: grant.SourceGeneration, TargetID: profile.TargetID, ProfileDigest: profile.Digest, ActionID: "echo", Arguments: []string{"-test.run=TestTrustedHelperProcess", "--", "hello"}, RootID: "workspace", RelativeCWD: ".", Deadline: time.Now().Add(4 * time.Second)}
}

func callGateway(t *testing.T, port int, envelope Envelope) []Event {
	t.Helper()
	connection, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := json.NewEncoder(connection).Encode(envelope); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(connection)
	var events []Event
	for {
		var event Event
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		if event.Kind == "exit" || event.Kind == "error" {
			return events
		}
	}
}
