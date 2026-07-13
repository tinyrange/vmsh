package shell

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestSSHKnownHostsConcurrentWriterHelper(t *testing.T) {
	path := os.Getenv("VMSH_TEST_KNOWN_HOSTS_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(os.Getenv("VMSH_TEST_KNOWN_HOSTS_KEY")))
	if err != nil {
		t.Fatalf("parse helper host key: %v", err)
	}
	hostname := os.Getenv("VMSH_TEST_KNOWN_HOSTS_NAME")
	if err := addSSHKnownHostFile(path, hostname, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, key); err != nil {
		t.Fatalf("add helper host key: %v", err)
	}
}

func TestSSHKnownHostsSerializesConcurrentProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	key := newTestSSHHostPublicKey(t)
	encodedKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))

	const duplicateWriters = 12
	const uniqueWriters = 12
	type runningCommand struct {
		cmd    *exec.Cmd
		output *bytes.Buffer
	}
	commands := make([]runningCommand, 0, duplicateWriters+uniqueWriters)
	start := func(hostname string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestSSHKnownHostsConcurrentWriterHelper$")
		cmd.Env = append(os.Environ(),
			"VMSH_TEST_KNOWN_HOSTS_PATH="+path,
			"VMSH_TEST_KNOWN_HOSTS_NAME="+hostname,
			"VMSH_TEST_KNOWN_HOSTS_KEY="+encodedKey,
		)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Start(); err != nil {
			t.Fatalf("start known_hosts writer: %v", err)
		}
		commands = append(commands, runningCommand{cmd: cmd, output: &output})
	}
	for range duplicateWriters {
		start("shared.example:22")
	}
	for i := range uniqueWriters {
		start(fmt.Sprintf("host-%d.example:22", i))
	}
	for _, command := range commands {
		if err := command.cmd.Wait(); err != nil {
			t.Errorf("known_hosts writer: %v\n%s", err, command.output.String())
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if want := 1 + uniqueWriters; len(lines) != want {
		t.Fatalf("known_hosts records = %d, want %d", len(lines), want)
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		t.Fatalf("parse known_hosts: %v", err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	for _, hostname := range append([]string{"shared.example:22"}, testSSHKnownHostsNames(uniqueWriters)...) {
		if err := callback(hostname, remote, key); err != nil {
			t.Errorf("verify %s: %v", hostname, err)
		}
	}
}

func TestSSHKnownHostsRejectsChangedKeyUnderLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	hostname := "changed.example:22"
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := addSSHKnownHostFile(path, hostname, remote, newTestSSHHostPublicKey(t)); err != nil {
		t.Fatalf("add initial host key: %v", err)
	}

	err := addSSHKnownHostFile(path, hostname, remote, newTestSSHHostPublicKey(t))
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) || len(keyErr.Want) == 0 {
		t.Fatalf("changed key error = %v, want knownhosts.KeyError with existing keys", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read known_hosts: %v", readErr)
	}
	if got := len(bytes.Split(bytes.TrimSpace(data), []byte("\n"))); got != 1 {
		t.Fatalf("known_hosts records after changed key = %d, want 1", got)
	}
}

func TestSSHKnownHostsPreservesExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatalf("create known_hosts: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("set known_hosts mode: %v", err)
	}
	if err := addSSHKnownHostFile(path, "mode.example:22", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, newTestSSHHostPublicKey(t)); err != nil {
		t.Fatalf("add host key: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat known_hosts: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("known_hosts mode = %o, want 640", got)
	}
}

func newTestSSHHostPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("create SSH host key: %v", err)
	}
	return key
}

func testSSHKnownHostsNames(count int) []string {
	names := make([]string, count)
	for i := range count {
		names[i] = fmt.Sprintf("host-%d.example:22", i)
	}
	return names
}
