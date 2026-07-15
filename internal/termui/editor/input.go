package editor

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/tinyrange/vmsh/internal/termui/terminal"
)

type key int

const (
	keyRune key = iota
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyTab
	keyEscape
	keyPageUp
	keyPageDown
	keyCtrlC
	keyCtrlD
	keyCtrlG
	keyCtrlL
	keyCtrlR
	keyPaste
	keyUnknown
)

type keyEvent struct {
	key  key
	r    rune
	text string
}

func (e *Editor) readKey(ctx context.Context) (keyEvent, error) {
	for {
		ev, ok, err := e.pollKeyContext(ctx)
		if ok || err != nil && !isAgain(err) {
			return ev, err
		}
		select {
		case <-ctx.Done():
			return keyEvent{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (e *Editor) pollKey() (keyEvent, bool, error) {
	return e.pollKeyContext(context.Background())
}

func (e *Editor) pollKeyContext(ctx context.Context) (keyEvent, bool, error) {
	b, n, err := e.readByte()
	if n == 0 {
		if err == nil {
			return keyEvent{}, false, syscall.EAGAIN
		}
		if errors.Is(err, io.EOF) {
			if e.caps.Mode != terminal.ModeNonInteractive {
				return keyEvent{}, false, syscall.EAGAIN
			}
			return keyEvent{}, false, err
		}
		return keyEvent{}, false, err
	}
	return e.parseFirstByte(ctx, b)
}

func (e *Editor) parseFirstByte(ctx context.Context, b byte) (keyEvent, bool, error) {
	switch b {
	case '\r', '\n':
		return keyEvent{key: keyEnter}, true, nil
	case 0x03:
		return keyEvent{key: keyCtrlC}, true, nil
	case 0x04:
		return keyEvent{key: keyCtrlD}, true, nil
	case 0x07:
		return keyEvent{key: keyCtrlG}, true, nil
	case 0x0c:
		return keyEvent{key: keyCtrlL}, true, nil
	case 0x12:
		return keyEvent{key: keyCtrlR}, true, nil
	case '\t':
		return keyEvent{key: keyTab}, true, nil
	case 0x7f, 0x08:
		return keyEvent{key: keyBackspace}, true, nil
	case 0x1b:
		return e.readEscape(ctx)
	default:
		if b < 0x20 {
			return keyEvent{key: keyUnknown}, true, nil
		}
		return e.readRune(b)
	}
}

func (e *Editor) readRune(first byte) (keyEvent, bool, error) {
	buf := []byte{first}
	if first < utf8.RuneSelf {
		return keyEvent{key: keyRune, r: rune(first)}, true, nil
	}
	for len(buf) < utf8.UTFMax && !utf8.FullRune(buf) {
		b, n, err := e.readByte()
		if n == 1 {
			buf = append(buf, b)
			continue
		}
		if err != nil && !isAgain(err) {
			return keyEvent{}, false, err
		}
		time.Sleep(time.Millisecond)
	}
	r, _ := utf8.DecodeRune(buf)
	if r == utf8.RuneError {
		return keyEvent{key: keyUnknown}, true, nil
	}
	return keyEvent{key: keyRune, r: r}, true, nil
}

func (e *Editor) readEscape(ctx context.Context) (keyEvent, bool, error) {
	first, ok, err := e.readEscapeByte(ctx)
	if err != nil {
		return keyEvent{}, false, err
	}
	if !ok {
		return keyEvent{key: keyEscape}, true, nil
	}
	if first != '[' && first != 'O' {
		e.unreadBytes([]byte{first})
		return keyEvent{key: keyEscape}, true, nil
	}

	seq := []byte{first}
	for len(seq) < 32 {
		b, ok, err := e.readEscapeByte(ctx)
		if err != nil {
			return keyEvent{}, false, err
		}
		if !ok {
			return keyEvent{key: keyEscape}, true, nil
		}
		seq = append(seq, b)
		if first == 'O' || b >= 0x40 && b <= 0x7e {
			break
		}
	}

	sequence := string(seq)
	if sequence == "[200~" {
		text, err := e.readUntilPasteEnd(ctx, "")
		return keyEvent{key: keyPaste, text: text}, true, err
	}
	for _, binding := range []struct {
		key       key
		sequences []string
	}{
		{key: keyLeft, sequences: []string{"[D", "OD"}},
		{key: keyRight, sequences: []string{"[C", "OC"}},
		{key: keyUp, sequences: []string{"[A", "OA"}},
		{key: keyDown, sequences: []string{"[B", "OB"}},
		{key: keyHome, sequences: []string{"[H", "OH", "[1~"}},
		{key: keyEnd, sequences: []string{"[F", "OF", "[4~"}},
		{key: keyDelete, sequences: []string{"[3~"}},
		{key: keyPageUp, sequences: []string{"[5~"}},
		{key: keyPageDown, sequences: []string{"[6~"}},
		{key: keyUnknown, sequences: []string{"[201~"}},
	} {
		for _, candidate := range binding.sequences {
			if sequence == candidate {
				return keyEvent{key: binding.key}, true, nil
			}
		}
	}
	return keyEvent{key: keyEscape}, true, nil
}

func (e *Editor) readEscapeByte(ctx context.Context) (byte, bool, error) {
	deadline := time.NewTimer(25 * time.Millisecond)
	defer deadline.Stop()
	for {
		b, n, err := e.readByte()
		if n == 1 {
			return b, true, nil
		}
		if err != nil && !isAgain(err) && !errors.Is(err, io.EOF) {
			return 0, false, err
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-deadline.C:
			return 0, false, nil
		case <-time.After(time.Millisecond):
		}
	}
}

func (e *Editor) readUntilPasteEnd(ctx context.Context, initial string) (string, error) {
	var out strings.Builder
	out.WriteString(initial)
	for {
		s := out.String()
		if idx := strings.Index(s, "\x1b[201~"); idx >= 0 {
			if tail := s[idx+len("\x1b[201~"):]; tail != "" {
				e.unreadBytes([]byte(tail))
			}
			return s[:idx], nil
		}
		b, n, err := e.readByte()
		if n == 1 {
			out.WriteByte(b)
			continue
		}
		if err != nil && !isAgain(err) {
			return out.String(), err
		}
		select {
		case <-ctx.Done():
			return out.String(), ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (e *Editor) readByte() (byte, int, error) {
	if len(e.pending) > 0 {
		b := e.pending[0]
		e.pending = e.pending[1:]
		return b, 1, nil
	}
	var buf [1]byte
	n, err := e.in.Read(buf[:])
	if n == 1 {
		return buf[0], 1, err
	}
	return 0, n, err
}

func (e *Editor) unreadBytes(bs []byte) {
	if len(bs) == 0 {
		return
	}
	next := append([]byte(nil), bs...)
	e.pending = append(next, e.pending...)
}

func (e *Editor) queueEvent(ev keyEvent) {
	switch ev.key {
	case keyRune:
		e.queued = append(e.queued, ev.r)
	case keyPaste:
		e.queued = append(e.queued, []rune(normalizePaste(ev.text))...)
	case keyBackspace:
		if len(e.queued) > 0 {
			e.queued = e.queued[:len(e.queued)-1]
		}
	case keyCtrlD:
	case keyEnter:
		e.queued = append(e.queued, '\n')
	}
}
