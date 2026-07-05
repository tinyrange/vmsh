//go:build !windows

package ptyterm

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSessionOwnsPTYCapturesOutputAndExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{
		Command:      []string{"sh", "-lc", "printf 'hello'; printf '\\r\\nworld\\r\\n'"},
		Size:         Size{Cols: 20, Rows: 4},
		HistoryLimit: 10,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.Close()

	waitForLine(t, s, "world")
	result := s.Wait(ctx)
	if result.Err != nil {
		t.Fatalf("wait result = %+v", result)
	}
	snap := s.Snapshot()
	if !snap.Exited || snap.ExitCode != 0 {
		t.Fatalf("snapshot exit = exited:%v code:%d err:%q", snap.Exited, snap.ExitCode, snap.ExitError)
	}
	if !snapshotContainsLine(snap, "hello") || !snapshotContainsLine(snap, "world") {
		t.Fatalf("snapshot lines=%#v history=%#v", snap.Lines, snap.History)
	}
	if snap.BytesRead == 0 {
		t.Fatalf("snapshot did not record bytes read")
	}
}

func TestSessionRoutesStdinAndResize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{
		Command:      []string{"sh", "-lc", "stty size; IFS= read -r _; stty size"},
		Size:         Size{Cols: 22, Rows: 6},
		HistoryLimit: 10,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.Close()

	waitForLine(t, s, "6 22")
	if err := s.Resize(Size{Cols: 33, Rows: 7}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if n, err := s.Write([]byte("\n")); err != nil || n != 1 {
		t.Fatalf("write stdin n=%d err=%v", n, err)
	}
	waitForLine(t, s, "7 33")
	result := s.Wait(ctx)
	if result.Err != nil {
		t.Fatalf("wait result = %+v", result)
	}
	snap := s.Snapshot()
	if snap.LastResize == nil || *snap.LastResize != (Size{Cols: 33, Rows: 7}) || snap.ResizeCount != 1 {
		t.Fatalf("resize snapshot = last:%+v count:%d", snap.LastResize, snap.ResizeCount)
	}
	if snap.BytesStdin != 1 {
		t.Fatalf("stdin bytes = %d, want 1", snap.BytesStdin)
	}
}

func TestSessionExitCodeForFailingCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{Command: []string{"sh", "-lc", "exit 17"}, Size: Size{Cols: 10, Rows: 2}})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.Close()

	result := s.Wait(ctx)
	if result.ExitCode != 17 || result.Err == nil {
		t.Fatalf("result = %+v, want exit 17", result)
	}
}

func TestSessionWaitAndSnapshotAreRepeatableAfterExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{Command: []string{"sh", "-lc", "printf done"}, Size: Size{Cols: 10, Rows: 2}})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.Close()

	first := s.Wait(ctx)
	second := s.Wait(ctx)
	if first.ExitCode != 0 || first.Err != nil || second.ExitCode != 0 || second.Err != nil {
		t.Fatalf("wait results = first:%+v second:%+v", first, second)
	}
	firstSnap := s.Snapshot()
	secondSnap := s.Snapshot()
	if !firstSnap.Exited || !secondSnap.Exited || firstSnap.ExitCode != 0 || secondSnap.ExitCode != 0 {
		t.Fatalf("snapshot exits = first:%+v second:%+v", firstSnap, secondSnap)
	}
}

func TestSessionWaitDrainsFinalTerminalTeardown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 20; i++ {
		s, err := Start(ctx, Options{
			Command: []string{"sh", "-lc", "printf main; printf '\\033[?1049hALT\\033[?1049lRESTORE'"},
			Size:    Size{Cols: 20, Rows: 4},
		})
		if err != nil {
			t.Fatalf("start session: %v", err)
		}
		result := s.Wait(ctx)
		if result.Err != nil {
			_ = s.Close()
			t.Fatalf("wait result = %+v", result)
		}
		snap := s.Snapshot()
		_ = s.Close()
		if snap.AltScreen {
			t.Fatalf("iteration %d still on alternate screen: %+v", i, snap)
		}
		if !snap.Exited || !snapshotContainsLine(snap, "RESTORE") {
			t.Fatalf("iteration %d snapshot did not include final teardown output: %+v", i, snap)
		}
		if snapshotContainsLine(snap, "ALT") {
			t.Fatalf("iteration %d snapshot kept alternate-screen content: %+v", i, snap)
		}
	}
}

func TestSessionCanExerciseComplexTerminalProgramsWhenRequested(t *testing.T) {
	if os.Getenv("VMSH_PTYTERM_COMPLEX") == "" {
		t.Skip("set VMSH_PTYTERM_COMPLEX=1 to run optional vim/tmux PTY smoke tests")
	}
	for _, tc := range []struct {
		name string
		bin  string
		args []string
	}{
		{
			name: "vim",
			bin:  "vim",
			args: []string{"-Nu", "NONE", "-n", "-T", "xterm", "-c", "set nomore", "-c", "redraw", "-c", "qa!"},
		},
		{
			name: "tmux",
			bin:  "tmux",
			args: []string{"-L", "vmsh-ptyterm-test", "new-session", "-d", "printf nested-tmux; sleep 0.1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := exec.LookPath(tc.bin)
			if err != nil {
				t.Skipf("%s not found", tc.bin)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := append([]string{path}, tc.args...)
			s, err := Start(ctx, Options{Command: cmd, Size: Size{Cols: 80, Rows: 24}, HistoryLimit: 200})
			if err != nil {
				t.Fatalf("start %s: %v", tc.name, err)
			}
			defer s.Close()
			result := s.Wait(ctx)
			if result.Err != nil {
				t.Fatalf("%s result = %+v snapshot=%+v", tc.name, result, s.Snapshot())
			}
		})
	}
}

func waitForLine(t *testing.T, s *Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snapshotContainsLine(s.Snapshot(), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap := s.Snapshot()
	t.Fatalf("line %q not found; lines=%#v history=%#v raw=%q", needle, snap.Lines, snap.History, snap.RawTail)
}

func snapshotContainsLine(snap Snapshot, needle string) bool {
	for _, line := range append(append([]string{}, snap.History...), snap.Lines...) {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
