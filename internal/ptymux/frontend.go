package ptymux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tinyrange/vmsh/internal/ptyterm"
	"github.com/tinyrange/vmsh/internal/termui/terminal"
)

const (
	PrefixKey     = byte(0x07)
	trueColorBase = 1 << 24
)

type Session interface {
	Write([]byte) (int, error)
	Resize(ptyterm.Size) error
	Snapshot() ptyterm.Snapshot
	Close() error
}

type Starter func(context.Context, ptyterm.Options) (Session, error)

type Options struct {
	Command      []string
	Dir          string
	Env          []string
	HistoryLimit int
	Size         ptyterm.Size
	Stdin        *os.File
	Stdout       *os.File
	Starter      Starter
}

type Frontend struct {
	command      []string
	dir          string
	env          []string
	historyLimit int
	size         ptyterm.Size
	starter      Starter
	panes        []*Pane
	active       int
	prefix       bool
	message      string
}

type Pane struct {
	ID      int
	Session Session
}

type Action int

const (
	ActionNone Action = iota
	ActionQuit
	ActionCreate
	ActionSwitch
)

type Event struct {
	Action Action
	Pane   int
}

func New(ctx context.Context, opts Options) (*Frontend, error) {
	f := &Frontend{
		command:      defaultCommand(opts.Command),
		dir:          opts.Dir,
		env:          opts.Env,
		historyLimit: opts.HistoryLimit,
		size:         normalizeSize(opts.Size),
		starter:      opts.Starter,
	}
	if f.starter == nil {
		f.starter = startSession
	}
	if err := f.CreatePane(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Frontend) CreatePane(ctx context.Context) error {
	session, err := f.starter(ctx, ptyterm.Options{
		Command:      append([]string(nil), f.command...),
		Dir:          f.dir,
		Env:          f.env,
		Size:         f.paneSize(),
		HistoryLimit: f.historyLimit,
	})
	if err != nil {
		return err
	}
	pane := &Pane{ID: len(f.panes), Session: session}
	f.panes = append(f.panes, pane)
	f.active = len(f.panes) - 1
	f.message = "created pane " + strconv.Itoa(pane.ID)
	return nil
}

func (f *Frontend) Close() error {
	var err error
	for _, pane := range f.panes {
		if pane.Session == nil {
			continue
		}
		if closeErr := pane.Session.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

func (f *Frontend) ActivePane() int {
	return f.active
}

func (f *Frontend) PaneCount() int {
	return len(f.panes)
}

func (f *Frontend) HandleInput(ctx context.Context, data []byte) (Event, error) {
	event := Event{}
	for _, b := range data {
		next, err := f.handleByte(ctx, b)
		if err != nil {
			return event, err
		}
		if next.Action != ActionNone {
			event = next
		}
	}
	return event, nil
}

func (f *Frontend) Resize(size ptyterm.Size) {
	f.size = normalizeSize(size)
	for _, pane := range f.panes {
		if pane.Session != nil {
			_ = pane.Session.Resize(f.paneSize())
		}
	}
}

func (f *Frontend) ReapExited() Event {
	if len(f.panes) == 0 {
		return Event{Action: ActionQuit, Pane: -1}
	}
	oldActive := f.active
	next := f.panes[:0]
	removedActive := false
	removed := 0
	for i, pane := range f.panes {
		exited := pane == nil || pane.Session == nil || pane.Session.Snapshot().Exited
		if !exited {
			next = append(next, pane)
			continue
		}
		removed++
		if i == oldActive {
			removedActive = true
		}
		if pane != nil && pane.Session != nil {
			_ = pane.Session.Close()
		}
	}
	if removed == 0 {
		return Event{Action: ActionNone, Pane: f.active}
	}
	f.panes = next
	if len(f.panes) == 0 {
		f.active = -1
		f.message = "pane exited"
		return Event{Action: ActionQuit, Pane: -1}
	}
	if f.active >= len(f.panes) {
		f.active = len(f.panes) - 1
	}
	if f.active < 0 {
		f.active = 0
	}
	if removedActive {
		f.message = "pane exited; switched to " + strconv.Itoa(f.active)
		return Event{Action: ActionSwitch, Pane: f.active}
	}
	f.message = "pane exited"
	return Event{Action: ActionNone, Pane: f.active}
}

func (f *Frontend) Render() string {
	size := normalizeSize(f.size)
	paneRows := size.Rows - 1
	if paneRows < 1 {
		paneRows = 1
	}
	var snap ptyterm.Snapshot
	if len(f.panes) != 0 && f.active >= 0 && f.active < len(f.panes) {
		snap = f.panes[f.active].Session.Snapshot()
	}
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	for y := 0; y < paneRows; y++ {
		b.WriteString(cursorPosition(y+1, 1))
		b.WriteString(renderSnapshotLine(snap, y, size.Cols))
	}
	b.WriteString("\x1b[0m")
	b.WriteString(cursorPosition(size.Rows, 1))
	b.WriteString(f.statusLine(size.Cols))
	cursor := clampCursorForRender(snap.Cursor, size.Cols, paneRows)
	b.WriteString("\x1b[0m")
	if snap.CursorVisible {
		b.WriteString("\x1b[?25h")
		b.WriteString(cursorPosition(cursor.Y+1, cursor.X+1))
	} else {
		b.WriteString("\x1b[?25l")
	}
	return b.String()
}

func (f *Frontend) handleByte(ctx context.Context, b byte) (Event, error) {
	if f.prefix {
		f.prefix = false
		switch {
		case b == 'q':
			f.message = "exit"
			return Event{Action: ActionQuit, Pane: f.active}, nil
		case b == 'c':
			if err := f.CreatePane(ctx); err != nil {
				return Event{}, err
			}
			return Event{Action: ActionCreate, Pane: f.active}, nil
		case b >= '0' && b <= '9':
			index := int(b - '0')
			if index >= 0 && index < len(f.panes) {
				f.active = index
				f.message = "pane " + strconv.Itoa(index)
				return Event{Action: ActionSwitch, Pane: index}, nil
			}
			f.message = "no pane " + strconv.Itoa(index)
			return Event{Action: ActionNone, Pane: f.active}, nil
		case b == PrefixKey:
			return f.writeActive([]byte{PrefixKey})
		default:
			f.message = "unknown Ctrl-G " + string([]byte{b})
			return Event{Action: ActionNone, Pane: f.active}, nil
		}
	}
	if b == PrefixKey {
		f.prefix = true
		f.message = "prefix"
		return Event{Action: ActionNone, Pane: f.active}, nil
	}
	return f.writeActive([]byte{b})
}

func (f *Frontend) writeActive(data []byte) (Event, error) {
	if len(f.panes) == 0 || f.active < 0 || f.active >= len(f.panes) {
		return Event{}, nil
	}
	_, err := f.panes[f.active].Session.Write(data)
	return Event{Action: ActionNone, Pane: f.active}, err
}

func (f *Frontend) statusLine(cols int) string {
	parts := make([]string, 0, len(f.panes)+2)
	for i := range f.panes {
		label := strconv.Itoa(i)
		if i == f.active {
			label = "[" + label + "]"
		}
		parts = append(parts, label)
	}
	right := "Ctrl-G q exit  Ctrl-G c create  Ctrl-G <num> switch"
	if f.prefix {
		right = "Ctrl-G"
	} else if f.message != "" {
		right = f.message + " | " + right
	}
	text := " vmsh " + strings.Join(parts, " ") + " "
	if len(text)+len(right) < cols {
		text += strings.Repeat(" ", cols-len(text)-len(right)) + right
	} else {
		text += right
	}
	return "\x1b[7m" + padLine(fitLine(text, cols), cols) + "\x1b[27m"
}

func (f *Frontend) paneSize() ptyterm.Size {
	size := normalizeSize(f.size)
	if size.Rows > 1 {
		size.Rows--
	}
	return size
}

func Run(ctx context.Context, opts Options) error {
	in := opts.Stdin
	out := opts.Stdout
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if !terminal.IsTerminal(in) || !terminal.IsTerminal(out) {
		return fmt.Errorf("ptymux frontend requires an interactive terminal")
	}
	cols, rows, err := terminal.Size(out)
	if err == nil {
		opts.Size = ptyterm.Size{Cols: cols, Rows: rows}
	}
	restoreIn, err := terminal.MakeRaw(in)
	if err != nil {
		return err
	}
	defer restoreIn()
	restoreOut := terminal.PrepareOutput(out)
	defer restoreOut()

	f, err := New(ctx, opts)
	if err != nil {
		return err
	}

	_, _ = io.WriteString(out, "\x1b[?1049h\x1b[?25h\x1b[2J")
	defer io.WriteString(out, "\x1b[?25h\x1b[?1049l")
	defer f.Close()

	ticker := time.NewTicker(33 * time.Millisecond)
	defer ticker.Stop()
	resizeTicker := time.NewTicker(500 * time.Millisecond)
	defer resizeTicker.Stop()
	if event := f.ReapExited(); event.Action == ActionQuit {
		return nil
	}
	_, _ = io.WriteString(out, f.Render())
	buf := make([]byte, 1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if event := f.ReapExited(); event.Action == ActionQuit {
				return nil
			}
			_, _ = io.WriteString(out, f.Render())
		case <-resizeTicker.C:
			if cols, rows, err := terminal.Size(out); err == nil && (cols != f.size.Cols || rows != f.size.Rows) {
				f.Resize(ptyterm.Size{Cols: cols, Rows: rows})
				if event := f.ReapExited(); event.Action == ActionQuit {
					return nil
				}
				_, _ = io.WriteString(out, "\x1b[2J"+f.Render())
			}
		default:
		}
		n, err := in.Read(buf)
		if n > 0 {
			event, handleErr := f.HandleInput(ctx, buf[:n])
			if handleErr != nil {
				return handleErr
			}
			if event.Action == ActionQuit {
				return nil
			}
			if event := f.ReapExited(); event.Action == ActionQuit {
				return nil
			}
			_, _ = io.WriteString(out, f.Render())
			continue
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				if event := f.ReapExited(); event.Action == ActionQuit {
					return nil
				}
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func defaultCommand(command []string) []string {
	if len(command) != 0 {
		return append([]string(nil), command...)
	}
	if runtime.GOOS == "windows" {
		if shell := strings.TrimSpace(os.Getenv("COMSPEC")); shell != "" {
			return []string{shell}
		}
		return []string{"cmd.exe"}
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		return []string{bash, "--noprofile", "--norc", "-i"}
	}
	if shell, err := exec.LookPath("sh"); err == nil {
		return []string{shell, "-i"}
	}
	return []string{"/bin/sh", "-i"}
}

func startSession(ctx context.Context, opts ptyterm.Options) (Session, error) {
	return ptyterm.Start(ctx, opts)
}

func normalizeSize(size ptyterm.Size) ptyterm.Size {
	if size.Cols <= 0 {
		size.Cols = 80
	}
	if size.Rows <= 1 {
		size.Rows = 24
	}
	return size
}

func fitLine(line string, cols int) string {
	runes := []rune(line)
	if len(runes) > cols {
		runes = runes[:cols]
	}
	return string(runes)
}

func padLine(line string, cols int) string {
	width := len([]rune(line))
	if width >= cols {
		return line
	}
	return line + strings.Repeat(" ", cols-width)
}

func renderSnapshotLine(snap ptyterm.Snapshot, y int, cols int) string {
	if y >= 0 && y < len(snap.Cells) {
		return renderCells(snap.Cells[y], cols)
	}
	if y >= 0 && y < len(snap.Lines) {
		return padLine(fitLine(snap.Lines[y], cols), cols)
	}
	return strings.Repeat(" ", cols)
}

func renderCells(cells []ptyterm.Cell, cols int) string {
	var b strings.Builder
	attr := ptyterm.Attr{FG: -1, BG: -1}
	written := 0
	for x := 0; x < cols; x++ {
		cell := ptyterm.Cell{R: ' ', Attr: ptyterm.Attr{FG: -1, BG: -1}}
		if x < len(cells) {
			cell = cells[x]
		}
		if cell.R == 0 {
			written++
			continue
		}
		if cell.Attr != attr {
			b.WriteString(sgr(cell.Attr))
			attr = cell.Attr
		}
		b.WriteRune(cell.R)
		written++
	}
	if attr != (ptyterm.Attr{FG: -1, BG: -1}) {
		b.WriteString("\x1b[0m")
	}
	if written < cols {
		b.WriteString(strings.Repeat(" ", cols-written))
	}
	return b.String()
}

func sgr(attr ptyterm.Attr) string {
	codes := []string{"0"}
	if attr.Bold {
		codes = append(codes, "1")
	}
	if attr.Underline {
		codes = append(codes, "4")
	}
	if attr.Inverse {
		codes = append(codes, "7")
	}
	if attr.FG >= 0 {
		codes = append(codes, ansiColor(attr.FG, false))
	}
	if attr.BG >= 0 {
		codes = append(codes, ansiColor(attr.BG, true))
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

func ansiColor(color int, background bool) string {
	if color >= trueColorBase {
		prefix := "38"
		if background {
			prefix = "48"
		}
		rgb := color & (trueColorBase - 1)
		return prefix + ";2;" +
			strconv.Itoa((rgb>>16)&0xff) + ";" +
			strconv.Itoa((rgb>>8)&0xff) + ";" +
			strconv.Itoa(rgb&0xff)
	}
	if color >= 16 {
		prefix := "38"
		if background {
			prefix = "48"
		}
		return prefix + ";5;" + strconv.Itoa(color)
	}
	base := 30
	brightBase := 90
	if background {
		base = 40
		brightBase = 100
	}
	if color >= 8 && color <= 15 {
		return strconv.Itoa(brightBase + color - 8)
	}
	return strconv.Itoa(base + color%8)
}

func clampCursorForRender(cursor ptyterm.Cursor, cols int, rows int) ptyterm.Cursor {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	if cursor.X < 0 {
		cursor.X = 0
	}
	if cursor.X >= cols {
		cursor.X = cols - 1
	}
	if cursor.Y < 0 {
		cursor.Y = 0
	}
	if cursor.Y >= rows {
		cursor.Y = rows - 1
	}
	return cursor
}

func cursorPosition(row, col int) string {
	return "\x1b[" + strconv.Itoa(row) + ";" + strconv.Itoa(col) + "H"
}
