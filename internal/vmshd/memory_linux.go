//go:build linux

package vmshd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type systemMemoryObserver struct{}

func (systemMemoryObserver) Snapshot() (memorySnapshot, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memorySnapshot{}, err
	}
	snapshot, ok := parseLinuxMeminfo(string(data))
	if !ok {
		return memorySnapshot{}, fmt.Errorf("parse /proc/meminfo")
	}
	if totalMB, availableMB, ok := linuxCgroupMemory("/proc/self/cgroup", "/sys/fs/cgroup"); ok {
		snapshot.TotalMB = min(snapshot.TotalMB, totalMB)
		snapshot.AvailableMB = min(snapshot.AvailableMB, availableMB)
	}
	return snapshot, nil
}

func linuxCgroupMemory(procPath, root string) (uint64, uint64, bool) {
	data, err := os.ReadFile(procPath)
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
			continue
		}
		rel := strings.TrimPrefix(filepath.Clean("/"+parts[2]), "/")
		dir := filepath.Join(root, rel)
		maxText, maxErr := os.ReadFile(filepath.Join(dir, "memory.max"))
		currentText, currentErr := os.ReadFile(filepath.Join(dir, "memory.current"))
		if maxErr != nil || currentErr != nil {
			return 0, 0, false
		}
		if strings.TrimSpace(string(maxText)) == "max" {
			maxText, maxErr = os.ReadFile(filepath.Join(dir, "memory.high"))
			if maxErr != nil || strings.TrimSpace(string(maxText)) == "max" {
				return 0, 0, false
			}
		}
		maximum, maxErr := strconv.ParseUint(strings.TrimSpace(string(maxText)), 10, 64)
		current, currentErr := strconv.ParseUint(strings.TrimSpace(string(currentText)), 10, 64)
		if maxErr != nil || currentErr != nil || maximum == 0 {
			return 0, 0, false
		}
		available := uint64(0)
		if current < maximum {
			available = maximum - current
		}
		return maximum >> 20, available >> 20, true
	}
	return 0, 0, false
}

func parseLinuxMeminfo(data string) (memorySnapshot, bool) {
	var totalKB, availableKB uint64
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			totalKB = value
		case "MemAvailable":
			availableKB = value
		}
	}
	if totalKB == 0 {
		return memorySnapshot{}, false
	}
	return memorySnapshot{
		TotalMB:     totalKB / 1024,
		AvailableMB: availableKB / 1024,
	}, true
}
