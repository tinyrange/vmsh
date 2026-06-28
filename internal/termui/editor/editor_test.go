package editor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/termui/terminal"
)

func TestHistoryRejectsMultilineEntries(t *testing.T) {
	h := &history{limit: 10}
	h.add("one\ntwo")
	if len(h.items) != 0 {
		t.Fatalf("history items = %#v, want none", h.items)
	}
}

func TestReadLineConsumesQueuedCompleteLine(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ed := New(Options{In: r, Out: os.Stderr})
	ed.queued = []rune("echo hi\nnext")

	line, err := ed.ReadLine(context.Background(), "> ")
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "echo hi" {
		t.Fatalf("line = %q, want echo hi", line)
	}
	if got := string(ed.queued); got != "next" {
		t.Fatalf("queued = %q, want next", got)
	}
}

func TestReadLinePreparedPreservesBufferedPromptInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	out, err := os.CreateTemp("", "termui-editor-prepared-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out.Name())
	defer out.Close()
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: 80, Height: 24}
	ed := New(Options{In: r, Out: out, Capabilities: &caps})
	if _, err := w.Write([]byte("ls\n@freebsd\nass\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	for _, want := range []string{"ls", "@freebsd", "ass"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		got, err := ed.ReadLinePrepared(ctx, "> ")
		cancel()
		if err != nil {
			t.Fatalf("ReadLinePrepared(%q): %v", want, err)
		}
		if got != want {
			t.Fatalf("ReadLinePrepared returned %q, want %q", got, want)
		}
	}
}

