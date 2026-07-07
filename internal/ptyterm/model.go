package ptyterm

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type Size struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type Cursor struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Attr struct {
	Bold      bool `json:"bold,omitempty"`
	Underline bool `json:"underline,omitempty"`
	Inverse   bool `json:"inverse,omitempty"`
	FG        int  `json:"fg,omitempty"`
	BG        int  `json:"bg,omitempty"`
}

type Cell struct {
	R    rune `json:"r"`
	Attr Attr `json:"attr,omitempty"`
}

type Snapshot struct {
	Size          Size     `json:"size"`
	Cursor        Cursor   `json:"cursor"`
	CursorVisible bool     `json:"cursor_visible"`
	AltScreen     bool     `json:"alt_screen,omitempty"`
	Lines         []string `json:"lines"`
	Cells         [][]Cell `json:"cells,omitempty"`
	History       []string `json:"history,omitempty"`
	Title         string   `json:"title,omitempty"`
	RawTail       []byte   `json:"raw_tail,omitempty"`
	Exited        bool     `json:"exited,omitempty"`
	ExitCode      int      `json:"exit_code,omitempty"`
	ExitError     string   `json:"exit_error,omitempty"`
	LastResize    *Size    `json:"last_resize,omitempty"`
	BytesRead     int64    `json:"bytes_read,omitempty"`
	BytesStdin    int64    `json:"bytes_stdin,omitempty"`
	ResizeCount   int      `json:"resize_count,omitempty"`
}

type Emulator struct {
	mu             sync.Mutex
	size           Size
	historyLimit   int
	cursor         Cursor
	savedCursor    Cursor
	altSavedCursor Cursor
	cursorVisible  bool
	attr           Attr
	main           *screenBuffer
	alt            *screenBuffer
	altScreen      bool
	scrollTop      int
	scrollBottom   int
	insertMode     bool
	originMode     bool
	autoWrap       bool
	activeSet      int
	g0LineDraw     bool
	g1LineDraw     bool
	lastRune       rune
	lastAttr       Attr
	lastValid      bool
	title          string
	rawTail        []byte
	bytesRead      int64
	state          parserState
	esc            strings.Builder
	osc            strings.Builder
}

type parserState int

const (
	stateGround parserState = iota
	stateEscape
	stateCSI
	stateOSC
	stateCharsetG0
	stateCharsetG1
)

const trueColorBase = 1 << 24

type screenBuffer struct {
	lines [][]Cell
	hist  [][]Cell
}

func NewEmulator(size Size, historyLimit int) *Emulator {
	size = normalizeSize(size)
	if historyLimit < 0 {
		historyLimit = 0
	}
	e := &Emulator{
		size:          size,
		historyLimit:  historyLimit,
		main:          newScreenBuffer(size),
		alt:           newScreenBuffer(size),
		attr:          Attr{FG: -1, BG: -1},
		cursorVisible: true,
		scrollBottom:  size.Rows - 1,
		autoWrap:      true,
	}
	return e
}

func Restore(snapshot Snapshot, historyLimit int) *Emulator {
	e := NewEmulator(snapshot.Size, historyLimit)
	e.cursor = clampCursor(snapshot.Cursor, e.size)
	e.cursorVisible = snapshot.CursorVisible
	if !snapshot.CursorVisible && snapshot.BytesRead == 0 && len(snapshot.Lines) == 0 && len(snapshot.Cells) == 0 {
		e.cursorVisible = true
	}
	e.altScreen = snapshot.AltScreen
	e.title = snapshot.Title
	e.rawTail = append([]byte(nil), snapshot.RawTail...)
	e.bytesRead = snapshot.BytesRead
	target := e.active()
	if len(snapshot.Cells) != 0 {
		for y := 0; y < e.size.Rows && y < len(snapshot.Cells); y++ {
			target.lines[y] = blankLine(e.size.Cols)
			copy(target.lines[y], snapshot.Cells[y])
		}
	} else {
		for y := 0; y < e.size.Rows && y < len(snapshot.Lines); y++ {
			target.lines[y] = cellsFromString(snapshot.Lines[y], e.size.Cols, Attr{FG: -1, BG: -1})
		}
	}
	for _, line := range snapshot.History {
		target.hist = append(target.hist, cellsFromString(line, e.size.Cols, Attr{FG: -1, BG: -1}))
	}
	target.trimHistory(historyLimit)
	return e
}

