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
	seq := e.readAvailable(32, 2*time.Millisecond)
	switch {
	case seq == "[D" || seq == "OD":
		return keyEvent{key: keyLeft}, true, nil
	case seq == "[C" || seq == "OC":
		return keyEvent{key: keyRight}, true, nil
	case seq == "[A" || seq == "OA":
		return keyEvent{key: keyUp}, true, nil
	case seq == "[B" || seq == "OB":
		return keyEvent{key: keyDown}, true, nil
	case seq == "[H" || seq == "OH" || seq == "[1~":
		return keyEvent{key: keyHome}, true, nil
	case seq == "[F" || seq == "OF" || seq == "[4~":
		return keyEvent{key: keyEnd}, true, nil
	case seq == "[3~":
		return keyEvent{key: keyDelete}, true, nil
	case seq == "[5~":
		return keyEvent{key: keyPageUp}, true, nil
	case seq == "[6~":
		return keyEvent{key: keyPageDown}, true, nil
	case strings.HasPrefix(seq, "[200~"):
		text, err := e.readUntilPasteEnd(ctx, strings.TrimPrefix(seq, "[200~"))
		return keyEvent{key: keyPaste, text: text}, true, err
	case seq == "[201~":
		return keyEvent{key: keyUnknown}, true, nil
	default:
		return keyEvent{key: keyEscape}, true, nil
	}
}

func (e *Editor) readAvailable(max int, quiet time.Duration) string {
	var out strings.Builder
	deadline := time.Now().Add(quiet)
	for out.Len() < max && time.Now().Before(deadline) {
		b, n, err := e.readByte()
		if n == 1 {
			out.WriteByte(b)
			deadline = time.Now().Add(quiet)
			continue
		}
		if err != nil && !isAgain(err) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return out.String()
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
