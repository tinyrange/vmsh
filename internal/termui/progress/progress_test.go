package progress

import (
	"testing"
	"time"
)

func TestSnapshotMessageDeterminateProgress(t *testing.T) {
	started := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	got := Snapshot{
		Operation: "reading",
		Unit:      "bytes",
		Done:      5,
		Total:     10,
		Started:   started,
	}.Message(started.Add(2 * time.Second))

	if want := "reading: 5/10 bytes (50%, 2s)"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestSnapshotMessageUnknownTotalAvoidsPercentage(t *testing.T) {
	started := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	got := Snapshot{
		Operation: "scanning",
		Unit:      "files",
		Done:      7,
		Started:   started,
	}.Message(started.Add(time.Second))

	if want := "scanning: 7 files (1s)"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}