func (e *Emulator) Write(data []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	written := len(data)
	e.bytesRead += int64(len(data))
	e.appendRawTail(data)
	for len(data) > 0 {
		b := data[0]
		switch e.state {
		case stateGround:
			if b == 0x1b {
				e.state = stateEscape
				e.esc.Reset()
				data = data[1:]
				continue
			}
			if b < 0x20 || b == 0x7f {
				e.control(b)
				data = data[1:]
				continue
			}
			r, n := utf8.DecodeRune(data)
			if r == utf8.RuneError && n == 1 {
				e.printRune(r)
				data = data[1:]
				continue
			}
			e.printRune(r)
			data = data[n:]
		case stateEscape:
			e.handleEscapeByte(b)
			data = data[1:]
		case stateCSI:
			e.esc.WriteByte(b)
			if b >= 0x40 && b <= 0x7e {
				e.handleCSI(e.esc.String())
				e.state = stateGround
			}
			data = data[1:]
		case stateOSC:
			if b == 0x07 {
				e.handleOSC(e.osc.String())
				e.state = stateGround
				data = data[1:]
				continue
			}
			if b == 0x1b && len(data) > 1 && data[1] == '\\' {
				e.handleOSC(e.osc.String())
				e.state = stateGround
				data = data[2:]
				continue
			}
			e.osc.WriteByte(b)
			data = data[1:]
		case stateCharsetG0:
			e.g0LineDraw = b == '0'
			e.state = stateGround
			data = data[1:]
		case stateCharsetG1:
			e.g1LineDraw = b == '0'
			e.state = stateGround
			data = data[1:]
		}
	}
	return written, nil
}

func (e *Emulator) Resize(size Size) {
	e.mu.Lock()
	defer e.mu.Unlock()
	size = normalizeSize(size)
	e.size = size
	e.main.resize(size, e.historyLimit)
	e.alt.resize(size, 0)
	e.cursor = clampCursor(e.cursor, e.size)
	e.savedCursor = clampCursor(e.savedCursor, e.size)
	e.altSavedCursor = clampCursor(e.altSavedCursor, e.size)
	e.scrollTop = 0
	e.scrollBottom = e.size.Rows - 1
}

func (e *Emulator) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	buf := e.active()
	lines := make([]string, 0, len(buf.lines))
	cells := make([][]Cell, 0, len(buf.lines))
	for _, line := range buf.lines {
		lines = append(lines, cellsString(line))
		cells = append(cells, cloneLine(line))
	}
	history := make([]string, 0, len(buf.hist))
	for _, line := range buf.hist {
		history = append(history, cellsString(line))
	}
	return Snapshot{
		Size:          e.size,
		Cursor:        e.cursor,
		CursorVisible: e.cursorVisible,
		AltScreen:     e.altScreen,
		Lines:         lines,
		Cells:         cells,
		History:       history,
		Title:         e.title,
		RawTail:       append([]byte(nil), e.rawTail...),
		BytesRead:     e.bytesRead,
	}
}

func (e *Emulator) active() *screenBuffer {
	if e.altScreen {
		return e.alt
	}
	return e.main
}

func (e *Emulator) appendRawTail(data []byte) {
	const limit = 64 << 10
	e.rawTail = append(e.rawTail, data...)
	if len(e.rawTail) > limit {
		e.rawTail = append([]byte(nil), e.rawTail[len(e.rawTail)-limit:]...)
	}
}

func (e *Emulator) control(b byte) {
	switch b {
	case '\a':
	case '\b':
		if e.cursor.X > 0 {
			e.cursor.X--
		}
	case '\t':
		next := ((e.cursor.X / 8) + 1) * 8
		if next >= e.size.Cols {
			next = e.size.Cols - 1
		}
		e.cursor.X = next
	case '\n', '\v', '\f':
		e.lineFeed()
	case '\r':
		e.cursor.X = 0
	case '\016':
		e.activeSet = 1
	case '\017':
		e.activeSet = 0
	}
}

