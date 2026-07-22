package editor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/ptyterm"
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

func TestReadLinePreparedWaitsForBracketedPasteEnd(t *testing.T) {
	out, err := os.CreateTemp("", "termui-editor-paste-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out.Name())
	defer out.Close()
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: 80, Height: 24}
	ed := New(Options{
		In:           os.Stdin,
		Out:          out,
		Reader:       newDelayedReader("\x1b[200~stealth", "\x1b[201~\n", 25*time.Millisecond),
		Capabilities: &caps,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := ed.ReadLinePrepared(ctx, "> ")
	if err != nil {
		t.Fatalf("ReadLinePrepared: %v", err)
	}
	if got != "stealth" {
		t.Fatalf("line = %q, want stealth", got)
	}
}

func TestReadLinePreparedPreservesTextAfterNavigationKeys(t *testing.T) {
	out, err := os.CreateTemp("", "termui-editor-navigation-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out.Name())
	defer out.Close()
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: 80, Height: 24}
	ed := New(Options{
		In:           os.Stdin,
		Out:          out,
		Reader:       bytes.NewBufferString("tail\x1b[Hhead \x1b[F\n"),
		Capabilities: &caps,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := ed.ReadLinePrepared(ctx, "> ")
	if err != nil {
		t.Fatalf("ReadLinePrepared: %v", err)
	}
	if got != "head tail" {
		t.Fatalf("line = %q, want %q", got, "head tail")
	}
}

func TestReadLinePreparedPreservesTextAfterEscapeSequences(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sequence string
	}{
		{name: "left CSI", sequence: "\x1b[D"},
		{name: "left SS3", sequence: "\x1bOD"},
		{name: "right CSI", sequence: "\x1b[C"},
		{name: "up CSI", sequence: "\x1b[A"},
		{name: "down CSI", sequence: "\x1b[B"},
		{name: "home CSI", sequence: "\x1b[H"},
		{name: "home tilde", sequence: "\x1b[1~"},
		{name: "end CSI", sequence: "\x1b[F"},
		{name: "end tilde", sequence: "\x1b[4~"},
		{name: "delete", sequence: "\x1b[3~"},
		{name: "page up", sequence: "\x1b[5~"},
		{name: "page down", sequence: "\x1b[6~"},
		{name: "standalone escape", sequence: "\x1b"},
		{name: "unknown CSI", sequence: "\x1b[99~"},
		{name: "unknown SS3", sequence: "\x1bOP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := os.CreateTemp("", "termui-editor-escape-*")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(out.Name())
			defer out.Close()
			caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: 80, Height: 24}
			ed := New(Options{
				In:           os.Stdin,
				Out:          out,
				Reader:       bytes.NewBufferString(tc.sequence + "tail\n"),
				Capabilities: &caps,
			})

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			got, err := ed.ReadLinePrepared(ctx, "> ")
			if err != nil {
				t.Fatalf("ReadLinePrepared: %v", err)
			}
			if got != "tail" {
				t.Fatalf("line = %q, want %q", got, "tail")
			}
		})
	}
}

func TestReadLinePreparedPreservesTextAfterBracketedPaste(t *testing.T) {
	out, err := os.CreateTemp("", "termui-editor-paste-tail-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out.Name())
	defer out.Close()
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: 80, Height: 24}
	ed := New(Options{
		In:           os.Stdin,
		Out:          out,
		Reader:       bytes.NewBufferString("\x1b[200~paste\x1b[201~tail\n"),
		Capabilities: &caps,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := ed.ReadLinePrepared(ctx, "> ")
	if err != nil {
		t.Fatalf("ReadLinePrepared: %v", err)
	}
	if got != "pastetail" {
		t.Fatalf("line = %q, want %q", got, "pastetail")
	}
}

func TestReadLinePreparedRecognizesFragmentedEscapeSequences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
	}{
		{name: "CSI", chunks: []string{"\x1b[", "Dtail\n"}},
		{name: "SS3", chunks: []string{"\x1bO", "Dtail\n"}},
		{name: "parameters", chunks: []string{"\x1b[3", "~tail\n"}},
		{name: "bracketed paste start", chunks: []string{"\x1b[20", "0~paste\x1b[201~tail\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := os.CreateTemp("", "termui-editor-fragmented-*")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(out.Name())
			defer out.Close()
			caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: 80, Height: 24}
			ed := New(Options{
				In:           os.Stdin,
				Out:          out,
				Reader:       newGapReader(tc.chunks, 10*time.Millisecond),
				Capabilities: &caps,
			})

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := ed.ReadLinePrepared(ctx, "> ")
			if err != nil {
				t.Fatalf("ReadLinePrepared: %v", err)
			}
			want := "tail"
			if tc.name == "bracketed paste start" {
				want = "pastetail"
			}
			if got != want {
				t.Fatalf("line = %q, want %q", got, want)
			}
		})
	}
}

