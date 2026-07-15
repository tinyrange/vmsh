package backend

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrDaemonLockTimeout = errors.New("timed out waiting for daemon state lock")

type daemonFileLock struct {
	file   *os.File
	unlock func() error
	once   sync.Once
	err    error
}

func acquireDaemonFileLock(path string, timeout time.Duration) (*daemonFileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		unlock, acquired, err := tryLockDaemonFile(file)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if acquired {
			return &daemonFileLock{file: file, unlock: unlock}, nil
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, ErrDaemonLockTimeout
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (l *daemonFileLock) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.unlock != nil {
			l.err = l.unlock()
		}
		if err := l.file.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}

func withDaemonStateWriteLock(path string, fn func() error) error {
	lock, err := acquireDaemonFileLock(path+".lock", 5*time.Second)
	if err != nil {
		return fmt.Errorf("lock daemon state %s: %w", path, err)
	}
	defer lock.Release()
	return fn()
}