func (e *Emulator) printRune(r rune) {
	if r == 0 {
		return
	}
	if unicode.Is(unicode.Mn, r) {
		return
	}
	width := runeWidth(r)
	if width <= 0 {
		return
	}
	if e.cursor.X >= e.size.Cols {
		if e.autoWrap {
			e.cursor.X = 0
			e.lineFeed()
		} else {
			e.cursor.X = e.size.Cols - 1
		}
	}
	if e.cursor.X+width > e.size.Cols && e.autoWrap {
		e.cursor.X = 0
		e.lineFeed()
	}
	r = e.mapCharset(r)
	buf := e.active()
	line := buf.lines[e.cursor.Y]
	if e.insertMode {
		insertCells(line, e.cursor.X, width, e.attr)
	}
	line[e.cursor.X] = Cell{R: r, Attr: e.attr}
	e.lastRune = r
	e.lastAttr = e.attr
	e.lastValid = true
	for i := 1; i < width && e.cursor.X+i < e.size.Cols; i++ {
		line[e.cursor.X+i] = Cell{R: 0, Attr: e.attr}
	}
	e.cursor.X += width
	if e.cursor.X >= e.size.Cols {
		if e.autoWrap {
			e.cursor.X = e.size.Cols
		} else {
			e.cursor.X = e.size.Cols - 1
		}
	}
}

func (e *Emulator) lineFeed() {
	if e.cursor.Y == e.scrollBottom {
		e.active().scrollRegion(e.scrollTop, e.scrollBottom, e.historyLimit, e.attr)
		return
	}
	if e.cursor.Y < e.size.Rows-1 {
		e.cursor.Y++
	}
}

func (e *Emulator) handleEscapeByte(b byte) {
	switch b {
	case '[':
		e.state = stateCSI
		e.esc.Reset()
	case ']':
		e.state = stateOSC
		e.osc.Reset()
	case 'c':
		e.reset()
		e.state = stateGround
	case 'D':
		e.lineFeed()
		e.state = stateGround
	case 'E':
		e.cursor.X = 0
		e.lineFeed()
		e.state = stateGround
	case 'H':
		e.state = stateGround
	case 'M':
		e.reverseIndex()
		e.state = stateGround
	case '7':
		e.saveCursor()
		e.state = stateGround
	case '8':
		e.restoreCursor()
		e.state = stateGround
	case '(':
		e.state = stateCharsetG0
	case ')':
		e.state = stateCharsetG1
	default:
		e.state = stateGround
	}
}

func (e *Emulator) handleCSI(seq string) {
	if seq == "" {
		return
	}
	final := seq[len(seq)-1]
	body := seq[:len(seq)-1]
	private := false
	if strings.HasPrefix(body, "?") {
		private = true
		body = strings.TrimPrefix(body, "?")
	}
	params := parseCSIParams(body)
	switch final {
	case '@':
		e.insertCharacter(paramDefault(params, 0, 1))
	case 'A':
		e.cursorUp(paramDefault(params, 0, 1))
	case 'B':
		e.cursorDown(paramDefault(params, 0, 1))
	case 'C':
		e.cursor.X += paramDefault(params, 0, 1)
	case 'D':
		e.cursor.X -= paramDefault(params, 0, 1)
	case 'E':
		e.cursor.X = 0
		e.cursorDown(paramDefault(params, 0, 1))
	case 'F':
		e.cursor.X = 0
		e.cursorUp(paramDefault(params, 0, 1))
	case 'G':
		e.cursor.X = paramDefault(params, 0, 1) - 1
	case 'H', 'f':
		y := paramDefault(params, 0, 1) - 1
		if e.originMode {
			y += e.scrollTop
		}
		e.cursor.Y = y
		e.cursor.X = paramDefault(params, 1, 1) - 1
	case 'J':
		e.eraseDisplay(paramDefault(params, 0, 0))
	case 'K':
		e.eraseLine(paramDefault(params, 0, 0))
	case 'L':
		e.insertLine(paramDefault(params, 0, 1))
	case 'M':
		e.deleteLine(paramDefault(params, 0, 1))
	case 'P':
		e.deleteCharacter(paramDefault(params, 0, 1))
	case 'S':
		e.scrollUp(paramDefault(params, 0, 1))
	case 'T':
		e.scrollDown(paramDefault(params, 0, 1))
	case 'X':
		e.eraseCharacters(paramDefault(params, 0, 1))
	case 'b':
		e.repeatLast(paramDefault(params, 0, 1))
	case 'd':
		y := paramDefault(params, 0, 1) - 1
		if e.originMode {
			y += e.scrollTop
		}
		e.cursor.Y = y
	case 'm':
		e.setSGR(params)
	case 'r':
		e.setScrollRegion(paramDefault(params, 0, 1)-1, paramDefault(params, 1, e.size.Rows)-1)
	case 's':
		e.saveCursor()
	case 'u':
		e.restoreCursor()
	case 'h':
		if private {
			e.setPrivateMode(params, true)
		} else {
			e.setMode(params, true)
		}
	case 'l':
		if private {
			e.setPrivateMode(params, false)
		} else {
			e.setMode(params, false)
		}
	}
	e.cursor = clampCursor(e.cursor, e.size)
}

