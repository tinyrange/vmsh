package ptyterm

import (
	"context"
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSessionOwnsPTYCapturesOutputAndExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{
		Command:      shellOutputCommand(),
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
		Command:      shellReadResizeCommand(),
		Size:         Size{Cols: 22, Rows: 6},
		HistoryLimit: 10,
		Env:          []string{ptyTermHelperEnv},
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.Close()

	waitForLine(t, s, "6 22")
	if err := s.Resize(Size{Cols: 33, Rows: 7}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if runtime.GOOS == "windows" {
		waitForRawTail(t, s, "\x1b[8;7;33t")
	}
	if n, err := s.Write(enterInput()); err != nil || n != len(enterInput()) {
		t.Fatalf("write stdin n=%d err=%v", n, err)
	}
	if runtime.GOOS != "windows" {
		waitForLine(t, s, "7 33")
	}
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

func TestSessionResizeValidatesDimensionsBeforePTYConversion(t *testing.T) {
	var resized Size
	s := &Session{
		pty: &stubPTY{},
		emu: NewEmulator(Size{Cols: 80, Rows: 24}, 0),
		resizePTY: func(size Size) error {
			resized = size
			return nil
		},
	}

	for _, size := range []Size{
		{Cols: -1, Rows: 24},
		{Cols: 80, Rows: -1},
		{Cols: MaxTerminalDimension + 1, Rows: 24},
		{Cols: 80, Rows: MaxTerminalDimension + 1},
	} {
		if err := s.Resize(size); err == nil {
			t.Fatalf("resize %+v returned no error", size)
		}
		if resized != (Size{}) {
			t.Fatalf("invalid resize %+v reached PTY as %+v", size, resized)
		}
	}

	if err := s.Resize(Size{}); err != nil {
		t.Fatalf("resize with defaults: %v", err)
	}
	if resized != (Size{Cols: 80, Rows: 24}) {
		t.Fatalf("zero resize reached PTY as %+v, want defaults", resized)
	}
}

type stubPTY struct{}

func (*stubPTY) Read([]byte) (int, error)    { return 0, io.EOF }
func (*stubPTY) Write(p []byte) (int, error) { return len(p), nil }
func (*stubPTY) Close() error                { return nil }

func TestSessionExitCodeForFailingCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{Command: shellExitCommand(17), Size: Size{Cols: 10, Rows: 2}})
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
	s, err := Start(ctx, Options{Command: shellPrintCommand("done"), Size: Size{Cols: 10, Rows: 2}})
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
	for i := 0; i < finalDrainIterations(); i++ {
		ctx, cancel := context.WithTimeout(context.Background(), finalDrainTimeout())
		s, err := Start(ctx, Options{
			Command: shellAltScreenTeardownCommand(),
			Size:    Size{Cols: 20, Rows: 4},
		})
		if err != nil {
			cancel()
			t.Fatalf("start session: %v", err)
		}
		result := s.Wait(ctx)
		cancel()
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

func finalDrainIterations() int {
	if runtime.GOOS == "windows" {
		return 3
	}
	return 20
}

func finalDrainTimeout() time.Duration {
	if runtime.GOOS == "windows" {
		return 15 * time.Second
	}
	return 5 * time.Second
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

func waitForRawTail(t *testing.T, s *Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(string(s.Snapshot().RawTail), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap := s.Snapshot()
	t.Fatalf("raw tail %q not found; lines=%#v history=%#v raw=%q", needle, snap.Lines, snap.History, snap.RawTail)
}

func enterInput() []byte {
	if runtime.GOOS == "windows" {
		return []byte("\r")
	}
	return []byte("\n")
}

func shellOutputCommand() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/d", "/q", "/c", "echo hello& echo world"}
	}
	return []string{"sh", "-lc", "printf 'hello'; printf '\\r\\nworld\\r\\n'"}
}

func shellReadResizeCommand() []string {
	if runtime.GOOS == "windows" {
		return ptyTermTestHelperCommand("size")
	}
	return []string{"sh", "-lc", "stty size; IFS= read -r _; stty size"}
}

func shellExitCommand(code int) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/d", "/q", "/c", "exit /b " + strconv.Itoa(code)}
	}
	return []string{"sh", "-lc", "exit " + strconv.Itoa(code)}
}

func shellPrintCommand(text string) []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[Console]::Out.Write(" + strconv.Quote(text) + "); [Console]::Out.Flush()"}
	}
	return []string{"sh", "-lc", "printf " + shellQuote(text)}
}

func shellAltScreenTeardownCommand() []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "$e=[char]27; [Console]::Out.Write('main' + $e + '[?1049hALT' + $e + '[?1049lRESTORE'); [Console]::Out.Flush()"}
	}
	return []string{"sh", "-lc", "printf main; printf '\\033[?1049hALT\\033[?1049lRESTORE'"}
}

func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "'\\''") + "'"
}

func snapshotContainsLine(snap Snapshot, needle string) bool {
	for _, line := range append(append([]string{}, snap.History...), snap.Lines...) {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
