package editor

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLineEditorTabKeepsOpenCompletionSelectionStable(t *testing.T) {
	e := &LineEditor{}
	e.openCompletionMenu([]string{"alpha", "beta"}, 1, "a")
	e.menu.selected = 1

	e.handleTab()

	if e.menu.selected != 1 {
		t.Fatalf("selected completion = %d, want 1", e.menu.selected)
	}
	if !e.menu.active {
		t.Fatalf("completion menu was closed")
	}
}

func TestRefreshMovesToTopOfWrappedInputBeforeClearingOwnedRows(t *testing.T) {
	var out bytes.Buffer
	e := &LineEditor{
		out:    &out,
		width:  10,
		prompt: "> ",
		buf:    []rune("abcdefghijklm"),
		cursor: 13,
	}

	e.refresh()
	out.Reset()
	e.insertRune('n')
	e.refresh()

	got := out.String()
	if strings.Contains(got, "\x1b[J") {
		t.Fatalf("refresh used clear-to-end-of-screen: %q", got)
	}
	wantPrefix := "\r\x1b[1A\x1b[2K\x1b[1B\r\x1b[2K\x1b[1A\r"
	if !strings.HasPrefix(got, wantPrefix) {
		prefix := got
		if len(prefix) > len(wantPrefix)+10 {
			prefix = prefix[:len(wantPrefix)+10]
		}
		t.Fatalf("second refresh prefix = %q, want row-local clears", prefix)
	}
}

func TestAppendRuneAtEndEchoesWithoutRefresh(t *testing.T) {
	var out bytes.Buffer
	e := &LineEditor{
		out:    &out,
		width:  10,
		prompt: "> ",
		buf:    []rune("abcdefghi"),
		cursor: 9,
	}
	e.updateRenderedRows()
	out.Reset()

	if !e.appendRuneAndEcho('j') {
		t.Fatalf("appendRuneAndEcho returned false")
	}

	if got := out.String(); got != "j" {
		t.Fatalf("append output = %q, want only inserted rune", got)
	}
	if e.renderedCursorRow != 1 || e.renderedLastRow != 1 {
		t.Fatalf("rendered rows = cursor %d last %d, want 1/1", e.renderedCursorRow, e.renderedLastRow)
	}
}

func TestLongAppendAtEndDoesNotClearWrappedRows(t *testing.T) {
	var out bytes.Buffer
	e := &LineEditor{
		out:    &out,
		width:  10,
		prompt: "> ",
	}
	e.updateRenderedRows()

	for _, r := range "abcdefghijklmnopqrstuvwxyz" {
		if !e.appendRuneAndEcho(r) {
			t.Fatalf("appendRuneAndEcho(%q) returned false", r)
		}
	}

	got := out.String()
	if got != "abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("append output = %q, want direct typed bytes", got)
	}
	for _, seq := range []string{"\x1b[2K", "\x1b[1A", "\x1b[1B"} {
		if strings.Contains(got, seq) {
			t.Fatalf("append output contained cursor/clear sequence %q: %q", seq, got)
		}
	}
	if e.renderedCursorRow != 2 || e.renderedLastRow != 2 {
		t.Fatalf("rendered rows = cursor %d last %d, want 2/2", e.renderedCursorRow, e.renderedLastRow)
	}
}

func TestAppendRuneInMiddleRequiresRefresh(t *testing.T) {
	var out bytes.Buffer
	e := &LineEditor{
		out:    &out,
		width:  80,
		prompt: "> ",
		buf:    []rune("abc"),
		cursor: 1,
	}
	if e.appendRuneAndEcho('x') {
		t.Fatalf("mid-line append unexpectedly used direct echo path")
	}
	if out.Len() != 0 {
		t.Fatalf("mid-line append wrote %q", out.String())
	}
}

func TestVisibleWidthIgnoresEscapeSequences(t *testing.T) {
	got := visibleWidth("\x1b[32m➜\x1b[0m demo \x1b]0;title\x07x")
	if got != 8 {
		t.Fatalf("visible width = %d, want 8", got)
	}
}

