//go:build windows

package ptyterm

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionWindowsConPTYCapturesOutputAndExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{
		Command:      []string{"cmd.exe", "/d", "/q", "/c", "echo conpty-ready"},
		Size:         Size{Cols: 80, Rows: 8},
		HistoryLimit: 10,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.Close()

	result := s.Wait(ctx)
	if result.Err != nil {
		snap := s.Snapshot()
		t.Fatalf("wait result = %+v snapshot = exited:%v code:%d bytes:%d lines:%#v history:%#v raw:%q", result, snap.Exited, snap.ExitCode, snap.BytesRead, snap.Lines, snap.History, snap.RawTail)
	}
	snap := s.Snapshot()
	if !snap.Exited || snap.ExitCode != 0 {
		t.Fatalf("snapshot exit = exited:%v code:%d err:%q", snap.Exited, snap.ExitCode, snap.ExitError)
	}
	if !snapshotHasSubstring(snap, "conpty-ready") {
		t.Fatalf("snapshot missing output: lines=%#v history=%#v raw=%q", snap.Lines, snap.History, snap.RawTail)
	}
	if snap.BytesRead == 0 {
		t.Fatalf("snapshot did not record bytes read")
	}
}

func TestSessionWindowsConPTYExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{
		Command: []string{"cmd.exe", "/d", "/q", "/c", "exit /b 17"},
		Size:    Size{Cols: 80, Rows: 8},
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.Close()

	result := s.Wait(ctx)
	if result.ExitCode != 17 || result.Err == nil {
		snap := s.Snapshot()
		t.Fatalf("result = %+v, want exit 17; snapshot = exited:%v code:%d bytes:%d lines:%#v history:%#v raw:%q", result, snap.Exited, snap.ExitCode, snap.BytesRead, snap.Lines, snap.History, snap.RawTail)
	}
	if snap := s.Snapshot(); !snap.Exited || snap.ExitCode != 17 {
		t.Fatalf("snapshot exit = exited:%v code:%d err:%q", snap.Exited, snap.ExitCode, snap.ExitError)
	}
}

func snapshotHasSubstring(snap Snapshot, needle string) bool {
	for _, line := range append(append([]string{}, snap.History...), snap.Lines...) {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
