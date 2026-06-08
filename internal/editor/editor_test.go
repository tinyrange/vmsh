package editor

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

type fakeCompleter struct {
	items      []string
	replaceLen int
	kind       CompletionKind
}

func (f fakeCompleter) CompleteWithKind([]rune, int) ([]string, int, CompletionKind) {
	return f.items, f.replaceLen, f.kind
}

func TestLineEditorReadsBasicLine(t *testing.T) {
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	defer master.Close()
	defer tty.Close()

	editor := NewLineEditor(tty, io.Discard, "", nil)
	done := readLineAsync(editor)
	time.Sleep(25 * time.Millisecond)
	if _, err := master.Write([]byte("echo hi\r")); err != nil {
		t.Fatal(err)
	}
	assertLine(t, done, "echo hi")
}

func TestLineEditorInsertsTabOnEmptyInput(t *testing.T) {
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	defer master.Close()
	defer tty.Close()

	editor := NewLineEditor(tty, io.Discard, "", fakeCompleter{items: []string{"alpha"}, kind: CompletionPath})
	done := readLineAsync(editor)
	time.Sleep(25 * time.Millisecond)
	if _, err := master.Write([]byte("\t\r")); err != nil {
		t.Fatal(err)
	}
	assertLine(t, done, "\t")
}

func TestLineEditorInsertsTabAfterOnlyTabs(t *testing.T) {
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	defer master.Close()
	defer tty.Close()

	editor := NewLineEditor(tty, io.Discard, "", fakeCompleter{items: []string{"alpha"}, kind: CompletionPath})
	done := readLineAsync(editor)
	time.Sleep(25 * time.Millisecond)
	if _, err := master.Write([]byte("\t\t\r")); err != nil {
		t.Fatal(err)
	}
	assertLine(t, done, "\t\t")
}

func TestLineEditorAcceptsCompletionSelection(t *testing.T) {
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	defer master.Close()
	defer tty.Close()

	editor := NewLineEditor(tty, io.Discard, "", fakeCompleter{items: []string{"aa", "beta"}, kind: CompletionPath})
	done := readLineAsync(editor)
	time.Sleep(25 * time.Millisecond)
	if _, err := master.Write([]byte("cat \t\r\r")); err != nil {
		t.Fatal(err)
	}
	assertLine(t, done, "cat aa")
}

func TestLineEditorInsertsCompletionSuffix(t *testing.T) {
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	defer master.Close()
	defer tty.Close()

	editor := NewLineEditor(tty, io.Discard, "", fakeCompleter{items: []string{"ho"}, replaceLen: 2, kind: CompletionCommand})
	done := readLineAsync(editor)
	time.Sleep(25 * time.Millisecond)
	if _, err := master.Write([]byte("ec\t\r\r")); err != nil {
		t.Fatal(err)
	}
	assertLine(t, done, "echo")
}

func TestLineEditorAsksBeforeHugeCommandCompletion(t *testing.T) {
	editor := &LineEditor{width: 183}
	items := make([]string, commandCompletionAskLimit+1)
	for i := range items {
		items[i] = fmt.Sprintf("p%d", i)
	}
	editor.buf = []rune("p")
	editor.cursor = 1
	editor.confirm = completionConfirm{
		active:     true,
		items:      items,
		replaceLen: 1,
		token:      "p",
	}
	var out bytes.Buffer
	editor.out = &out
	editor.renderCompletionConfirm()
	if got := out.String(); !strings.Contains(got, "do you wish to see all 101 possibilities") {
		t.Fatalf("confirmation output = %q", got)
	}
	editor.handleCompletionConfirm(keyEvent{key: keyRune, r: 'y'})
	if editor.confirm.active || !editor.menu.active || len(editor.menu.items) != len(items) {
		t.Fatalf("confirm yes = confirm %v menu %v items %d", editor.confirm.active, editor.menu.active, len(editor.menu.items))
	}
}

func TestLineEditorLeftRightNavigateCompletionMenu(t *testing.T) {
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	defer master.Close()
	defer tty.Close()

	editor := NewLineEditor(tty, io.Discard, "", fakeCompleter{items: []string{"aa", "bb", "cc"}, kind: CompletionPath})
	done := readLineAsync(editor)
	time.Sleep(25 * time.Millisecond)
	if _, err := master.Write([]byte("cat \t\x1b[C\r\r")); err != nil {
		t.Fatal(err)
	}
	assertLine(t, done, "cat bb")
}

type lineResult struct {
	line string
	err  error
}

func readLineAsync(editor *LineEditor) <-chan lineResult {
	done := make(chan lineResult, 1)
	go func() {
		line, err := editor.ReadLine("vmsh> ")
		done <- lineResult{line: line, err: err}
	}()
	return done
}

func assertLine(t *testing.T, done <-chan lineResult, want string) {
	t.Helper()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("ReadLine() error = %v", got.err)
		}
		if got.line != want {
			t.Fatalf("ReadLine() = %q, want %q", got.line, want)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadLine() timed out")
	}
}
