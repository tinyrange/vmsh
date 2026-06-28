package shell

import (
	"io"
	"os"
	"runtime"
	"testing"
)

func TestColorizeExternalLeavesNonTerminalPlain(t *testing.T) {
	got := colorizeExternal([]string{"ls", "-la"}, io.Discard)
	if len(got) != 2 || got[0] != "ls" || got[1] != "-la" {
		t.Fatalf("fields = %#v", got)
	}
}

func TestColorizeExternalForTerminalLs(t *testing.T) {
	if !isTerminalWriter(os.Stdout) {
		t.Skip("stdout is not a terminal")
	}
	got := colorizeExternal([]string{"ls", "-la"}, os.Stdout)
	switch runtime.GOOS {
	case "darwin":
		if len(got) < 3 || got[1] != "-G" {
			t.Fatalf("fields = %#v, want -G injected", got)
		}
	case "linux":
		if len(got) < 3 || got[1] != "--color=auto" {
			t.Fatalf("fields = %#v, want --color=auto injected", got)
		}
	default:
		if len(got) != 2 {
			t.Fatalf("fields = %#v, want unchanged on %s", got, runtime.GOOS)
		}
	}
}

func TestColorizeExternalDoesNotDuplicateColorFlag(t *testing.T) {
	if !isTerminalWriter(os.Stdout) {
		t.Skip("stdout is not a terminal")
	}
	darwinGot := colorizeExternal([]string{"ls", "-G"}, os.Stdout)
	linuxGot := colorizeExternal([]string{"ls", "--color=never"}, os.Stdout)
	if runtime.GOOS == "darwin" && len(darwinGot) != 2 {
		t.Fatalf("darwin fields = %#v, want unchanged", darwinGot)
	}
	if runtime.GOOS == "linux" && len(linuxGot) != 2 {
		t.Fatalf("linux fields = %#v, want unchanged", linuxGot)
	}
}
