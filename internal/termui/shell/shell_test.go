package shell_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyrange/vmsh/internal/termui/editor"
	"github.com/tinyrange/vmsh/internal/termui/shell"
	"github.com/tinyrange/vmsh/internal/termui/testkit"
)

func TestShellTranscript(t *testing.T) {
	got := testkit.RunShell(t, "help\nexit\n")
	if strings.TrimSpace(got.Stdout) != "builtins: cd pwd sleep spin progress failwait timeout help exit" {
		t.Fatalf("stdout = %q", got.Stdout)
	}
	if got.Stderr != "" {
		t.Fatalf("stderr = %q, want empty noninteractive transcript", got.Stderr)
	}
}

func TestCommandCompleterCompletesBuiltins(t *testing.T) {
	items, replaceLen, kind := shell.CommandCompleter{}.Complete([]rune("pro"), 3)
	if kind != editor.CompletionCommand {
		t.Fatalf("kind = %q, want command", kind)
	}
	if replaceLen != 3 {
		t.Fatalf("replaceLen = %d, want 3", replaceLen)
	}
	if !contains(items, "progress") {
		t.Fatalf("items = %#v, want progress", items)
	}
}

func TestCommandCompleterEmptyTokenIncludesPathCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zz-termui-test-command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	items, replaceLen, kind := shell.CommandCompleter{}.Complete(nil, 0)
	if kind != editor.CompletionCommand {
		t.Fatalf("kind = %q, want command", kind)
	}
	if replaceLen != 0 {
		t.Fatalf("replaceLen = %d, want 0", replaceLen)
	}
	if !contains(items, "zz-termui-test-command") {
		t.Fatalf("items = %#v, want PATH command", items)
	}
}

func TestCommandCompleterCompletesPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "al")
	line := []rune("cat " + token)
	items, replaceLen, kind := shell.CommandCompleter{}.Complete(line, len(line))
	if kind != editor.CompletionPath {
		t.Fatalf("kind = %q, want path", kind)
	}
	if replaceLen != len([]rune(token)) {
		t.Fatalf("replaceLen = %d, want %d", replaceLen, len([]rune(token)))
	}
	if !contains(items, filepath.Join(dir, "alpha.txt")) {
		t.Fatalf("items = %#v, want alpha.txt path", items)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

type metadataRecorder struct {
	names []string
}

func (r *metadataRecorder) Metadata(name string, fields map[string]any) {
	r.names = append(r.names, name)
}

func TestShellRecordsSlowInteractionMetadata(t *testing.T) {
	h := testkit.NewHarness("spin 80ms\nexit\n")
	rec := &metadataRecorder{}
	h.Shell.Recorder = rec
	if err := h.Shell.Loop(context.Background()); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if len(rec.names) != 1 || rec.names[0] != "termui.slow_interaction" {
		t.Fatalf("metadata names = %#v", rec.names)
	}
}

func TestFailWaitReportsDurableFailure(t *testing.T) {
	h := testkit.NewHarness("failwait 80ms\nexit\n")
	if err := h.Shell.Loop(context.Background()); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if !strings.Contains(h.Err.String(), "check failed: demo failure after partial work") {
		t.Fatalf("stderr = %q", h.Err.String())
	}
}

func TestShellExternalRunnerCanBeStubbed(t *testing.T) {
	h := testkit.NewHarness("echo hello\nexit\n")
	var called []string
	h.Shell.External = func(_ context.Context, fields []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
		called = append(called, strings.Join(fields, " "))
		return nil
	}
	if err := h.Shell.Loop(context.Background()); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if len(called) != 1 || called[0] != "echo hello" {
		t.Fatalf("called = %#v", called)
	}
}