func TestReadLinePreparedRandomizedEditingMatchesLineModel(t *testing.T) {
	out, err := os.CreateTemp("", "termui-editor-random-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out.Name())
	defer out.Close()
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: 80, Height: 24}
	rng := rand.New(rand.NewSource(192))
	runes := []rune("abc XYZ09é界")

	for trial := 0; trial < 500; trial++ {
		var input bytes.Buffer
		var model []rune
		cursor := 0
		for step := 0; step < 100; step++ {
			switch rng.Intn(11) {
			case 0, 1, 2, 3, 4:
				r := runes[rng.Intn(len(runes))]
				input.WriteRune(r)
				model = append(model, 0)
				copy(model[cursor+1:], model[cursor:])
				model[cursor] = r
				cursor++
			case 5:
				input.WriteByte(0x7f)
				if cursor > 0 {
					model = append(model[:cursor-1], model[cursor:]...)
					cursor--
				}
			case 6:
				input.WriteString("\x1b[3~")
				if cursor < len(model) {
					model = append(model[:cursor], model[cursor+1:]...)
				}
			case 7:
				input.WriteString("\x1b[D")
				if cursor > 0 {
					cursor--
				}
			case 8:
				input.WriteString("\x1b[C")
				if cursor < len(model) {
					cursor++
				}
			case 9:
				input.WriteString("\x1b[H")
				cursor = 0
			case 10:
				input.WriteString("\x1b[F")
				cursor = len(model)
			}
		}
		input.WriteByte('\n')
		ed := New(Options{
			In:           os.Stdin,
			Out:          out,
			Reader:       &input,
			Writer:       io.Discard,
			Capabilities: &caps,
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		got, err := ed.ReadLinePrepared(ctx, "> ")
		cancel()
		if err != nil {
			t.Fatalf("trial %d: ReadLinePrepared: %v", trial, err)
		}
		if want := string(model); got != want {
			t.Fatalf("trial %d: line = %q, want %q", trial, got, want)
		}
	}
}

type gapReader struct {
	chunks  [][]byte
	delay   time.Duration
	readyAt time.Time
	waiting bool
}

func newGapReader(chunks []string, delay time.Duration) *gapReader {
	r := &gapReader{delay: delay}
	for _, chunk := range chunks {
		r.chunks = append(r.chunks, []byte(chunk))
	}
	return r
}

func (r *gapReader) Read(p []byte) (int, error) {
	for len(r.chunks) > 0 && len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
		r.waiting = len(r.chunks) > 0
		r.readyAt = time.Now().Add(r.delay)
	}
	if len(r.chunks) == 0 {
		return 0, syscall.EAGAIN
	}
	if r.waiting && time.Now().Before(r.readyAt) {
		return 0, syscall.EAGAIN
	}
	r.waiting = false
	p[0] = r.chunks[0][0]
	r.chunks[0] = r.chunks[0][1:]
	return 1, nil
}

type delayedReader struct {
	first     []byte
	second    []byte
	delay     time.Duration
	startedAt time.Time
}

func newDelayedReader(first, second string, delay time.Duration) *delayedReader {
	return &delayedReader{
		first:     []byte(first),
		second:    []byte(second),
		delay:     delay,
		startedAt: time.Now(),
	}
}

func (r *delayedReader) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		p[0] = r.first[0]
		r.first = r.first[1:]
		return 1, nil
	}
	if time.Since(r.startedAt) < r.delay {
		return 0, syscall.EAGAIN
	}
	if len(r.second) > 0 {
		p[0] = r.second[0]
		r.second = r.second[1:]
		return 1, nil
	}
	return 0, syscall.EAGAIN
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