func TestRefreshMovesCursorLeft(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	out, err := os.CreateTemp("", "termui-editor-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out.Name())
	defer out.Close()
	ed := New(Options{In: r, Out: out})
	st := &lineState{prompt: "> ", buf: []rune("abc"), cursor: 1, width: 80}

	ed.refresh(st, "")
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if _, err := data.ReadFrom(out); err != nil {
		t.Fatal(err)
	}
	if got := data.String(); got != "\r\x1b[2K> abc\r\x1b[3C" {
		t.Fatalf("refresh = %q", got)
	}
}

func TestCompletionMenuRendersFuzzyPickerAndAcceptsSelection(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	st := &lineState{prompt: "> ", width: 80, height: 8}
	menu := completionMenu{
		active:     true,
		items:      []string{"alpha", "beta", "delta", "gamma"},
		filtered:   []string{"alpha", "beta", "delta", "gamma"},
		replaceLen: 0,
		selected:   1,
	}

	handled, accepted := e.handleCompletionMenu(st, &menu, keyEvent{key: keyEnter})
	if !handled || !accepted {
		t.Fatalf("handled=%t accepted=%t, want both true", handled, accepted)
	}
	if got := string(st.buf); got != "beta" {
		t.Fatalf("buffer = %q, want beta", got)
	}
}

func TestCompletionMenuLinesStayWithinTerminalWidth(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	st := &lineState{prompt: "> ", width: 80, height: 12}
	menu := completionMenu{active: true, selected: 0}
	for _, item := range []string{
		"411toppm", "DeRez", "MagickWand-config", "aa",
		"aarch64-elf-gcc-15.2.0", "aarch64-elf-lto-dump", "ac",
		"4channels", "DevToolsSecurity", "PasswordService",
		"aarch64-elf-addr2line", "aarch64-elf-gcc-ar",
	} {
		menu.items = append(menu.items, item)
	}
	menu.filtered = append([]string(nil), menu.items...)

	suffix := e.completionMenuSuffix(&menu, st)
	if strings.Contains(suffix, "\n") {
		t.Fatalf("suffix = %q, want inline picker", suffix)
	}
	if w := visibleWidth(suffix); w > st.width-1 {
		t.Fatalf("line width = %d > %d: %q", w, st.width-1, suffix)
	}
}

func TestCompletionMenuTypingRefinesFuzzyMatches(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	st := &lineState{}
	menu := completionMenu{
		active:     true,
		items:      []string{"alpha", "beta", "delta", "gamma"},
		filtered:   []string{"alpha", "beta", "delta", "gamma"},
		replaceLen: 0,
	}

	handled, accepted := e.handleCompletionMenu(st, &menu, keyEvent{key: keyRune, r: 'g'})
	if !handled || accepted {
		t.Fatalf("handled=%t accepted=%t, want handled refinement", handled, accepted)
	}
	if len(menu.filtered) == 0 || menu.filtered[0] != "gamma" {
		t.Fatalf("filtered = %#v, want gamma first", menu.filtered)
	}
}

func TestCompletionMenuKeepsSelectedVisible(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	st := &lineState{prompt: "> ", width: 140, height: 12}
	menu := completionMenu{
		active: true,
		items:  []string{"a", "b", "c", "d", "e", "f", "g"},
		filtered: []string{
			"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf",
		},
		selected: 3,
	}

	suffix := e.completionMenuSuffix(&menu, st)
	if !strings.Contains(suffix, "delta") {
		t.Fatalf("suffix = %q, want selected item visible", suffix)
	}
}

func TestCompletionMenuKeepsSelectedVisibleWithLongNeighbors(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	st := &lineState{prompt: "termui$ ", width: 80, height: 12}
	menu := completionMenu{
		active: true,
		items:  []string{"one", "two", "three", "four", "five"},
		filtered: []string{
			"411toppm", "4channels", "7zz", "AssetCacheLocatorUtil", "GetFileInfo",
		},
		selected: 3,
	}

	suffix := e.completionMenuSuffix(&menu, st)
	if !strings.Contains(suffix, "Asset") {
		t.Fatalf("suffix = %q, want selected AssetCache item visible", suffix)
	}
	if w := visibleWidth(suffix); w > st.width-1 {
		t.Fatalf("line width = %d > %d: %q", w, st.width-1, suffix)
	}
}

func TestFuzzyFilterRanksPrefixAndSubsequence(t *testing.T) {
	got := fuzzyFilter([]string{"aarch64-elf-gcc", "MagickWand-config", "gcc"}, "gcc")
	if len(got) == 0 || got[0] != "gcc" {
		t.Fatalf("matches = %#v, want exact prefix first", got)
	}
	got = fuzzyFilter([]string{"progress", "pwd", "printenv"}, "pg")
	if !containsString(got, "progress") {
		t.Fatalf("matches = %#v, want progress fuzzy match", got)
	}
}

func TestFuzzyFilterEmptyQueryKeepsAllCandidates(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	got := fuzzyFilter(items, "")
	if len(got) != len(items) {
		t.Fatalf("len = %d, want %d", len(got), len(items))
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestPhysicalRowsCountsWrapsAndIgnoresANSI(t *testing.T) {
	got := physicalRows("\x1b[7mabcdef\x1b[0m", 4)
	if got != 1 {
		t.Fatalf("rows = %d, want one wrapped row", got)
	}
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) {
	return 0, io.EOF
}

func TestInteractiveEOFIsNoInputYet(t *testing.T) {
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: 80, Height: 24}
	e := New(Options{
		Reader:       eofReader{},
		Writer:       io.Discard,
		Capabilities: &caps,
	})

	_, ok, err := e.pollKey()
	if ok {
		t.Fatalf("pollKey reported a key for EOF")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) && !isAgain(err) {
		t.Fatalf("pollKey err = %v, want retryable no-input error", err)
	}
}

func TestHistorySearchFindsNewestMatch(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	e.history.items = []string{"sleep 1", "progress 5", "sleep 9"}
	st := &lineState{buf: []rune("draft"), cursor: 5}

	search := e.startHistorySearch(st)
	for _, r := range "sleep" {
		e.handleHistorySearch(st, &search, keyEvent{key: keyRune, r: r})
	}

	if got := string(st.buf); got != "sleep 9" {
		t.Fatalf("search buffer = %q, want newest sleep command", got)
	}
}

func TestHistorySearchCtrlRCyclesOlderMatches(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	e.history.items = []string{"sleep 1", "progress 5", "sleep 9"}
	st := &lineState{}

	search := e.startHistorySearch(st)
	for _, r := range "sleep" {
		e.handleHistorySearch(st, &search, keyEvent{key: keyRune, r: r})
	}
	e.handleHistorySearch(st, &search, keyEvent{key: keyCtrlR})

	if got := string(st.buf); got != "sleep 1" {
		t.Fatalf("cycled buffer = %q, want older sleep command", got)
	}
}

func TestHistorySearchCancelRestoresDraft(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	e.history.items = []string{"help"}
	st := &lineState{buf: []rune("draft"), cursor: 3}

	search := e.startHistorySearch(st)
	e.handleHistorySearch(st, &search, keyEvent{key: keyRune, r: 'h'})
	e.handleHistorySearch(st, &search, keyEvent{key: keyCtrlG})

	if got := string(st.buf); got != "draft" || st.cursor != 3 {
		t.Fatalf("restored = %q cursor=%d, want draft cursor=3", got, st.cursor)
	}
}

func TestHistorySearchEnterAcceptsForExecution(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	e.history.items = []string{"help"}
	st := &lineState{}

	search := e.startHistorySearch(st)
	for _, r := range "help" {
		accepted, interrupted := e.handleHistorySearch(st, &search, keyEvent{key: keyRune, r: r})
		if accepted || interrupted {
			t.Fatalf("typing search unexpectedly accepted=%t interrupted=%t", accepted, interrupted)
		}
	}
	accepted, interrupted := e.handleHistorySearch(st, &search, keyEvent{key: keyEnter})
	if !accepted || interrupted {
		t.Fatalf("enter accepted=%t interrupted=%t, want accepted only", accepted, interrupted)
	}
	if got := string(st.buf); got != "help" {
		t.Fatalf("buffer = %q, want help", got)
	}
}
