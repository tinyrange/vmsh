package editor

import (
	"bytes"
	"strings"
	"testing"
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
