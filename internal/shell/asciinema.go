package shell

import (
	"encoding/base64"
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
	raw   *rawSessionRecorder
	start time.Time
}

type rawSessionRecorder struct {
	file *os.File
}

func newAsciinemaRecorder(path, rawPath string, cols, rows int) (*asciinemaRecorder, error) {
	if path == "" && rawPath == "" {
		return nil, nil
	}
	rec := &asciinemaRecorder{start: time.Now()}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		rec.file = file
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
			_ = rec.Close()
			return nil, err
		}
	}
	if rawPath != "" {
		raw, err := newRawSessionRecorder(rawPath, rec.start, cols, rows)
		if err != nil {
			_ = rec.Close()
			return nil, err
		}
		rec.raw = raw
	}
	return rec, nil
}

func (r *asciinemaRecorder) Close() error {
	if r == nil {
		return nil
	}
	var err error
	if r.file != nil {
		err = r.file.Close()
		r.file = nil
	}
	if r.raw != nil {
		if rawErr := r.raw.Close(); err == nil {
			err = rawErr
		}
		r.raw = nil
	}
	return err
}

func (r *asciinemaRecorder) recordOutput(data []byte) {
	r.recordEvent("o", data)
}

func (r *asciinemaRecorder) recordInput(data []byte) {
	r.recordEvent("i", data)
}

func (r *asciinemaRecorder) recordResize(cols, rows int) {
	if r == nil || cols <= 0 || rows <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.raw != nil {
		_ = r.raw.recordResize(time.Since(r.start).Seconds(), cols, rows)
	}
}

func (r *asciinemaRecorder) recordEvent(kind string, data []byte) {
	if r == nil || len(data) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	elapsed := time.Since(r.start).Seconds()
	if r.file != nil {
		_ = r.writeJSONLine([]any{elapsed, kind, string(data)})
	}
	if r.raw != nil {
		rawKind := "output"
		if kind == "i" {
			rawKind = "input"
		}
		_ = r.raw.recordBytes(elapsed, rawKind, data)
	}
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

func newRawSessionRecorder(path string, started time.Time, cols, rows int) (*rawSessionRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	rec := &rawSessionRecorder{file: file}
	header := map[string]any{
		"kind":      "vmsh.raw_session",
		"version":   1,
		"cols":      cols,
		"rows":      rows,
		"timestamp": started.Unix(),
	}
	if err := rec.writeJSONLine(header); err != nil {
		_ = file.Close()
		return nil, err
	}
	return rec, nil
}

func (r *rawSessionRecorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func (r *rawSessionRecorder) recordBytes(elapsed float64, kind string, data []byte) error {
	return r.writeJSONLine(map[string]any{
		"t":    elapsed,
		"kind": kind,
		"data": base64.StdEncoding.EncodeToString(data),
	})
}

func (r *rawSessionRecorder) recordResize(elapsed float64, cols, rows int) error {
	return r.writeJSONLine(map[string]any{
		"t":    elapsed,
		"kind": "resize",
		"cols": cols,
		"rows": rows,
	})
}

func (r *rawSessionRecorder) writeJSONLine(value any) error {
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
