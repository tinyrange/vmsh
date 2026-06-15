package shell

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tinyrange/vmsh/internal/terminal"
)

type asciinemaRecorder struct {
	mu    sync.Mutex
	file  *os.File
	start time.Time
}

func newAsciinemaRecorder(path string, cols, rows int) (*asciinemaRecorder, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	rec := &asciinemaRecorder{file: file, start: time.Now()}
	header := map[string]any{
		"version":   2,
		"width":     cols,
		"height":    rows,
		"timestamp": rec.start.Unix(),
		"env": map[string]string{
			"TERM":  os.Getenv("TERM"),
			"SHELL": os.Getenv("SHELL"),
		},
	}
	if err := rec.writeJSONLine(header); err != nil {
		_ = file.Close()
		return nil, err
	}
	return rec, nil
}

func (r *asciinemaRecorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

func (r *asciinemaRecorder) recordOutput(data []byte) {
	r.recordEvent("o", data)
}

func (r *asciinemaRecorder) recordInput(data []byte) {
	r.recordEvent("i", data)
}

func (r *asciinemaRecorder) recordEvent(kind string, data []byte) {
	if r == nil || len(data) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.writeJSONLine([]any{time.Since(r.start).Seconds(), kind, string(data)})
}

func (r *asciinemaRecorder) writeJSONLine(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := r.file.Write(data); err != nil {
		return err
	}
	_, err = r.file.Write([]byte("\n"))
	return err
}

type recordingTerminalWriter struct {
	file     *os.File
	recorder *asciinemaRecorder
}

func newRecordingTerminalWriter(file *os.File, recorder *asciinemaRecorder) io.Writer {
	if recorder == nil {
		return file
	}
	return &recordingTerminalWriter{file: file, recorder: recorder}
}

func (w *recordingTerminalWriter) Write(data []byte) (int, error) {
	n, err := w.file.Write(data)
	if n > 0 {
		w.recorder.recordOutput(data[:n])
	}
	return n, err
}

func (w *recordingTerminalWriter) Fd() uintptr {
	return w.file.Fd()
}

func (w *recordingTerminalWriter) terminalFile() *os.File {
	return w.file
}

func terminalWriterFile(w io.Writer) (*os.File, bool) {
	switch out := w.(type) {
	case *os.File:
		return out, true
	case interface{ terminalFile() *os.File }:
		file := out.terminalFile()
		return file, file != nil
	default:
		return nil, false
	}
}

func terminalWriterRecorder(w io.Writer) *asciinemaRecorder {
	switch out := w.(type) {
	case *recordingTerminalWriter:
		return out.recorder
	default:
		return nil
	}
}

func terminalDisplayWriter(w io.Writer) io.Writer {
	file, ok := terminalWriterFile(w)
	if !ok || file == nil {
		return w
	}
	if _, _, err := terminal.Size(file); err != nil {
		return w
	}
	return &terminalNewlineWriter{w: w}
}

type terminalNewlineWriter struct {
	w      io.Writer
	lastCR bool
}

func (w *terminalNewlineWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	out := make([]byte, 0, len(data)+8)
	for _, b := range data {
		if b == '\n' && !w.lastCR {
			out = append(out, '\r')
		}
		out = append(out, b)
		w.lastCR = b == '\r'
		if b != '\r' {
			w.lastCR = false
		}
	}
	_, err := w.w.Write(out)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}