func TestRefreshClearsWrappedHistoryEntry(t *testing.T) {
	const width = 24
	emulator := ptyterm.NewEmulator(ptyterm.Size{Cols: width, Rows: 10}, 20)
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: width, Height: 10}
	ed := New(Options{Reader: eofReader{}, Writer: emulator, Capabilities: &caps})
	st := &lineState{
		prompt: "> ",
		buf:    []rune("first-history-entry-that-wraps-across-several-terminal-lines"),
		width:  width,
		height: 10,
	}
	st.cursor = len(st.buf)

	ed.refresh(st, "")
	st.buf = []rune("short")
	st.cursor = len(st.buf)
	ed.refresh(st, "")

	snapshot := emulator.Snapshot()
	if got := snapshot.Lines[0]; got != "> short" {
		t.Fatalf("current line = %q, want %q", got, "> short")
	}
	for row, line := range snapshot.Lines[1:] {
		if line != "" {
			t.Fatalf("stale wrapped content on row %d: %q", row+1, line)
		}
	}
	if snapshot.Cursor != (ptyterm.Cursor{X: 7, Y: 0}) {
		t.Fatalf("cursor = %+v, want x=7 y=0", snapshot.Cursor)
	}
}

func TestRefreshPlacesCursorWithinWrappedLine(t *testing.T) {
	const width = 12
	emulator := ptyterm.NewEmulator(ptyterm.Size{Cols: width, Rows: 8}, 20)
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: width, Height: 8}
	ed := New(Options{Reader: eofReader{}, Writer: emulator, Capabilities: &caps})
	st := &lineState{
		prompt: "> ",
		buf:    []rune("abcdefghijklmno"),
		cursor: 5,
		width:  width,
		height: 8,
	}

	ed.refresh(st, "")

	snapshot := emulator.Snapshot()
	if got := snapshot.Lines[0] + snapshot.Lines[1]; got != "> abcdefghijklmno" {
		t.Fatalf("wrapped line = %q, want %q", got, "> abcdefghijklmno")
	}
	if snapshot.Cursor != (ptyterm.Cursor{X: 7, Y: 0}) {
		t.Fatalf("cursor = %+v, want x=7 y=0", snapshot.Cursor)
	}
}

