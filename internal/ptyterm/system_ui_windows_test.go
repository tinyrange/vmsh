//go:build windows

package ptyterm

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/termui/asciicast"
)

func TestDriverWindowsBuiltInConsoleProgram(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "powershell.txt")
	castPath := filepath.Join(root, "powershell.cast")
	rec, err := NewAsciicast(castPath, Size{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("create asciicast: %v", err)
	}
	defer rec.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{
		Command: []string{
			"powershell.exe",
			"-NoProfile",
			"-NonInteractive",
			"-Command",
			"$path = $env:VMSH_PTYTERM_WINDOWS_FILE; [Console]::Out.Write('windows-ready'); [Console]::Out.Flush(); $line = [Console]::In.ReadLine(); Set-Content -LiteralPath $path -Value $line -NoNewline; [Console]::Out.Write(\"`r`nsaved:$line`r`n\"); [Console]::Out.Flush()",
		},
		Size:         Size{Cols: 100, Rows: 30},
		HistoryLimit: 200,
		Env:          []string{"VMSH_PTYTERM_WINDOWS_FILE=" + file},
		Recorder:     rec,
	})
	if err != nil {
		t.Fatalf("start powershell: %v", err)
	}
	defer s.Close()

	driver := NewDriver(s)
	if _, err := driver.WaitLine(ctx, "windows-ready"); err != nil {
		t.Fatalf("wait ready: %v", err)
	}
	if err := driver.Send(Input("driven from conpty"), KeyEnter.Input()); err != nil {
		t.Fatalf("drive powershell: %v", err)
	}
	if _, err := driver.WaitLine(ctx, "saved:driven from conpty"); err != nil {
		t.Fatalf("wait saved: %v", err)
	}
	if result := s.Wait(ctx); result.Err != nil {
		t.Fatalf("powershell result = %+v snapshot=%+v", result, s.Snapshot())
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("close asciicast: %v", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read driven file: %v", err)
	}
	if string(data) != "driven from conpty" {
		t.Fatalf("driven file = %q", data)
	}
	assertWindowsAsciicastHasOutput(t, castPath)
}

func assertWindowsAsciicastHasOutput(t *testing.T, path string) {
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
