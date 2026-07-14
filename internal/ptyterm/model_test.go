package ptyterm

import (
	"testing"
)

func TestEmulatorControlCharactersCursorAndErase(t *testing.T) {
	e := NewEmulator(Size{Cols: 12, Rows: 3}, 10)
	writeModel(t, e, []byte("hello\rHE\tZ\r\nnext\x1b[1;1HX\x1b[K"))

	snap := e.Snapshot()
	if got := snap.Lines[0]; got != "X" {
		t.Fatalf("line 0 = %q, want %q", got, "X")
	}
	if got := snap.Lines[1]; got != "next" {
		t.Fatalf("line 1 = %q, want %q", got, "next")
	}
	if snap.Cursor != (Cursor{X: 1, Y: 0}) {
		t.Fatalf("cursor = %+v", snap.Cursor)
	}
}

func TestEmulatorScrollbackAndResize(t *testing.T) {
	e := NewEmulator(Size{Cols: 5, Rows: 2}, 3)
	writeModel(t, e, []byte("one\r\ntwo\r\nthree\r\nfour"))

	snap := e.Snapshot()
	if got := snap.History; len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("history = %#v", got)
	}
	if got := snap.Lines; got[0] != "three" || got[1] != "four" {
		t.Fatalf("lines = %#v", got)
	}

	e.Resize(Size{Cols: 8, Rows: 1})
	snap = e.Snapshot()
	if snap.Size != (Size{Cols: 8, Rows: 1}) {
		t.Fatalf("size = %+v", snap.Size)
	}
	if len(snap.History) > 3 {
		t.Fatalf("history exceeded limit: %#v", snap.History)
	}
	if got := snap.Lines[0]; got != "three" {
		t.Fatalf("line after resize = %q", got)
	}
}

func TestEmulatorAlternateScreenPreservesMainScreen(t *testing.T) {
	e := NewEmulator(Size{Cols: 12, Rows: 3}, 10)
	writeModel(t, e, []byte("main\x1b[?1049hALT\x1b[2;1Hscreen\x1b[?1049l"))

	snap := e.Snapshot()
	if snap.AltScreen {
		t.Fatalf("alt screen still active")
	}
	if got := snap.Lines[0]; got != "main" {
		t.Fatalf("main line = %q", got)
	}
	if snap.Cursor != (Cursor{X: 4, Y: 0}) {
		t.Fatalf("cursor after alternate screen = %+v, want main-screen cursor", snap.Cursor)
	}

	writeModel(t, e, []byte("X"))
	snap = e.Snapshot()
	if got := snap.Lines[0]; got != "mainX" {
		t.Fatalf("main line after alternate-screen exit write = %q", got)
	}

	writeModel(t, e, []byte("\x1b[?1049h"))
	snap = e.Snapshot()
	if !snap.AltScreen {
		t.Fatalf("alt screen not active")
	}
	if got := snap.Lines[0]; got != "" {
		t.Fatalf("fresh alt line = %q", got)
	}
}

func TestEmulatorPrivateCursorSaveRestore(t *testing.T) {
	e := NewEmulator(Size{Cols: 12, Rows: 3}, 0)
	writeModel(t, e, []byte("prompt\x1b[?1048h\x1b[3;5Halt\x1b[?1048lX"))

	snap := e.Snapshot()
	if got := snap.Lines[0]; got != "promptX" {
		t.Fatalf("line after private cursor restore = %q", got)
	}
	if snap.Cursor != (Cursor{X: 7, Y: 0}) {
		t.Fatalf("cursor after private restore = %+v", snap.Cursor)
	}
}

func TestEmulatorSGRAttributesAreStructuredState(t *testing.T) {
	e := NewEmulator(Size{Cols: 8, Rows: 2}, 0)
	writeModel(t, e, []byte("\x1b[1;31mR\x1b[0mN"))

	snap := e.Snapshot()
	red := snap.Cells[0][0]
	normal := snap.Cells[0][1]
	if red.R != 'R' || !red.Attr.Bold || red.Attr.FG != 1 {
		t.Fatalf("red cell = %+v", red)
	}
	if normal.R != 'N' || normal.Attr.Bold || normal.Attr.FG != -1 {
		t.Fatalf("normal cell = %+v", normal)
	}
}