func (e *Emulator) handleOSC(seq string) {
	if strings.HasPrefix(seq, "0;") || strings.HasPrefix(seq, "2;") {
		_, title, _ := strings.Cut(seq, ";")
		e.title = title
	}
}

func (e *Emulator) eraseDisplay(mode int) {
	buf := e.active()
	switch mode {
	case 2, 3:
		for y := range buf.lines {
			buf.lines[y] = blankLineWithAttr(e.size.Cols, e.attr)
		}
		if mode == 3 {
			buf.hist = nil
		}
	case 1:
		for y := 0; y <= e.cursor.Y && y < len(buf.lines); y++ {
			end := e.size.Cols
			if y == e.cursor.Y {
				end = e.cursor.X + 1
			}
			clearRange(buf.lines[y], 0, end, e.attr)
		}
	default:
		for y := e.cursor.Y; y < len(buf.lines); y++ {
			start := 0
			if y == e.cursor.Y {
				start = e.cursor.X
			}
			clearRange(buf.lines[y], start, e.size.Cols, e.attr)
		}
	}
}

func (e *Emulator) eraseLine(mode int) {
	line := e.active().lines[e.cursor.Y]
	switch mode {
	case 1:
		clearRange(line, 0, e.cursor.X+1, e.attr)
	case 2:
		clearRange(line, 0, e.size.Cols, e.attr)
	default:
		clearRange(line, e.cursor.X, e.size.Cols, e.attr)
	}
}

func (e *Emulator) setSGR(params []int) {
	if len(params) == 0 {
		params = []int{0}
	}
	for i := 0; i < len(params); i++ {
		p := params[i]
		switch {
		case p == 0:
			e.attr = Attr{FG: -1, BG: -1}
		case p == 1:
			e.attr.Bold = true
		case p == 4:
			e.attr.Underline = true
		case p == 7:
			e.attr.Inverse = true
		case p == 22:
			e.attr.Bold = false
		case p == 24:
			e.attr.Underline = false
		case p == 27:
			e.attr.Inverse = false
		case p == 39:
			e.attr.FG = -1
		case p == 49:
			e.attr.BG = -1
		case p == 38:
			if color, ok := extendedColor(params, &i); ok {
				e.attr.FG = color
			}
		case p == 48:
			if color, ok := extendedColor(params, &i); ok {
				e.attr.BG = color
			}
		case p >= 30 && p <= 37:
			e.attr.FG = p - 30
		case p >= 40 && p <= 47:
			e.attr.BG = p - 40
		case p >= 90 && p <= 97:
			e.attr.FG = p - 90 + 8
		case p >= 100 && p <= 107:
			e.attr.BG = p - 100 + 8
		}
	}
}

func (e *Emulator) setPrivateMode(params []int, enabled bool) {
	for _, p := range params {
		switch p {
		case 6:
			e.originMode = enabled
			e.cursor = Cursor{}
			if enabled {
				e.cursor.Y = e.scrollTop
			}
		case 7:
			e.autoWrap = enabled
		case 25:
			e.cursorVisible = enabled
		case 47, 1047:
			e.altScreen = enabled
			e.cursor = Cursor{}
			if enabled {
				e.alt = newScreenBuffer(e.size)
			}
		case 1048:
			if enabled {
				e.saveCursor()
			} else {
				e.restoreCursor()
			}
		case 1049:
			if enabled {
				e.altSavedCursor = e.cursor
				e.altScreen = true
				e.cursor = Cursor{}
				e.alt = newScreenBuffer(e.size)
			} else {
				e.altScreen = false
				e.cursor = clampCursor(e.altSavedCursor, e.size)
			}
		}
	}
}

func (e *Emulator) setMode(params []int, enabled bool) {
	for _, p := range params {
		if p == 4 {
			e.insertMode = enabled
		}
	}
}

