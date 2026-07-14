package ptyterm

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tinyrange/vmsh/internal/termui/asciicast"
)

type Recorder interface {
	Output([]byte)
	Metadata(string, map[string]any)
}

type Driver struct {
	session *Session
	delay   time.Duration
}

type Input []byte

func NewDriver(session *Session) *Driver {
	return &Driver{session: session}
}

func NewAsciicast(path string, size Size) (*asciicast.Recorder, error) {
	size = normalizeSize(size)
	return asciicast.Create(path, size.Cols, size.Rows)
}

func (d *Driver) SetDelay(delay time.Duration) {
	d.delay = delay
}

func (d *Driver) Send(inputs ...Input) error {
	for _, input := range inputs {
		if len(input) == 0 {
			continue
		}
		if _, err := d.session.Write(input); err != nil {
			return err
		}
		if d.delay > 0 {
			time.Sleep(d.delay)
		}
	}
	return nil
}

func (d *Driver) Text(text string) error {
	return d.Send(Input(text))
}

// Type writes text one rune at a time, following the same input path as
// individual keyboard events. SetDelay can be used to pace the keystrokes.
func (d *Driver) Type(text string) error {
	for _, r := range text {
		if err := d.Send(Input(string(r))); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) Key(key Key) error {
	return d.Send(key.Input())
}

func (d *Driver) Chord(chord string) error {
	input, err := ParseChord(chord)
	if err != nil {
		return err
	}
	return d.Send(input)
}

func (d *Driver) Wait(ctx context.Context, predicate func(Snapshot) bool) (Snapshot, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snap := d.session.Snapshot()
		if predicate(snap) {
			return snap, nil
		}
		if snap.Exited {
			return snap, fmt.Errorf("process exited before terminal condition (code %d): %s", snap.ExitCode, snap.ExitError)
		}
		select {
		case <-ctx.Done():
			return snap, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *Driver) WaitLine(ctx context.Context, text string) (Snapshot, error) {
	return d.Wait(ctx, func(snap Snapshot) bool {
		for _, line := range append(append([]string{}, snap.History...), snap.Lines...) {
			if strings.Contains(line, text) {
				return true
			}
		}
		return false
	})
}

func (d *Driver) WaitLineExact(ctx context.Context, text string) (Snapshot, error) {
	return d.Wait(ctx, func(snap Snapshot) bool {
		for _, line := range append(append([]string{}, snap.History...), snap.Lines...) {
			if line == text {
				return true
			}
		}
		return false
	})
}

func (d *Driver) WaitTitle(ctx context.Context, title string) (Snapshot, error) {
	return d.Wait(ctx, func(snap Snapshot) bool {
		return snap.Title == title
	})
}

func (d *Driver) WaitRaw(ctx context.Context, sequence []byte) (Snapshot, error) {
	return d.Wait(ctx, func(snap Snapshot) bool {
		return bytes.Contains(snap.RawTail, sequence)
	})
}

func (d *Driver) WaitRawAfter(ctx context.Context, position int64, sequence []byte) (Snapshot, error) {
	return d.Wait(ctx, func(snap Snapshot) bool {
		tailStart := snap.BytesRead - int64(len(snap.RawTail))
		start := position - tailStart
		if start < 0 {
			start = 0
		}
		if start > int64(len(snap.RawTail)) {
			return false
		}
		return bytes.Contains(snap.RawTail[start:], sequence)
	})
}

func (d *Driver) WaitExit(ctx context.Context) (Snapshot, error) {
	return d.Wait(ctx, func(snap Snapshot) bool {
		return snap.Exited
	})
}

type Key string

const (
	KeyEnter     Key = "\r"
	KeyEscape    Key = "\x1b"
	KeyTab       Key = "\t"
	KeyBackspace Key = "\x7f"
	KeyUp        Key = "\x1b[A"
	KeyDown      Key = "\x1b[B"
	KeyRight     Key = "\x1b[C"
	KeyLeft      Key = "\x1b[D"
	KeyHome      Key = "\x1b[H"
	KeyEnd       Key = "\x1b[F"
	KeyDelete    Key = "\x1b[3~"
	KeyPageUp    Key = "\x1b[5~"
	KeyPageDown  Key = "\x1b[6~"
)

func (k Key) Input() Input {
	return Input(k)
}

func Ctrl(r rune) Input {
	if r >= 'a' && r <= 'z' {
		return Input{byte(r - 'a' + 1)}
	}
	if r >= 'A' && r <= 'Z' {
		return Input{byte(r - 'A' + 1)}
	}
	switch r {
	case '[':
		return Input{0x1b}
	case '\\':
		return Input{0x1c}
	case ']':
		return Input{0x1d}
	case '^':
		return Input{0x1e}
	case '_':
		return Input{0x1f}
	}
	return nil
}

func Alt(r rune) Input {
	return append(Input{0x1b}, []byte(string(r))...)
}

func Function(n int) Input {
	switch n {
	case 1:
		return Input("\x1bOP")
	case 2:
		return Input("\x1bOQ")
	case 3:
		return Input("\x1bOR")
	case 4:
		return Input("\x1bOS")
	case 5:
		return Input("\x1b[15~")
	case 6:
		return Input("\x1b[17~")
	case 7:
		return Input("\x1b[18~")
	case 8:
		return Input("\x1b[19~")
	case 9:
		return Input("\x1b[20~")
	case 10:
		return Input("\x1b[21~")
	case 11:
		return Input("\x1b[23~")
	case 12:
		return Input("\x1b[24~")
	default:
		return nil
	}
}

func ParseChord(chord string) (Input, error) {
	parts := strings.Split(strings.TrimSpace(chord), "+")
	if len(parts) == 0 || parts[0] == "" {
		return nil, fmt.Errorf("empty chord")
	}
	if len(parts) == 1 {
		key := strings.ToLower(parts[0])
		switch key {
		case "enter":
			return KeyEnter.Input(), nil
		case "esc", "escape":
			return KeyEscape.Input(), nil
		case "tab":
			return KeyTab.Input(), nil
		case "backspace":
			return KeyBackspace.Input(), nil
		case "up":
			return KeyUp.Input(), nil
		case "down":
			return KeyDown.Input(), nil
		case "left":
			return KeyLeft.Input(), nil
		case "right":
			return KeyRight.Input(), nil
		}
		if strings.HasPrefix(key, "f") {
			var n int
			if _, err := fmt.Sscanf(key, "f%d", &n); err == nil {
				if input := Function(n); input != nil {
					return input, nil
				}
			}
		}
		return Input(parts[0]), nil
	}
	if len(parts) == 2 && strings.EqualFold(parts[0], "ctrl") && len([]rune(parts[1])) == 1 {
		if input := Ctrl([]rune(parts[1])[0]); input != nil {
			return input, nil
		}
	}
	if len(parts) == 2 && strings.EqualFold(parts[0], "alt") && len([]rune(parts[1])) == 1 {
		return Alt([]rune(parts[1])[0]), nil
	}
	return nil, fmt.Errorf("unsupported chord %q", chord)
}
