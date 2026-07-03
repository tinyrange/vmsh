//go:build !darwin && !linux

package vmshd

type systemMemoryObserver struct{}

func (systemMemoryObserver) Snapshot() (memorySnapshot, error) {
	return memorySnapshot{}, nil
}
