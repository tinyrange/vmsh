package vmshd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/net/websocket"
	"j5.nz/cc/client"
)

func newHostCapabilityTestEndpoint() *mcpEndpoint {
	return &mcpEndpoint{
		hostChallenges: make(map[string]*mcpHostChallenge),
		hostReadGrants: make(map[string]*mcpHostReadGrant),
		vms:            map[string]mcpVM{"vm": {ID: "vm"}},
	}
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestMCPHostReadChallengeMintsOnlyDirectEntryGrants(t *testing.T) {
	directory := resolvedTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, "payload.bin"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	endpoint := newHostCapabilityTestEndpoint()
	_, challenge, err := endpoint.createHostReadChallenge(context.Background(), nil, mcpHostReadChallengeInput{Directory: directory})
	if err != nil {
		t.Fatalf("create challenge: %v", err)
	}
	manifest, err := json.Marshal(mcpHostReadManifest{Paths: []string{"payload.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(challenge.ResponsePath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	_, claimed, err := endpoint.claimHostReadChallenge(context.Background(), nil, mcpHostReadClaimInput{ChallengeID: challenge.ChallengeID})
	if err != nil {
		t.Fatalf("claim challenge: %v", err)
	}
	if len(claimed.Grants) != 1 || claimed.Grants[0].Path != filepath.Join(directory, "payload.bin") || claimed.Grants[0].Directory {
		t.Fatalf("grants = %#v", claimed.Grants)
	}
	if _, err := os.Lstat(challenge.ResponsePath); !os.IsNotExist(err) {
		t.Fatalf("consumed response still exists: %v", err)
	}

	_, nestedChallenge, err := endpoint.createHostReadChallenge(context.Background(), nil, mcpHostReadChallengeInput{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	nestedManifest, _ := json.Marshal(mcpHostReadManifest{Paths: []string{"child"}})
	if err := os.WriteFile(nestedChallenge.ResponsePath, nestedManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := endpoint.claimHostReadChallenge(context.Background(), nil, mcpHostReadClaimInput{ChallengeID: nestedChallenge.ChallengeID}); err == nil {
		t.Fatal("parent challenge granted a child directory")
	}
	if _, _, err := endpoint.revokeHostGrant(context.Background(), nil, mcpHostGrantRevokeInput{ID: nestedChallenge.ChallengeID}); err != nil {
		t.Fatalf("failed claim did not retain its challenge: %v", err)
	}
}

func TestMCPHostReadDirectoryNeedsChallengeInsideIt(t *testing.T) {
	directory := resolvedTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, "payload"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint := newHostCapabilityTestEndpoint()
	_, challenge, err := endpoint.createHostReadChallenge(context.Background(), nil, mcpHostReadChallengeInput{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(mcpHostReadManifest{Paths: []string{"."}})
	if err := os.WriteFile(challenge.ResponsePath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	_, claimed, err := endpoint.claimHostReadChallenge(context.Background(), nil, mcpHostReadClaimInput{ChallengeID: challenge.ChallengeID})
	if err != nil {
		t.Fatalf("claim directory: %v", err)
	}
	if len(claimed.Grants) != 1 || !claimed.Grants[0].Directory || claimed.Grants[0].Path != directory {
		t.Fatalf("directory grant = %#v", claimed.Grants)
	}
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateMCPHostGrantContents(directory); err == nil {
		t.Fatal("directory grant reached an unchallenged subdirectory")
	}
}

func TestMCPHostChallengeRejectsSymlinkTraversal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks commonly requires additional Windows privileges")
	}
	realDirectory := resolvedTempDir(t)
	parent := resolvedTempDir(t)
	linkedDirectory := filepath.Join(parent, "linked")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatal(err)
	}
	endpoint := newHostCapabilityTestEndpoint()
	_, challenge, err := endpoint.createHostReadChallenge(context.Background(), nil, mcpHostReadChallengeInput{Directory: linkedDirectory})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(mcpHostReadManifest{Paths: []string{"."}})
	if err := os.WriteFile(challenge.ResponsePath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := endpoint.claimHostReadChallenge(context.Background(), nil, mcpHostReadClaimInput{ChallengeID: challenge.ChallengeID}); err == nil {
		t.Fatal("challenge traversing a symlink succeeded")
	}
}

func TestMCPCopyFromHostStreamsGrantedFile(t *testing.T) {
	directory := resolvedTempDir(t)
	source := filepath.Join(directory, "payload.bin")
	payload := bytes.Repeat([]byte{0x00, 0xff, 'v', 'm'}, 40<<10)
	if err := os.WriteFile(source, payload, 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	var received bytes.Buffer
	stream := websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var request client.ExecRequest
		if err := websocket.JSON.Receive(ws, &request); err != nil {
			t.Errorf("receive request: %v", err)
			return
		}
		if request.Kind != "fs_extract" || request.Path != "/work/output.bin" || request.ArchiveLimits != nil {
			t.Errorf("extract request = %#v", request)
			return
		}
		for {
			var input client.ExecInput
			if err := websocket.JSON.Receive(ws, &input); err != nil {
				t.Errorf("receive archive: %v", err)
				return
			}
			if input.Kind == "stdin_close" {
				break
			}
			received.Write(input.Data)
		}
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stream.ServeHTTP(w, r) }))
	defer server.Close()
	endpoint := newHostCapabilityTestEndpoint()
	endpoint.control = client.NewClient(server.URL, nil)
	endpoint.hostReadGrants["grant"] = &mcpHostReadGrant{ID: "grant", Path: source, CreatedAt: time.Now(), info: info}
	if _, result, err := endpoint.copyHostPathToVM(context.Background(), nil, mcpCopyFromHostInput{GrantID: "grant", VMID: "vm", DestinationPath: "/work/output.bin"}); err != nil || !result.Copied {
		t.Fatalf("copy from host = %#v, %v", result, err)
	}
	tr := tar.NewReader(bytes.NewReader(received.Bytes()))
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("read streamed archive: %v", err)
	}
	if header.Name != "payload.bin" {
		t.Fatalf("archive name = %q", header.Name)
	}
	got, err := io.ReadAll(tr)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("archive payload length = %d, error = %v", len(got), err)
	}
}

func TestMCPCopyToHostReplacesOnlyMatchingNoncePlaceholder(t *testing.T) {
	directory := resolvedTempDir(t)
	destination := filepath.Join(directory, "result.bin")
	payload := bytes.Repeat([]byte("guest-output"), 8192)
	archive := testMCPArchive(t, "result.bin", payload)
	stream := websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var request client.ExecRequest
		if err := websocket.JSON.Receive(ws, &request); err != nil {
			return
		}
		if request.Kind != "fs_archive" || request.Path != "/work/result.bin" {
			t.Errorf("archive request = %#v", request)
			return
		}
		var input client.ExecInput
		if err := websocket.JSON.Receive(ws, &input); err != nil || input.Kind != "stdin_close" {
			t.Errorf("archive stdin close = %#v, %v", input, err)
			return
		}
		for offset := 0; offset < len(archive); offset += 16 << 10 {
			end := offset + 16<<10
			if end > len(archive) {
				end = len(archive)
			}
			_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "stdout", Data: archive[offset:end]})
		}
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 0})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stream.ServeHTTP(w, r) }))
	defer server.Close()
	endpoint := newHostCapabilityTestEndpoint()
	endpoint.control = client.NewClient(server.URL, nil)
	_, challenge, err := endpoint.createHostWriteChallenge(context.Background(), nil, mcpHostWriteChallengeInput{Path: destination})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(challenge.Nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	_, result, err := endpoint.copyVMFileToHost(context.Background(), nil, mcpCopyToHostInput{ChallengeID: challenge.ChallengeID, VMID: "vm", SourcePath: "/work/result.bin"})
	if err != nil || !result.Copied || result.Bytes != int64(len(payload)) {
		t.Fatalf("copy to host = %#v, %v", result, err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("host output length = %d, error = %v", len(got), err)
	}
	if _, _, err := endpoint.revokeHostGrant(context.Background(), nil, mcpHostGrantRevokeInput{ID: challenge.ChallengeID}); err == nil {
		t.Fatal("consumed write challenge remained active")
	}
}

func TestMCPCopyToHostFailureRetainsPlaceholderAndChallenge(t *testing.T) {
	directory := resolvedTempDir(t)
	destination := filepath.Join(directory, "result.bin")
	stream := websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var request client.ExecRequest
		if err := websocket.JSON.Receive(ws, &request); err != nil {
			return
		}
		var input client.ExecInput
		_ = websocket.JSON.Receive(ws, &input)
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "error", Error: "source disappeared"})
		_ = websocket.JSON.Send(ws, client.ExecEvent{Kind: "exit", ExitCode: 1})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stream.ServeHTTP(w, r) }))
	defer server.Close()
	endpoint := newHostCapabilityTestEndpoint()
	endpoint.control = client.NewClient(server.URL, nil)
	_, challenge, err := endpoint.createHostWriteChallenge(context.Background(), nil, mcpHostWriteChallengeInput{Path: destination})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(challenge.Nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := endpoint.copyVMFileToHost(context.Background(), nil, mcpCopyToHostInput{ChallengeID: challenge.ChallengeID, VMID: "vm", SourcePath: "/missing"}); err == nil {
		t.Fatal("failed guest archive reported success")
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != challenge.Nonce {
		t.Fatalf("placeholder after failure = %q, %v", got, err)
	}
	if _, _, err := endpoint.revokeHostGrant(context.Background(), nil, mcpHostGrantRevokeInput{ID: challenge.ChallengeID}); err != nil {
		t.Fatalf("failed copy did not retain challenge: %v", err)
	}
}

func TestMCPHostWriteChallengeDoesNotReplaceChangedPlaceholder(t *testing.T) {
	directory := resolvedTempDir(t)
	destination := filepath.Join(directory, "result.bin")
	endpoint := newHostCapabilityTestEndpoint()
	_, challenge, err := endpoint.createHostWriteChallenge(context.Background(), nil, mcpHostWriteChallengeInput{Path: destination})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(challenge.Nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := validateHostWritePlaceholder(endpoint.hostChallenges[challenge.ChallengeID])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("new owner data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := revalidateHostWritePlaceholder(endpoint.hostChallenges[challenge.ChallengeID], info); err == nil {
		t.Fatal("changed placeholder remained eligible for replacement")
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "new owner data" {
		t.Fatalf("changed placeholder was damaged: %q, %v", got, err)
	}
}
