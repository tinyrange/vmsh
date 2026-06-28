package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactDemoCast(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.cast")
	out := filepath.Join(dir, "out.cast")
	p := paths{root: filepath.Join(dir, "repo")}
	input := `{"version":2,"width":80,"height":24}` + "\n" +
		`[0.1,"o","` + p.root + ` /tmp/d123/h 127.0.0.1:49152"]` + "\n"
	if err := os.WriteFile(raw, []byte(input), 0o644); err != nil {
		t.Fatalf("write raw cast: %v", err)
	}
	if err := redactDemoCast(raw, out, p, "127.0.0.1:49152"); err != nil {
		t.Fatalf("redact cast: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read redacted cast: %v", err)
	}
	lines := splitCastLines(string(data))
	if len(lines) != 2 {
		t.Fatalf("redacted cast lines = %q, want header and one event", lines)
	}
	event, err := parseDemoCastEvent(lines[1])
	if err != nil {
		t.Fatalf("parse redacted event: %v", err)
	}
	if event.Time != 0 {
		t.Fatalf("redacted event time = %v, want 0", event.Time)
	}
	wantData := "/work/vmsh /tmp/d123/h 127.0.0.1:2222"
	if event.Data != wantData {
		t.Fatalf("redacted event data = %q, want %q", event.Data, wantData)
	}
}

func TestParseSSHExecPayload(t *testing.T) {
	payload := make([]byte, 4+len("hostname"))
	binary.BigEndian.PutUint32(payload[:4], uint32(len("hostname")))
	copy(payload[4:], "hostname")
	got, err := parseSSHExecPayload(payload)
	if err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if got != "hostname" {
		t.Fatalf("payload = %q", got)
	}
	if _, err := parseSSHExecPayload([]byte{0, 0, 0, 9, 'h'}); err == nil {
		t.Fatalf("truncated payload succeeded")
	}
}

func splitCastLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