func (e *Emulator) reset() {
	e.main = newScreenBuffer(e.size)
	e.alt = newScreenBuffer(e.size)
	e.altScreen = false
	e.cursor = Cursor{}
	e.savedCursor = Cursor{}
	e.altSavedCursor = Cursor{}
	e.cursorVisible = true
	e.attr = Attr{FG: -1, BG: -1}
	e.scrollTop = 0
	e.scrollBottom = e.size.Rows - 1
	e.insertMode = false
	e.originMode = false
	e.autoWrap = true
	e.activeSet = 0
	e.g0LineDraw = false
	e.g1LineDraw = false
	e.lastRune = 0
	e.lastAttr = Attr{}
	e.lastValid = false
	e.title = ""
}

func (e *Emulator) saveCursor() {
	e.savedCursor = e.cursor
}

func (e *Emulator) restoreCursor() {
	e.cursor = clampCursor(e.savedCursor, e.size)
}

func (e *Emulator) reverseIndex() {
	if e.cursor.Y != e.scrollTop {
		if e.cursor.Y > 0 {
			e.cursor.Y--
		}
		return
	}
	e.active().scrollRegionDown(e.scrollTop, e.scrollBottom, e.attr)
}

func (e *Emulator) cursorUp(n int) {
	if n < 1 {
		n = 1
	}
	limit := 0
	if e.cursor.Y >= e.scrollTop && e.cursor.Y <= e.scrollBottom {
		limit = e.scrollTop
	}
	e.cursor.Y -= n
	if e.cursor.Y < limit {
		e.cursor.Y = limit
	}
}

func (e *Emulator) cursorDown(n int) {
	if n < 1 {
		n = 1
	}
	limit := e.size.Rows - 1
	if e.cursor.Y >= e.scrollTop && e.cursor.Y <= e.scrollBottom {
		limit = e.scrollBottom
	}
	e.cursor.Y += n
	if e.cursor.Y > limit {
		e.cursor.Y = limit
	}
}

func (e *Emulator) setScrollRegion(top, bottom int) {
	if top < 0 {
		top = 0
	}
	if bottom >= e.size.Rows {
		bottom = e.size.Rows - 1
	}
	if top >= bottom {
		return
	}
	e.scrollTop = top
	e.scrollBottom = bottom
	e.cursor = Cursor{}
}

func (e *Emulator) insertCharacter(n int) {
	if n < 1 {
		n = 1
	}
	line := e.active().lines[e.cursor.Y]
	insertCells(line, e.cursor.X, n, e.attr)
}

func (e *Emulator) deleteCharacter(n int) {
	if n < 1 {
		n = 1
	}
	line := e.active().lines[e.cursor.Y]
	if e.cursor.X >= len(line) {
		return
	}
	if n > len(line)-e.cursor.X {
		n = len(line) - e.cursor.X
	}
	copy(line[e.cursor.X:], line[e.cursor.X+n:])
	clearRange(line, len(line)-n, len(line), e.attr)
}

func (e *Emulator) eraseCharacters(n int) {
	if n < 1 {
		n = 1
	}
	line := e.active().lines[e.cursor.Y]
	clearRange(line, e.cursor.X, e.cursor.X+n, e.attr)
}

func (e *Emulator) repeatLast(n int) {
	if !e.lastValid {
		return
	}
	if n < 1 {
		n = 1
	}
	attr := e.attr
	e.attr = e.lastAttr
	for i := 0; i < n; i++ {
		e.printRune(e.lastRune)
	}
	e.attr = attr
}

func (e *Emulator) insertLine(n int) {
	if n < 1 {
		n = 1
	}
	e.active().insertLines(e.cursor.Y, e.scrollBottom, n, e.attr)
}

func (e *Emulator) deleteLine(n int) {
	if n < 1 {
		n = 1
	}
	e.active().deleteLines(e.cursor.Y, e.scrollBottom, n, e.attr)
}

func (e *Emulator) scrollUp(n int) {
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		e.active().scrollRegion(e.scrollTop, e.scrollBottom, e.historyLimit, e.attr)
	}
}

func (e *Emulator) scrollDown(n int) {
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		e.active().scrollRegionDown(e.scrollTop, e.scrollBottom, e.attr)
	}
}