func TestEmulatorExtendedSGRAndCursorVisibility(t *testing.T) {
	e := NewEmulator(Size{Cols: 8, Rows: 2}, 0)
	writeModel(t, e, []byte("\x1b[38;5;196mP\x1b[48;2;1;2;3mT\x1b[38:2::4:5:6mR\x1b[?25l"))

	snap := e.Snapshot()
	if snap.CursorVisible {
		t.Fatalf("cursor visible = true, want false")
	}
	if got := snap.Cells[0][0].Attr.FG; got != 196 {
		t.Fatalf("palette foreground = %d, want 196", got)
	}
	wantBG := trueColorBase | 1<<16 | 2<<8 | 3
	if got := snap.Cells[0][1].Attr.BG; got != wantBG {
		t.Fatalf("truecolor background = %#x, want %#x", got, wantBG)
	}
	wantFG := trueColorBase | 4<<16 | 5<<8 | 6
	if got := snap.Cells[0][2].Attr.FG; got != wantFG {
		t.Fatalf("colon truecolor foreground = %#x, want %#x", got, wantFG)
	}

	writeModel(t, e, []byte("\x1b[?25h"))
	if !e.Snapshot().CursorVisible {
		t.Fatalf("cursor visible = false, want true")
	}
}

func TestEmulatorEraseAndScrollUseCurrentAttributes(t *testing.T) {
	e := NewEmulator(Size{Cols: 4, Rows: 2}, 10)
	writeModel(t, e, []byte("\x1b[48;5;17m\x1b[2J"))

	snap := e.Snapshot()
	for y := range snap.Cells {
		for x, cell := range snap.Cells[y] {
			if cell.R != ' ' || cell.Attr.BG != 17 {
				t.Fatalf("erased cell[%d][%d] = %+v, want blank with bg 17", y, x, cell)
			}
		}
	}

	writeModel(t, e, []byte("\x1b[48;5;52m\x1b[2;1H\n"))
	snap = e.Snapshot()
	for x, cell := range snap.Cells[1] {
		if cell.R != ' ' || cell.Attr.BG != 52 {
			t.Fatalf("scrolled cell[1][%d] = %+v, want blank with bg 52", x, cell)
		}
	}
}

func TestEmulatorUTF8WideRunesAndSnapshotRestore(t *testing.T) {
	e := NewEmulator(Size{Cols: 8, Rows: 2}, 5)
	writeModel(t, e, []byte("a界b\x1b]0;title\x07"))

	snap := e.Snapshot()
	if got := snap.Lines[0]; got != "a界b" {
		t.Fatalf("line = %q", got)
	}
	if snap.Cells[0][1].R != '界' || snap.Cells[0][2].R != 0 || snap.Cursor.X != 4 {
		t.Fatalf("wide rune cells/cursor = %+v cursor=%+v", snap.Cells[0][:4], snap.Cursor)
	}
	if snap.Title != "title" {
		t.Fatalf("title = %q", snap.Title)
	}

	restored := Restore(snap, 5).Snapshot()
	if restored.Lines[0] != snap.Lines[0] || restored.Cells[0][1].R != '界' || restored.Title != snap.Title {
		t.Fatalf("restored = %+v want line/title from %+v", restored, snap)
	}
}

func TestEmulatorWideRuneWrapsBeforeLastColumn(t *testing.T) {
	e := NewEmulator(Size{Cols: 4, Rows: 2}, 5)
	writeModel(t, e, []byte("abc界z"))

	snap := e.Snapshot()
	if got := snap.Lines[0]; got != "abc" {
		t.Fatalf("first line = %q, want %q", got, "abc")
	}
	if got := snap.Lines[1]; got != "界z" {
		t.Fatalf("second line = %q, want %q", got, "界z")
	}
	if snap.Cells[1][0].R != '界' || snap.Cells[1][1].R != 0 || snap.Cursor != (Cursor{X: 3, Y: 1}) {
		t.Fatalf("wide wrap cells/cursor = %+v cursor=%+v", snap.Cells[1], snap.Cursor)
	}
}

