package asciicast

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestRecorderWritesTourInputOutputAndMetadataEvents(t *testing.T) {
	file, err := os.CreateTemp("", "termui-cast-*.cast")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	file.Close()
	defer os.Remove(path)

	rec, err := CreateTour(path, 80, 24, &TourHeader{
		Schema:      1,
		ID:          "context-switching",
		Title:       "Move between host and VM contexts",
		VMSHVersion: "v1.2.3",
		Commit:      "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	var dst bytes.Buffer
	if _, err := rec.Writer(&dst).Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	rec.Input([]byte("uname -s\r"))
	rec.Metadata("vmsh.tour.section", map[string]any{"title": "Run a command", "markdown": "Use `uname`."})
	rec.Metadata("ptyterm.resize", map[string]any{"cols": 100, "rows": 30})
	rec.Metadata("termui.slow_interaction", map[string]any{"elapsed_ms": 51})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	dec := json.NewDecoder(file)
	var header Header
	if err := dec.Decode(&header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.Version != 2 || header.Width != 80 || header.Height != 24 {
		t.Fatalf("header = %+v, want version 2 and 80x24", header)
	}
	if header.VMSHTour == nil || header.VMSHTour.Schema != 1 || header.VMSHTour.ID != "context-switching" || header.VMSHTour.VMSHVersion != "v1.2.3" || header.VMSHTour.Commit != "abc123" {
		t.Fatalf("tour header = %+v", header.VMSHTour)
	}

	var output []any
	if err := dec.Decode(&output); err != nil {
		t.Fatalf("decode output event: %v", err)
	}
	if len(output) != 3 || output[1] != "o" || output[2] != "hello" {
		t.Fatalf("output event = %#v", output)
	}

	var input []any
	if err := dec.Decode(&input); err != nil {
		t.Fatalf("decode input event: %v", err)
	}
	if len(input) != 3 || input[1] != "i" || input[2] != "uname -s\r" {
		t.Fatalf("input event = %#v", input)
	}

	var marker []any
	if err := dec.Decode(&marker); err != nil {
		t.Fatalf("decode marker event: %v", err)
	}
	if len(marker) != 3 || marker[1] != "m" || marker[2] != "Run a command" {
		t.Fatalf("marker event = %#v", marker)
	}

	var section []any
	if err := dec.Decode(&section); err != nil {
		t.Fatalf("decode section event: %v", err)
	}
	if len(section) != 3 || section[1] != "vmsh" {
		t.Fatalf("section event = %#v", section)
	}

	var resize []any
	if err := dec.Decode(&resize); err != nil {
		t.Fatalf("decode resize event: %v", err)
	}
	if len(resize) != 3 || resize[1] != "r" || resize[2] != "100x30" {
		t.Fatalf("resize event = %#v", resize)
	}

	var metadata []any
	if err := dec.Decode(&metadata); err != nil {
		t.Fatalf("decode metadata event: %v", err)
	}
	if len(metadata) != 3 || metadata[1] != "vmsh" {
		t.Fatalf("metadata event = %#v", metadata)
	}
	payload, ok := metadata[2].(map[string]any)
	if !ok {
		t.Fatalf("metadata payload = %#v", metadata[2])
	}
	if payload["name"] != "termui.slow_interaction" {
		t.Fatalf("metadata name = %#v", payload["name"])
	}
	fields, ok := payload["fields"].(map[string]any)
	if !ok || fields["elapsed_ms"] != float64(51) {
		t.Fatalf("metadata fields = %#v", payload["fields"])
	}
}