func (e *Emulator) mapCharset(r rune) rune {
	lineDraw := e.g0LineDraw
	if e.activeSet == 1 {
		lineDraw = e.g1LineDraw
	}
	if !lineDraw {
		return r
	}
	switch r {
	case '`':
		return '◆'
	case 'a':
		return '▒'
	case 'f':
		return '°'
	case 'g':
		return '±'
	case 'j':
		return '┘'
	case 'k':
		return '┐'
	case 'l':
		return '┌'
	case 'm':
		return '└'
	case 'n':
		return '┼'
	case 'q':
		return '─'
	case 't':
		return '├'
	case 'u':
		return '┤'
	case 'v':
		return '┴'
	case 'w':
		return '┬'
	case 'x':
		return '│'
	}
	return r
}

func newScreenBuffer(size Size) *screenBuffer {
	b := &screenBuffer{lines: make([][]Cell, size.Rows)}
	for i := range b.lines {
		b.lines[i] = blankLine(size.Cols)
	}
	return b
}

func (b *screenBuffer) scroll(historyLimit int) {
	if len(b.lines) == 0 {
		return
	}
	b.scrollRegion(0, len(b.lines)-1, historyLimit, Attr{FG: -1, BG: -1})
}

func (b *screenBuffer) scrollRegion(top, bottom, historyLimit int, attr Attr) {
	if len(b.lines) == 0 {
		return
	}
	if top < 0 {
		top = 0
	}
	if bottom >= len(b.lines) {
		bottom = len(b.lines) - 1
	}
	if top > bottom {
		return
	}
	if historyLimit > 0 {
		if top == 0 && bottom == len(b.lines)-1 {
			b.hist = append(b.hist, cloneLine(b.lines[0]))
			b.trimHistory(historyLimit)
		}
	}
	copy(b.lines[top:bottom], b.lines[top+1:bottom+1])
	b.lines[bottom] = blankLineWithAttr(len(b.lines[bottom]), attr)
}

func (b *screenBuffer) scrollRegionDown(top, bottom int, attr Attr) {
	if len(b.lines) == 0 {
		return
	}
	if top < 0 {
		top = 0
	}
	if bottom >= len(b.lines) {
		bottom = len(b.lines) - 1
	}
	if top > bottom {
		return
	}
	copy(b.lines[top+1:bottom+1], b.lines[top:bottom])
	b.lines[top] = blankLineWithAttr(len(b.lines[top]), attr)
}

func (b *screenBuffer) insertLines(y, bottom, n int, attr Attr) {
	if len(b.lines) == 0 || y < 0 || y >= len(b.lines) {
		return
	}
	if bottom >= len(b.lines) {
		bottom = len(b.lines) - 1
	}
	if y > bottom {
		bottom = len(b.lines) - 1
	}
	if n > bottom-y+1 {
		n = bottom - y + 1
	}
	copy(b.lines[y+n:bottom+1], b.lines[y:bottom+1-n])
	for i := 0; i < n; i++ {
		b.lines[y+i] = blankLineWithAttr(len(b.lines[y+i]), attr)
	}
}

func (b *screenBuffer) deleteLines(y, bottom, n int, attr Attr) {
	if len(b.lines) == 0 || y < 0 || y >= len(b.lines) {
		return
	}
	if bottom >= len(b.lines) {
		bottom = len(b.lines) - 1
	}
	if y > bottom {
		bottom = len(b.lines) - 1
	}
	if n > bottom-y+1 {
		n = bottom - y + 1
	}
	copy(b.lines[y:bottom+1-n], b.lines[y+n:bottom+1])
	for i := bottom - n + 1; i <= bottom; i++ {
		b.lines[i] = blankLineWithAttr(len(b.lines[i]), attr)
	}
}

func (b *screenBuffer) resize(size Size, historyLimit int) {
	old := b.lines
	lines := make([][]Cell, size.Rows)
	for y := 0; y < size.Rows; y++ {
		lines[y] = blankLine(size.Cols)
		if y < len(old) {
			copy(lines[y], old[y])
		}
	}
	if len(old) > size.Rows && historyLimit > 0 {
		for _, line := range old[:len(old)-size.Rows] {
			b.hist = append(b.hist, cloneLine(line))
		}
	}
	b.lines = lines
	b.trimHistory(historyLimit)
}

