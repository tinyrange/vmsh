package ptyterm

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/termui/asciicast"
)

func TestParseChord(t *testing.T) {
	for _, tc := range []struct {
		chord string
		want  string
	}{
		{chord: "enter", want: "\r"},
		{chord: "escape", want: "\x1b"},
		{chord: "ctrl+c", want: "\x03"},
		{chord: "alt+x", want: "\x1bx"},
		{chord: "f5", want: "\x1b[15~"},
	} {
		got, err := ParseChord(tc.chord)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.chord, err)
		}
		if string(got) != tc.want {
			t.Fatalf("parse %q = %q, want %q", tc.chord, got, tc.want)
		}
	}
}

func TestDriverSendsTextKeysAndRecordsAsciicast(t *testing.T) {
	path := t.TempDir() + "/session.cast"
	rec, err := NewAsciicast(path, Size{Cols: 24, Rows: 4})
	if err != nil {
		t.Fatalf("create asciicast: %v", err)
	}
	defer rec.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{
		Command:      driverEchoCommand(),
		Size:         Size{Cols: 24, Rows: 4},
		HistoryLimit: 10,
		Env:          []string{ptyTermHelperEnv},
		Recorder:     rec,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.Close()

	driver := NewDriver(s)
	if _, err := driver.WaitLine(ctx, "ready"); err != nil {
		t.Fatalf("wait ready: %v", err)
	}
	if err := driver.Text("hello"); err != nil {
		t.Fatalf("send text: %v", err)
	}
	if err := driver.Key(KeyEnter); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if _, err := driver.WaitLine(ctx, "got:hello"); err != nil {
		t.Fatalf("wait response: %v", err)
	}
	if result := s.Wait(ctx); result.Err != nil {
		t.Fatalf("wait result = %+v", result)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("close asciicast: %v", err)
	}
	assertAsciicastHasOutput(t, path)
}

func assertAsciicastHasOutput(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open asciicast: %v", err)
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	var header asciicast.Header
	if err := dec.Decode(&header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.Version != 2 {
		t.Fatalf("header version = %d", header.Version)
	}
	for {
		var event []any
		err := dec.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if len(event) == 3 && event[1] == "o" && event[2] != "" {
			return
		}
	}
	t.Fatalf("asciicast had no output events")
}

func driverEchoCommand() []string {
	if runtime.GOOS == "windows" {
		return ptyTermTestHelperCommand("echo")
	}
	return []string{"sh", "-lc", "printf ready; IFS= read -r line; printf '\\r\\ngot:%s\\r\\n' \"$line\""}
}
