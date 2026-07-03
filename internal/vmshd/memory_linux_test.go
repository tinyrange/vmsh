//go:build linux

package vmshd

import "testing"

func TestParseLinuxMeminfo(t *testing.T) {
	snapshot, ok := parseLinuxMeminfo("MemTotal:       16384000 kB\nMemFree:         1000000 kB\nMemAvailable:    8192000 kB\n")
	if !ok {
		t.Fatalf("parseLinuxMeminfo ok = false")
	}
	if snapshot.TotalMB != 16000 {
		t.Fatalf("total_mb = %d, want 16000", snapshot.TotalMB)
	}
	if snapshot.AvailableMB != 8000 {
		t.Fatalf("available_mb = %d, want 8000", snapshot.AvailableMB)
	}
}
