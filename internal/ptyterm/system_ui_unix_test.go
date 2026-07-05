//go:build !windows

package ptyterm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/termui/asciicast"
)

func TestDriverSystemComplexPrograms(t *testing.T) {
	if os.Getenv("VMSH_PTYTERM_SYSTEM_UI") == "" {
		t.Skip("set VMSH_PTYTERM_SYSTEM_UI=1 to run local TUI driving tests")
	}
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		bin  string
		run  func(*testing.T, string, string)
	}{
		{name: "nvim", bin: "nvim", run: driveNvim},
		{name: "vim", bin: "vim", run: driveVim},
		{name: "nano", bin: "nano", run: driveNano},
		{name: "less", bin: "less", run: driveLess},
		{name: "man", bin: "man", run: driveMan},
		{name: "top", bin: "top", run: driveTop},
		{name: "btop", bin: "btop", run: driveBtop},
		{name: "watch", bin: "watch", run: driveWatch},
		{name: "tmux", bin: "tmux", run: driveTmux},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := exec.LookPath(tc.bin)
			if err != nil {
				t.Skipf("%s not found", tc.bin)
			}
			tc.run(t, path, root)
		})
	}
}

func driveNvim(t *testing.T, bin string, root string) {
	file := filepath.Join(root, "nvim.txt")
	runEditorSave(t, "nvim", []string{bin, "-Nu", "NONE", "-n", "--cmd", "set noswapfile", file}, file, "nvim drove this file")
}

func driveVim(t *testing.T, bin string, root string) {
	file := filepath.Join(root, "vim.txt")
	runEditorSave(t, "vim", []string{bin, "-Nu", "NONE", "-n", "-i", "NONE", file}, file, "vim drove this file")
}