func TestRefreshHandlesExactWidthBeforeShorterEntry(t *testing.T) {
	for _, exact := range []string{"abcdef", "界界界"} {
		t.Run(exact, func(t *testing.T) {
			const width = 8
			emulator := ptyterm.NewEmulator(ptyterm.Size{Cols: width, Rows: 5}, 10)
			caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: width, Height: 5}
			ed := New(Options{Reader: eofReader{}, Writer: emulator, Capabilities: &caps})
			st := &lineState{prompt: "> ", buf: []rune(exact), width: width, height: 5}
			st.cursor = len(st.buf)

			ed.refresh(st, "")
			if cursor := emulator.Snapshot().Cursor; cursor != (ptyterm.Cursor{X: 0, Y: 1}) {
				t.Fatalf("exact-width cursor = %+v, want x=0 y=1", cursor)
			}
			st.buf = []rune("x")
			st.cursor = len(st.buf)
			ed.refresh(st, "")

			snapshot := emulator.Snapshot()
			if snapshot.Lines[0] != "> x" || snapshot.Lines[1] != "" {
				t.Fatalf("screen after shorter entry = %q", snapshot.Lines)
			}
			if snapshot.Cursor != (ptyterm.Cursor{X: 3, Y: 0}) {
				t.Fatalf("short cursor = %+v, want x=3 y=0", snapshot.Cursor)
			}
		})
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

func TestCompletionMenuUsesVerticalLayoutOnWideTerminal(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	st := &lineState{prompt: "> ", width: 140, height: 12}
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
	below, inline := splitDisplaySuffix(suffix)
	if inline != "" {
		t.Fatalf("inline suffix = %q, want vertical picker", inline)
	}
	if len(below) < 2 {
		t.Fatalf("vertical lines = %#v, want header and candidates", below)
	}
	for _, line := range below {
		if w := visibleWidth(line); w > st.width {
			t.Fatalf("line width = %d > %d: %q", w, st.width, line)
		}
	}
}

func TestCompletionMenuUsesVerticalLayoutOnNarrowTerminal(t *testing.T) {
	e := New(Options{Reader: eofReader{}, Writer: io.Discard})
	st := &lineState{prompt: "> ", width: 64, height: 12}
	menu := completionMenu{
		active:   true,
		selected: 2,
		items: []string{
			"411toppm", "DeRez", "MagickWand-config", "aa",
			"aarch64-elf-gcc-15.2.0", "aarch64-elf-lto-dump",
		},
	}
	menu.filtered = append([]string(nil), menu.items...)

	suffix := e.completionMenuSuffix(&menu, st)
	below, inline := splitDisplaySuffix(suffix)
	if inline != "" {
		t.Fatalf("inline suffix = %q, want vertical picker", inline)
	}
	if len(below) < 2 {
		t.Fatalf("picker lines = %d, want completion menu lines", len(below))
	}
	if visibleWidth(below[0]) > st.width {
		t.Fatalf("header width = %d > %d: %q", visibleWidth(below[0]), st.width, below[0])
	}
	var selected bool
	for _, line := range below[1:] {
		if visibleWidth(line) > st.width {
			t.Fatalf("item width = %d > %d: %q", visibleWidth(line), st.width, line)
		}
		if strings.HasPrefix(line, "\x1b[7m>") {
			selected = true
		}
	}
	if !selected {
		t.Fatalf("picker = %#v, want selected item marker", below)
	}
}

func TestRefreshRendersCompletionBelowInputAndPreservesOutput(t *testing.T) {
	const width = 40
	emulator := ptyterm.NewEmulator(ptyterm.Size{Cols: width, Rows: 10}, 20)
	_, _ = emulator.Write([]byte("previous output\r\n"))
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: width, Height: 10}
	ed := New(Options{Reader: eofReader{}, Writer: emulator, Capabilities: &caps})
	st := &lineState{prompt: "> ", buf: []rune("a"), cursor: 1, width: width, height: 10}
	menu := completionMenu{
		active:   true,
		items:    []string{"alpha", "alpine", "archive"},
		filtered: []string{"alpha", "alpine", "archive"},
	}

	ed.refresh(st, ed.completionMenuSuffix(&menu, st))
	snapshot := emulator.Snapshot()
	if snapshot.Lines[0] != "previous output" || snapshot.Lines[1] != "> a" {
		t.Fatalf("output and input rows = %q", snapshot.Lines[:2])
	}
	if snapshot.Cursor != (ptyterm.Cursor{X: 3, Y: 1}) {
		t.Fatalf("cursor = %+v, want x=3 y=1", snapshot.Cursor)
	}
	for row, line := range snapshot.Lines {
		if strings.Contains(line, "alpha") && row <= snapshot.Cursor.Y {
			t.Fatalf("completion rendered above input on row %d: %q", row, line)
		}
	}

	ed.refresh(st, "")
	snapshot = emulator.Snapshot()
	if snapshot.Lines[0] != "previous output" || snapshot.Lines[1] != "> a" {
		t.Fatalf("output changed after closing completion: %q", snapshot.Lines[:2])
	}
	for row, line := range snapshot.Lines[2:] {
		if line != "" {
			t.Fatalf("completion content remained on row %d: %q", row+2, line)
		}
	}
}

func TestRefreshReservesCompletionRowsAtTerminalBottom(t *testing.T) {
	const (
		width  = 40
		height = 6
	)
	emulator := ptyterm.NewEmulator(ptyterm.Size{Cols: width, Rows: height}, 20)
	for i := 0; i < height-1; i++ {
		_, _ = fmt.Fprintf(emulator, "output-%d\r\n", i)
	}
	caps := terminal.Capabilities{Mode: terminal.ModeDynamicInteractive, Width: width, Height: height}
	ed := New(Options{Reader: eofReader{}, Writer: emulator, Capabilities: &caps})
	st := &lineState{prompt: "> ", buf: []rune("a"), cursor: 1, width: width, height: height}
	menu := completionMenu{
		active: true,
		items: []string{
			"alpha", "alpine", "archive", "awk", "basename", "bash", "cat", "chmod",
		},
	}
	menu.filtered = append([]string(nil), menu.items...)

	ed.refresh(st, ed.completionMenuSuffix(&menu, st))
	snapshot := emulator.Snapshot()
	if snapshot.Lines[snapshot.Cursor.Y] != "> a" {
		t.Fatalf("input was not moved above reserved rows: cursor=%+v lines=%q", snapshot.Cursor, snapshot.Lines)
	}
	var selectedRow = -1
	for row, cells := range snapshot.Cells {
		for _, cell := range cells {
			if cell.Attr.Inverse {
				selectedRow = row
			}
		}
	}
	if selectedRow <= snapshot.Cursor.Y {
		t.Fatalf("selected completion row = %d, input row = %d; lines=%q", selectedRow, snapshot.Cursor.Y, snapshot.Lines)
	}
	if len(snapshot.History) == 0 {
		t.Fatalf("completion did not scroll to reserve rows: lines=%q", snapshot.Lines)
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
