package asciicast

import (
	"encoding/json"
	"fmt"
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
	VMSHTour  *TourHeader       `json:"vmsh_tour,omitempty"`
}

// TourHeader identifies an enhanced vmsh tutorial cast. Section content is
// emitted as an extension event alongside a standard asciinema marker.
type TourHeader struct {
	Schema      int    `json:"schema"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	VMSHVersion string `json:"vmsh_version,omitempty"`
	Commit      string `json:"commit,omitempty"`
}

func Create(path string, width, height int) (*Recorder, error) {
	return CreateTour(path, width, height, nil)
}

func CreateTour(path string, width, height int, tour *TourHeader) (*Recorder, error) {
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
		VMSHTour: tour,
	}
	if err := json.NewEncoder(file).Encode(header); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Recorder{file: file, started: now}, nil
}

func (r *Recorder) Input(data []byte) {
	if r == nil || len(data) == 0 {
		return
	}
	r.event("i", string(data))
}

func (r *Recorder) Writer(dst io.Writer) io.Writer {
	return writer{rec: r, dst: dst}
}

func (r *Recorder) Output(data []byte) {
	if r == nil || len(data) == 0 {
		return
	}
	r.event("o", string(data))
}

func (r *Recorder) Metadata(name string, fields map[string]any) {
	if r == nil {
		return
	}
	if name == "ptyterm.resize" {
		cols, colsOK := integerField(fields["cols"])
		rows, rowsOK := integerField(fields["rows"])
		if colsOK && rowsOK {
			r.event("r", fmt.Sprintf("%dx%d", cols, rows))
			return
		}
	}
	if name == "vmsh.tour.section" {
		if title, ok := fields["title"].(string); ok {
			r.event("m", title)
		}
	}
	payload := map[string]any{
		"name":   name,
		"fields": fields,
	}
	r.event("vmsh", payload)
}

func integerField(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	default:
		return 0, false
	}
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
