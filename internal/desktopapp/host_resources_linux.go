//go:build linux

package desktopapp

import (
	"os"
	"strconv"
	"strings"
)

func hostMemoryMB() uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			return value / 1024
		}
	}
	return 0
}