func (b *screenBuffer) trimHistory(limit int) {
	if limit <= 0 {
		b.hist = nil
		return
	}
	if len(b.hist) > limit {
		b.hist = append([][]Cell(nil), b.hist[len(b.hist)-limit:]...)
	}
}

func normalizeSize(size Size) Size {
	if size.Cols <= 0 {
		size.Cols = 80
	}
	if size.Rows <= 0 {
		size.Rows = 24
	}
	return size
}

func clampCursor(c Cursor, size Size) Cursor {
	if c.X < 0 {
		c.X = 0
	}
	if c.Y < 0 {
		c.Y = 0
	}
	if c.X >= size.Cols {
		c.X = size.Cols - 1
	}
	if c.Y >= size.Rows {
		c.Y = size.Rows - 1
	}
	return c
}

func blankLine(cols int) []Cell {
	return blankLineWithAttr(cols, Attr{FG: -1, BG: -1})
}

func blankLineWithAttr(cols int, attr Attr) []Cell {
	line := make([]Cell, cols)
	for i := range line {
		line[i] = Cell{R: ' ', Attr: attr}
	}
	return line
}

func cloneLine(line []Cell) []Cell {
	out := make([]Cell, len(line))
	copy(out, line)
	return out
}

func clearRange(line []Cell, start, end int, attr Attr) {
	if start < 0 {
		start = 0
	}
	if end > len(line) {
		end = len(line)
	}
	for i := start; i < end; i++ {
		line[i] = Cell{R: ' ', Attr: attr}
	}
}

func insertCells(line []Cell, start, n int, attr Attr) {
	if start < 0 {
		start = 0
	}
	if start >= len(line) {
		return
	}
	if n > len(line)-start {
		n = len(line) - start
	}
	copy(line[start+n:], line[start:len(line)-n])
	clearRange(line, start, start+n, attr)
}

func cellsString(line []Cell) string {
	var b strings.Builder
	for _, cell := range line {
		if cell.R == 0 {
			continue
		}
		r := cell.R
		if r == 0 {
			r = ' '
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ")
}

func cellsFromString(s string, cols int, attr Attr) []Cell {
	line := blankLine(cols)
	x := 0
	for _, r := range s {
		if x >= cols {
			break
		}
		line[x] = Cell{R: r, Attr: attr}
		x += runeWidth(r)
	}
	return line
}

func parseCSIParams(body string) []int {
	if body == "" {
		return nil
	}
	body = strings.ReplaceAll(body, ":", ";")
	fields := strings.Split(body, ";")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			out = append(out, 0)
			continue
		}
		n := 0
		for _, r := range f {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out
}

func extendedColor(params []int, index *int) (int, bool) {
	if *index+1 >= len(params) {
		return 0, false
	}
	mode := params[*index+1]
	switch mode {
	case 5:
		if *index+2 >= len(params) {
			return 0, false
		}
		color := params[*index+2]
		if color < 0 {
			color = 0
		}
		if color > 255 {
			color = 255
		}
		*index += 2
		return color, true
	case 2:
		if *index+4 >= len(params) {
			return 0, false
		}
		offset := 2
		if *index+5 < len(params) && params[*index+2] == 0 {
			offset = 3
		}
		r := clampColorComponent(params[*index+offset])
		g := clampColorComponent(params[*index+offset+1])
		b := clampColorComponent(params[*index+offset+2])
		*index += offset + 2
		return trueColorBase | r<<16 | g<<8 | b, true
	default:
		return 0, false
	}
}

func clampColorComponent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func paramDefault(params []int, index int, fallback int) int {
	if index >= len(params) || params[index] == 0 {
		return fallback
	}
	return params[index]
}

func runeWidth(r rune) int {
	if r == 0 || unicode.Is(unicode.Mn, r) {
		return 0
	}
	if r >= 0x1100 &&
		(r <= 0x115f ||
			r == 0x2329 || r == 0x232a ||
			(r >= 0x2e80 && r <= 0xa4cf) ||
			(r >= 0xac00 && r <= 0xd7a3) ||
			(r >= 0xf900 && r <= 0xfaff) ||
			(r >= 0xfe10 && r <= 0xfe19) ||
			(r >= 0xfe30 && r <= 0xfe6f) ||
			(r >= 0xff00 && r <= 0xff60) ||
			(r >= 0xffe0 && r <= 0xffe6)) {
		return 2
	}
	return 1
}
