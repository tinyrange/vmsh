package asciicast

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

type Recorder struct {
	mu      sync.Mutex
	file    *os.File
	started time.Time
	closed  bool
}

type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Env       map[string]string `json:"env,omitempty"`
	Termui    map[string]any    `json:"termui,omitempty"`
}

func Create(path string, width, height int) (*Recorder, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	header := Header{
		Version:   2,
		Width:     width,
		Height:    height,
		Timestamp: now.Unix(),
		Env: map[string]string{
			"SHELL": os.Getenv("SHELL"),
			"TERM":  os.Getenv("TERM"),
		},
		Termui: map[string]any{
			"metadata_events": true,
		},
	}
	if err := json.NewEncoder(file).Encode(header); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Recorder{file: file, started: now}, nil
}

func (r *Recorder) Writer(dst io.Writer) io.Writer {
	return writer{rec: r, dst: dst}
}

func (r *Recorder) Metadata(name string, fields map[string]any) {
	if r == nil {
		return
	}
	payload := map[string]any{
		"name":   name,
		"fields": fields,
	}
	r.event("m", payload)
}

func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.file.Close()
}

func (r *Recorder) event(kind string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	event := []any{time.Since(r.started).Seconds(), kind, data}
	_ = json.NewEncoder(r.file).Encode(event)
}

type writer struct {
	rec *Recorder
	dst io.Writer
}

func (w writer) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.rec.event("o", string(p[:n]))
	}
	return n, err
}