func TestTerminalRowForCellsDoesNotAdvanceAtExactWidth(t *testing.T) {
	tests := []struct {
		cells int
		want  int
	}{
		{cells: 0, want: 0},
		{cells: 1, want: 0},
		{cells: 9, want: 0},
		{cells: 10, want: 0},
		{cells: 11, want: 1},
		{cells: 20, want: 1},
		{cells: 21, want: 2},
	}
	for _, tt := range tests {
		if got := terminalRowForCells(tt.cells, 10); got != tt.want {
			t.Fatalf("terminalRowForCells(%d, 10) = %d, want %d", tt.cells, got, tt.want)
		}
	}
}

func TestResetRenderedRowsClearsWrappedLineState(t *testing.T) {
	e := &LineEditor{renderedCursorRow: 2, renderedLastRow: 3}
	e.resetRenderedRows()
	if e.renderedCursorRow != 0 || e.renderedLastRow != 0 {
		t.Fatalf("rendered rows = cursor %d last %d, want both zero", e.renderedCursorRow, e.renderedLastRow)
	}
}

func TestLineHistoryDoesNotSaveMultiLinePastes(t *testing.T) {
	h := &lineHistory{limit: 10}

	h.add("@host echo one\n@host echo two")

	if len(h.items) != 0 {
		t.Fatalf("history items = %#v, want no multi-line paste entry", h.items)
	}
}

func TestLineHistorySavesSingleLineCommands(t *testing.T) {
	h := &lineHistory{limit: 10}

	h.add("@host echo one")

	if got, want := h.items, []string{"@host echo one"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("history items = %#v, want %#v", got, want)
	}
}

func TestReadPasteBurstAfterEnterDoesNotBlockOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows console reads cannot be timed out by polling for EAGAIN")
	}
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	e := &LineEditor{in: in}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if text, ok := e.readPasteBurstAfterEnter(); ok || text != "" {
			t.Errorf("paste burst = %q, %v; want empty false", text, ok)
		}
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		_ = out.Close()
		t.Fatal("readPasteBurstAfterEnter blocked")
	}
}

func TestReadLinePreparedPreservesBufferedPromptInputOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows raw-mode prompt handoff regression")
	}
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	var terminal bytes.Buffer
	e := NewLineEditor(in, &terminal, "", nil)

	input := "ls\n@freebsd\nass\n"
	if _, err := out.Write([]byte(input)); err != nil {
		t.Fatalf("write editor input: %v", err)
	}

	for _, want := range []string{"ls", "@freebsd", "ass"} {
		got, err := e.ReadLinePrepared("> ")
		if err != nil {
			t.Fatalf("ReadLinePrepared(%q): %v", want, err)
		}
		if got != want {
			t.Fatalf("ReadLinePrepared returned %q, want %q; terminal output:\n%q", got, want, terminal.String())
		}
	}
}

func TestHistorySearchFindsNewestMatchingCommand(t *testing.T) {
	e := &LineEditor{
		out:     ioDiscard{},
		width:   80,
		history: &lineHistory{items: []string{"@ssh old uptime", "@copy src dst", "@ssh new uptime"}},
		buf:     []rune("draft"),
		cursor:  5,
	}

	e.startHistorySearch()
	for _, r := range "@ssh" {
		if _, _, err := e.handleHistorySearchEvent(keyEvent{key: keyRune, r: r}); err != nil {
			t.Fatalf("type search rune: %v", err)
		}
	}

	if got := string(e.buf); got != "@ssh new uptime" {
		t.Fatalf("search buffer = %q, want newest matching ssh command", got)
	}
	if got := string(e.search.query); got != "@ssh" {
		t.Fatalf("search query = %q, want @ssh", got)
	}
}

