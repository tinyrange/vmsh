package asciicast

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestRecorderWritesOutputAndMetadataEvents(t *testing.T) {
	file, err := os.CreateTemp("", "termui-cast-*.cast")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	file.Close()
	defer os.Remove(path)

	rec, err := Create(path, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	var dst bytes.Buffer
	if _, err := rec.Writer(&dst).Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
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

	var output []any
	if err := dec.Decode(&output); err != nil {
		t.Fatalf("decode output event: %v", err)
	}
	if len(output) != 3 || output[1] != "o" || output[2] != "hello" {
		t.Fatalf("output event = %#v", output)
	}

	var metadata []any
	if err := dec.Decode(&metadata); err != nil {
		t.Fatalf("decode metadata event: %v", err)
	}
	if len(metadata) != 3 || metadata[1] != "m" {
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