func TestEmulatorSaveRestoreCursor(t *testing.T) {
	e := NewEmulator(Size{Cols: 10, Rows: 3}, 0)
	writeModel(t, e, []byte("one\x1b7\x1b[3;1Htwo\x1b8X"))

	snap := e.Snapshot()
	if got := snap.Lines[0]; got != "oneX" {
		t.Fatalf("line 0 = %q", got)
	}
	if got := snap.Lines[2]; got != "two" {
		t.Fatalf("line 2 = %q", got)
	}
}

func TestEmulatorScrollRegionLineFeedAndReverseIndex(t *testing.T) {
	e := NewEmulator(Size{Cols: 8, Rows: 4}, 10)
	writeModel(t, e, []byte("top\r\none\r\ntwo\r\nbottom"))
	writeModel(t, e, []byte("\x1b[2;3r\x1b[3;1H\n"))

	snap := e.Snapshot()
	if got := snap.Lines; got[0] != "top" || got[1] != "two" || got[2] != "" || got[3] != "bottom" {
		t.Fatalf("after regional LF lines = %#v", got)
	}
	if len(snap.History) != 0 {
		t.Fatalf("regional scroll entered history: %#v", snap.History)
	}

	writeModel(t, e, []byte("\x1b[2;1H\x1bM"))
	snap = e.Snapshot()
	if got := snap.Lines; got[0] != "top" || got[1] != "" || got[2] != "two" || got[3] != "bottom" {
		t.Fatalf("after RI lines = %#v", got)
	}
}

func TestEmulatorInsertDeleteAndEraseCharacters(t *testing.T) {
	e := NewEmulator(Size{Cols: 12, Rows: 2}, 0)
	writeModel(t, e, []byte("abcdef\x1b[1;3H\x1b[2@XY\x1b[1;2H\x1b[2P\x1b[1;5H\x1b[3X"))

	snap := e.Snapshot()
	if got := snap.Lines[0]; got != "aYcd" {
		t.Fatalf("line = %q", got)
	}
}

func TestEmulatorInsertDeleteLinesRespectRegion(t *testing.T) {
	e := NewEmulator(Size{Cols: 8, Rows: 5}, 0)
	writeModel(t, e, []byte("zero\r\none\r\ntwo\r\nthree\r\nfour"))
	writeModel(t, e, []byte("\x1b[2;4r\x1b[3;1H\x1b[Lnew\x1b[2;1H\x1b[M"))

	snap := e.Snapshot()
	if got := snap.Lines; got[0] != "zero" || got[1] != "new" || got[2] != "two" || got[3] != "" || got[4] != "four" {
		t.Fatalf("lines = %#v", got)
	}
}

func TestEmulatorInsertOriginAndWrapModes(t *testing.T) {
	e := NewEmulator(Size{Cols: 6, Rows: 4}, 0)
	writeModel(t, e, []byte("abcdef\x1b[1;3H\x1b[4hZ\x1b[4l"))
	if got := e.Snapshot().Lines[0]; got != "abZcde" {
		t.Fatalf("insert mode line = %q", got)
	}

	writeModel(t, e, []byte("\x1b[2;3r\x1b[?6h\x1b[1;1HO\x1b[?6l"))
	snap := e.Snapshot()
	if got := snap.Lines[1]; got != "O" {
		t.Fatalf("origin mode line = %q", got)
	}

	writeModel(t, e, []byte("\x1b[?7l\x1b[4;1Habcdefghi"))
	snap = e.Snapshot()
	if got := snap.Lines[3]; got != "abcdei" {
		t.Fatalf("wrap disabled line = %q", got)
	}
	if snap.Cursor != (Cursor{X: 5, Y: 3}) {
		t.Fatalf("wrap disabled cursor = %+v", snap.Cursor)
	}
}

func TestEmulatorVT100LineDrawingCharset(t *testing.T) {
	e := NewEmulator(Size{Cols: 8, Rows: 3}, 0)
	writeModel(t, e, []byte("\x1b(0lqk\r\nx x\r\nmqj\x1b(B"))

	snap := e.Snapshot()
	if got := snap.Lines[0]; got != "┌─┐" {
		t.Fatalf("top border = %q", got)
	}
	if got := snap.Lines[1]; got != "│ │" {
		t.Fatalf("middle border = %q", got)
	}
	if got := snap.Lines[2]; got != "└─┘" {
		t.Fatalf("bottom border = %q", got)
	}
}

