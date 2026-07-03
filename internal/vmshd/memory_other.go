//go:build !darwin

package vmshd

type systemMemoryObserver struct{}

func (systemMemoryObserver) Snapshot() (memorySnapshot, error) {
	return memorySnapshot{}, nil
}
