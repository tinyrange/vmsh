//go:build linux

package vmshd

import (
	"fmt"
	"os"
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
	return snapshot, nil
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
