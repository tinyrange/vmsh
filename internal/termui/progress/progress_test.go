package progress

import (
	"strings"
	"testing"
	"time"
)

func TestSnapshotMessageUnknownTotalAvoidsPercentage(t *testing.T) {
	started := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	got := Snapshot{
		Operation: "scanning",
		Unit:      "files",
		Done:      7,
		Started:   started,
	}.Message(started.Add(time.Second))

	if strings.Contains(got, "%") {
		t.Fatalf("unknown-total message includes percentage: %q", got)
	}
	if !strings.Contains(got, "7") || !strings.Contains(got, "files") {
		t.Fatalf("unknown-total message lost measured progress: %q", got)
	}
}