func driveNano(t *testing.T, bin string, root string) {
	file := filepath.Join(root, "nano.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, driver, cast := startDrivenProgram(t, ctx, "nano", []string{bin, file}, root)
	defer cast.Close()
	defer s.Close()
	waitForBytes(t, ctx, driver)
	if err := driver.Send(Input("nano drove this file\n"), Ctrl('o'), KeyEnter.Input(), Ctrl('x')); err != nil {
		t.Fatalf("drive nano: %v", err)
	}
	expectExit(t, ctx, s, "nano")
	assertFile(t, file, "nano drove this file\n")
}

func driveLess(t *testing.T, bin string, root string) {
	file := filepath.Join(root, "less.txt")
	if err := os.WriteFile(file, []byte("alpha\nless-ready\nomega\n"), 0o600); err != nil {
		t.Fatalf("write less fixture: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, driver, cast := startDrivenProgram(t, ctx, "less", []string{bin, "-R", file}, root)
	defer cast.Close()
	defer s.Close()
	if _, err := driver.WaitLine(ctx, "less-ready"); err != nil {
		t.Fatalf("wait less: %v", err)
	}
	if err := driver.Key(Key("q")); err != nil {
		t.Fatalf("quit less: %v", err)
	}
	expectExit(t, ctx, s, "less")
}

func driveMan(t *testing.T, bin string, root string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, driver, cast := startDrivenProgram(t, ctx, "man", []string{bin, "ls"}, root)
	defer cast.Close()
	defer s.Close()
	waitForBytes(t, ctx, driver)
	if err := driver.Key(Key("q")); err != nil {
		t.Fatalf("quit man: %v", err)
	}
	if !waitExitWithin(ctx, driver, 500*time.Millisecond) {
		if err := driver.Send(Ctrl('c')); err != nil {
			t.Fatalf("interrupt man: %v", err)
		}
	}
	expectExited(t, ctx, s, "man")
}

func driveTop(t *testing.T, bin string, root string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, driver, cast := startDrivenProgram(t, ctx, "top", []string{bin}, root)
	defer cast.Close()
	defer s.Close()
	waitForBytes(t, ctx, driver)
	if err := driver.Key(Key("q")); err != nil {
		t.Fatalf("quit top: %v", err)
	}
	expectExit(t, ctx, s, "top")
}

func driveBtop(t *testing.T, bin string, root string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, driver, cast := startDrivenProgram(t, ctx, "btop", []string{bin, "--utf-force"}, root)
	defer cast.Close()
	defer s.Close()
	waitForBytes(t, ctx, driver)
	if s.Snapshot().Exited {
		return
	}
	if err := driver.Key(Key("q")); err != nil && !s.Snapshot().Exited {
		t.Fatalf("quit btop: %v", err)
	}
	expectExited(t, ctx, s, "btop")
}

func driveWatch(t *testing.T, bin string, root string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, driver, cast := startDrivenProgram(t, ctx, "watch", []string{bin, "-n", "1", "printf watch-ready"}, root)
	defer cast.Close()
	defer s.Close()
	if _, err := driver.WaitLine(ctx, "watch-ready"); err != nil {
		t.Fatalf("wait watch: %v", err)
	}
	if err := driver.Send(Ctrl('c')); err != nil {
		t.Fatalf("interrupt watch: %v", err)
	}
	expectExited(t, ctx, s, "watch")
}

func driveTmux(t *testing.T, bin string, root string) {
	socket := fmt.Sprintf("vmsh-ptyterm-%d", os.Getpid())
	_ = exec.Command(bin, "-L", socket, "kill-server").Run()
	t.Cleanup(func() { _ = exec.Command(bin, "-L", socket, "kill-server").Run() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, driver, cast := startDrivenProgram(t, ctx, "tmux", []string{bin, "-L", socket, "-f", "/dev/null", "new-session"}, root)
	defer cast.Close()
	defer s.Close()
	waitForBytes(t, ctx, driver)
	if err := driver.Send(Input("printf tmux-ready"), KeyEnter.Input()); err != nil {
		t.Fatalf("drive tmux command: %v", err)
	}
	if _, err := driver.WaitLine(ctx, "tmux-ready"); err != nil {
		t.Fatalf("wait tmux: %v", err)
	}
	if err := driver.Send(Input("exit"), KeyEnter.Input()); err != nil {
		t.Fatalf("exit tmux shell: %v", err)
	}
	expectExit(t, ctx, s, "tmux")
}

func runEditorSave(t *testing.T, name string, command []string, file string, content string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, driver, cast := startDrivenProgram(t, ctx, name, command, filepath.Dir(file))
	defer cast.Close()
	defer s.Close()
	waitForBytes(t, ctx, driver)
	if err := driver.Send(Input("i"), Input(content), KeyEscape.Input(), Input(":wq"), KeyEnter.Input()); err != nil {
		t.Fatalf("drive %s: %v", name, err)
	}
	expectExit(t, ctx, s, name)
	assertFile(t, file, content)
}

func startDrivenProgram(t *testing.T, ctx context.Context, name string, command []string, root string) (*Session, *Driver, *asciicast.Recorder) {
	t.Helper()
	castPath := filepath.Join(root, name+".cast")
	rec, err := NewAsciicast(castPath, Size{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("create %s asciicast: %v", name, err)
	}
	s, err := Start(ctx, Options{
		Command:      command,
		Size:         Size{Cols: 100, Rows: 30},
		HistoryLimit: 500,
		Env:          []string{"TERM=xterm-256color", "NO_COLOR=1"},
		Recorder:     rec,
	})
	if err != nil {
		_ = rec.Close()
		t.Fatalf("start %s: %v", name, err)
	}
	return s, NewDriver(s), rec
}

func waitForBytes(t *testing.T, ctx context.Context, driver *Driver) {
	t.Helper()
	if _, err := driver.Wait(ctx, func(snap Snapshot) bool { return snap.BytesRead > 0 }); err != nil {
		t.Fatalf("wait for output: %v", err)
	}
}

func expectExit(t *testing.T, ctx context.Context, s *Session, name string) {
	t.Helper()
	result := s.Wait(ctx)
	if result.Err != nil {
		t.Fatalf("%s result = %+v snapshot=%s", name, result, snapshotSummary(s.Snapshot()))
	}
}

func expectExited(t *testing.T, ctx context.Context, s *Session, name string) {
	t.Helper()
	_ = s.Wait(ctx)
	if !s.Snapshot().Exited {
		t.Fatalf("%s did not exit: %s", name, snapshotSummary(s.Snapshot()))
	}
}

func assertFile(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.TrimRight(string(data), "\n") != strings.TrimRight(want, "\n") {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func waitExitWithin(parent context.Context, driver *Driver, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	_, err := driver.WaitExit(ctx)
	return err == nil
}

func snapshotSummary(snap Snapshot) string {
	return fmt.Sprintf("exited=%v code=%d bytes=%d stdin=%d lines=%q history=%q", snap.Exited, snap.ExitCode, snap.BytesRead, snap.BytesStdin, snap.Lines, snap.History)
}
