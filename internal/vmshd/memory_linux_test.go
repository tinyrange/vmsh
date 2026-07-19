//go:build linux

package vmshd

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestLinuxCgroupMemoryReportsEffectiveAvailability(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "user.slice", "vmsh.scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(root, "self.cgroup")
	if err := os.WriteFile(proc, []byte("0::/user.slice/vmsh.scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("4294967296\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("1073741824\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	total, available, ok := linuxCgroupMemory(proc, root)
	if !ok || total != 4096 || available != 3072 {
		t.Fatalf("cgroup memory = total %d available %d ok %t", total, available, ok)
	}
}