func TestHistorySearchCtrlRCyclesOlderMatches(t *testing.T) {
	e := &LineEditor{
		out:     ioDiscard{},
		width:   80,
		history: &lineHistory{items: []string{"@ssh old uptime", "@copy src dst", "@ssh new uptime"}},
	}

	e.startHistorySearch()
	for _, r := range "@ssh" {
		_, _, _ = e.handleHistorySearchEvent(keyEvent{key: keyRune, r: r})
	}
	if got := string(e.buf); got != "@ssh new uptime" {
		t.Fatalf("first match = %q", got)
	}
	_, _, _ = e.handleHistorySearchEvent(keyEvent{key: keyCtrlR})
	if got := string(e.buf); got != "@ssh old uptime" {
		t.Fatalf("cycled match = %q, want older ssh command", got)
	}
	_, _, _ = e.handleHistorySearchEvent(keyEvent{key: keyCtrlR})
	if got := string(e.buf); got != "@ssh new uptime" {
		t.Fatalf("wrapped match = %q, want newest ssh command", got)
	}
}

func TestHistorySearchEnterAcceptsSelectedCommand(t *testing.T) {
	e := &LineEditor{
		out:     ioDiscard{},
		width:   80,
		history: &lineHistory{items: []string{"@host echo one", "@copy src dst"}},
	}

	e.startHistorySearch()
	for _, r := range "copy" {
		_, _, _ = e.handleHistorySearchEvent(keyEvent{key: keyRune, r: r})
	}
	line, accepted, err := e.handleHistorySearchEvent(keyEvent{key: keyEnter})
	if err != nil {
		t.Fatalf("accept search: %v", err)
	}
	if !accepted || line != "@copy src dst" {
		t.Fatalf("accepted = %t line = %q, want @copy src dst", accepted, line)
	}
}

func TestHistorySearchCancelRestoresOriginalLine(t *testing.T) {
	for _, key := range []editorKey{keyEscape, keyCtrlG} {
		e := &LineEditor{
			out:     ioDiscard{},
			width:   80,
			history: &lineHistory{items: []string{"@host echo one"}},
			buf:     []rune("draft command"),
			cursor:  5,
		}
		e.startHistorySearch()
		_, _, _ = e.handleHistorySearchEvent(keyEvent{key: keyRune, r: 'e'})

		if _, _, err := e.handleHistorySearchEvent(keyEvent{key: key}); err != nil {
			t.Fatalf("cancel search with %v: %v", key, err)
		}
		if e.search.active {
			t.Fatalf("search remained active after cancel with %v", key)
		}
		if got := string(e.buf); got != "draft command" || e.cursor != 5 {
			t.Fatalf("restored line = %q cursor=%d, want draft command cursor=5", got, e.cursor)
		}
	}
}

func TestHistorySearchFailedMatchKeepsOriginalLine(t *testing.T) {
	e := &LineEditor{
		out:     ioDiscard{},
		width:   80,
		history: &lineHistory{items: []string{"@host echo one"}},
		buf:     []rune("draft"),
		cursor:  2,
	}

	e.startHistorySearch()
	for _, r := range "missing" {
		_, _, _ = e.handleHistorySearchEvent(keyEvent{key: keyRune, r: r})
	}

	if e.search.matchPos != -1 {
		t.Fatalf("matchPos = %d, want failed match", e.search.matchPos)
	}
	if got := string(e.buf); got != "draft" || e.cursor != 2 {
		t.Fatalf("failed search line = %q cursor=%d, want original", got, e.cursor)
	}
	if got := e.historySearchDisplay(); !strings.Contains(got, "failed reverse-i-search") {
		t.Fatalf("search display = %q, want failed marker", got)
	}
}

func TestDecodeHistorySearchKeys(t *testing.T) {
	e := &LineEditor{}
	ctrlR, err := e.decodeKey(0x12)
	if err != nil {
		t.Fatalf("decode Ctrl+R: %v", err)
	}
	if ctrlR.key != keyCtrlR {
		t.Fatalf("Ctrl+R key = %v, want keyCtrlR", ctrlR.key)
	}
	ctrlG, err := e.decodeKey(0x07)
	if err != nil {
		t.Fatalf("decode Ctrl+G: %v", err)
	}
	if ctrlG.key != keyCtrlG {
		t.Fatalf("Ctrl+G key = %v, want keyCtrlG", ctrlG.key)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
