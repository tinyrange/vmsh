//go:build !windows

package ptyterm

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

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
