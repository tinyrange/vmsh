//go:build darwin

package vmshd

import "golang.org/x/sys/unix"

type systemMemoryObserver struct{}

func (systemMemoryObserver) Snapshot() (memorySnapshot, error) {
	totalBytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return memorySnapshot{}, err
	}
	pageSize := uint64(unix.Getpagesize())
	free, err := unix.SysctlUint32("vm.page_free_count")
	if err != nil {
		return memorySnapshot{}, err
	}
	speculative, err := unix.SysctlUint32("vm.page_speculative_count")
	if err != nil {
		return memorySnapshot{}, err
	}
	availableBytes := (uint64(free) + uint64(speculative)) * pageSize
	return memorySnapshot{
		TotalMB:     totalBytes / (1024 * 1024),
		AvailableMB: availableBytes / (1024 * 1024),
	}, nil
}
