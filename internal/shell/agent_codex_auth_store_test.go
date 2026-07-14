package shell

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCodexAuthSaveFailurePreservesPreviousFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	previous := []byte("{\"auth_mode\":\"api-key\",\"OPENAI_API_KEY\":\"old\"}\n")
	if err := os.WriteFile(path, previous, 0o640); err != nil {
		t.Fatalf("write previous auth: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("set previous auth mode: %v", err)
	}

	renameErr := errors.New("injected rename failure")
	err := writeCodexAuthFile(path, []byte("{\"auth_mode\":\"api-key\",\"OPENAI_API_KEY\":\"new\"}\n"), func(string, string) error {
		return renameErr
	})
	if !errors.Is(err, renameErr) {
		t.Fatalf("writeCodexAuthFile error = %v, want injected failure", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read preserved auth: %v", err)
	}
	if string(got) != string(previous) {
		t.Fatalf("preserved auth = %q, want original bytes", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat preserved auth: %v", err)
	}
	if gotMode := info.Mode().Perm(); runtime.GOOS != "windows" && gotMode != 0o640 {
		t.Fatalf("preserved auth mode = %o, want 640", gotMode)
	}
	assertOnlyCodexAuthFile(t, dir)
}

func TestCodexAuthSaveRejectsInvalidJSONBeforeActivation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	previous := []byte("{\"auth_mode\":\"api-key\",\"OPENAI_API_KEY\":\"old\"}\n")
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatalf("write previous auth: %v", err)
	}

	if err := writeCodexAuthFile(path, []byte("{"), os.Rename); err == nil {
		t.Fatal("writeCodexAuthFile accepted invalid JSON")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read preserved auth: %v", err)
	}
	if string(got) != string(previous) {
		t.Fatalf("preserved auth = %q, want original bytes", got)
	}
	assertOnlyCodexAuthFile(t, dir)
}

func TestCodexAuthStoreSerializesConcurrentLoadsAndSaves(t *testing.T) {
	dir := t.TempDir()
	store := &codexAgentProxyAuthStore{
		path: filepath.Join(dir, "auth.json"),
		now:  time.Now,
	}
	if err := store.save(codexAgentProxyAuthFile{AuthMode: "api-key", OpenAIAPIKey: "key-0"}); err != nil {
		t.Fatalf("save initial auth: %v", err)
	}

	const workers = 8
	const iterations = 25
	errCh := make(chan error, workers*iterations*2)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(2)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				key := fmt.Sprintf("key-%d-%d", worker, iteration)
				if err := store.save(codexAgentProxyAuthFile{AuthMode: "api-key", OpenAIAPIKey: key}); err != nil {
					errCh <- err
				}
			}
		}()
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				auth, err := store.load()
				if err != nil {
					errCh <- err
					continue
				}
				if auth.AuthMode != "api-key" || auth.OpenAIAPIKey == "" {
					errCh <- fmt.Errorf("invalid loaded auth state: mode=%q key_empty=%t", auth.AuthMode, auth.OpenAIAPIKey == "")
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent auth operation: %v", err)
	}
}

func assertOnlyCodexAuthFile(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read auth directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("auth directory entries = %v, want only auth.json", entries)
	}
}
