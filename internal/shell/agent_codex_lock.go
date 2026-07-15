package shell

import (
	cryptoRand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

var errCodexLockTimeout = errors.New("timed out waiting for Codex install lock")

type codexLockOwner struct {
	PID            int       `json:"pid"`
	Generation     string    `json:"generation"`
	AcquiredAt     time.Time `json:"acquired_at"`
	LeaseUpdatedAt time.Time `json:"lease_updated_at"`
}

type codexFileLock struct {
	file   *os.File
	owner  codexLockOwner
	unlock func() error
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	err    error
}

func newCodexFileLock(file *os.File, unlock func() error) (*codexFileLock, error) {
	var generation [16]byte
	if _, err := cryptoRand.Read(generation[:]); err != nil {
		return nil, fmt.Errorf("create Codex lock generation: %w", err)
	}
	now := time.Now().UTC()
	lock := &codexFileLock{
		file: file,
		owner: codexLockOwner{
			PID:            os.Getpid(),
			Generation:     hex.EncodeToString(generation[:]),
			AcquiredAt:     now,
			LeaseUpdatedAt: now,
		},
		unlock: unlock,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if err := lock.writeOwner(); err != nil {
		return nil, err
	}
	go lock.renewLease()
	return lock, nil
}

func (l *codexFileLock) renewLease() {
	defer close(l.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.owner.LeaseUpdatedAt = time.Now().UTC()
			_ = l.writeOwner()
		case <-l.stop:
			return
		}
	}
}

func (l *codexFileLock) writeOwner() error {
	data, err := json.Marshal(l.owner)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := l.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate Codex lock metadata: %w", err)
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek Codex lock metadata: %w", err)
	}
	if _, err := l.file.Write(data); err != nil {
		return fmt.Errorf("write Codex lock metadata: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync Codex lock metadata: %w", err)
	}
	return nil
}

func (l *codexFileLock) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		if l.unlock != nil {
			l.err = l.unlock()
		}
		if err := l.file.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}