func TestEmulatorRepeatAndLineCursorMoves(t *testing.T) {
	e := NewEmulator(Size{Cols: 8, Rows: 4}, 0)
	writeModel(t, e, []byte("A\x1b[3b\x1b[2Edown\x1b[1Fup\x1b[4drow"))

	snap := e.Snapshot()
	if got := snap.Lines[0]; got != "AAAA" {
		t.Fatalf("repeat line = %q", got)
	}
	if got := snap.Lines[1]; got != "up" {
		t.Fatalf("CPL line = %q", got)
	}
	if got := snap.Lines[2]; got != "down" {
		t.Fatalf("CNL line = %q", got)
	}
	if got := snap.Lines[3]; got != "  row" {
		t.Fatalf("VPA line = %q", got)
	}
}

func writeModel(t *testing.T, e *Emulator, data []byte) {
	t.Helper()
	if n, err := e.Write(data); err != nil || n != len(data) {
		t.Fatalf("write model n=%d err=%v", n, err)
	}
}

func FuzzEmulatorArbitraryTerminalBytesPreserveSnapshotInvariants(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("plain\r\ntext"),
		[]byte("\x1b[?1049h界\x1b[999;999H\x1b[?1049l"),
		[]byte("\x1b[38;2;255;0;7mred\x1b]0;title\x07"),
		[]byte("\x1b[2;4r\x1b[999999999999999999999A\x1b[3X"),
		{0, 1, 2, 3, 0x1b, '[', '?', ';', ':', 0xff, 0xfe},
	} {
		f.Add(seed, uint8(80), uint8(24), uint8(20))
	}
	f.Fuzz(func(t *testing.T, data []byte, colsByte, rowsByte, historyByte uint8) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		size := Size{Cols: int(colsByte%120) + 1, Rows: int(rowsByte%50) + 1}
		historyLimit := int(historyByte % 100)
		e := NewEmulator(size, historyLimit)
		for offset := 0; offset < len(data); {
			chunk := int(data[offset]%31) + 1
			if offset+chunk > len(data) {
				chunk = len(data) - offset
			}
			writeModel(t, e, data[offset:offset+chunk])
			offset += chunk
		}
		assertSnapshotInvariants(t, e.Snapshot(), historyLimit, int64(len(data)))

		resized := Size{Cols: size.Rows%97 + 1, Rows: size.Cols%37 + 1}
		e.Resize(resized)
		snapshot := e.Snapshot()
		assertSnapshotInvariants(t, snapshot, historyLimit, int64(len(data)))
		assertSnapshotInvariants(t, Restore(snapshot, historyLimit).Snapshot(), historyLimit, int64(len(data)))
	})
}

func assertSnapshotInvariants(t *testing.T, snapshot Snapshot, historyLimit int, bytesRead int64) {
	t.Helper()
	if snapshot.Size.Cols < 1 || snapshot.Size.Rows < 1 {
		t.Fatalf("invalid size: %+v", snapshot.Size)
	}
	if len(snapshot.Lines) != snapshot.Size.Rows || len(snapshot.Cells) != snapshot.Size.Rows {
		t.Fatalf("rows: lines=%d cells=%d size=%+v", len(snapshot.Lines), len(snapshot.Cells), snapshot.Size)
	}
	for y, row := range snapshot.Cells {
		if len(row) != snapshot.Size.Cols {
			t.Fatalf("row %d has %d cells, want %d", y, len(row), snapshot.Size.Cols)
		}
	}
	if snapshot.Cursor.X < 0 || snapshot.Cursor.X > snapshot.Size.Cols ||
		snapshot.Cursor.Y < 0 || snapshot.Cursor.Y >= snapshot.Size.Rows {
		t.Fatalf("cursor %+v outside size %+v", snapshot.Cursor, snapshot.Size)
	}
	if len(snapshot.History) > historyLimit {
		t.Fatalf("history length %d exceeds limit %d", len(snapshot.History), historyLimit)
	}
	if snapshot.BytesRead != bytesRead {
		t.Fatalf("bytes read = %d, want %d", snapshot.BytesRead, bytesRead)
	}
	if len(snapshot.RawTail) > 64<<10 {
		t.Fatalf("raw tail length = %d", len(snapshot.RawTail))
	}
}
