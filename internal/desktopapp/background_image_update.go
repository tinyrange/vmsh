package desktopapp

import (
	"context"
	"sync"
)

// backgroundImageStage coordinates one inactive image preparation. The stage
// uses the application lifetime rather than a VM attempt context, so stopping
// the old VM does not cancel a nearly complete download.
type backgroundImageStage struct {
	mu      sync.Mutex
	started bool
	name    string
	done    chan struct{}
	err     error
}

func (s *backgroundImageStage) start(name string, prepare func() error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return false
	}
	s.started = true
	s.name = name
	s.done = make(chan struct{})
	go func(done chan struct{}) {
		err := prepare()
		s.mu.Lock()
		s.err = err
		close(done)
		s.mu.Unlock()
	}(s.done)
	return true
}

// take waits for and consumes the current stage. A caller can fall back to a
// normal foreground pull when started is false or err is non-nil.
func (s *backgroundImageStage) take(ctx context.Context) (name string, started bool, err error) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return "", false, nil
	}
	done := s.done
	name = s.name
	s.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return name, true, ctx.Err()
	}

	s.mu.Lock()
	err = s.err
	s.started = false
	s.name = ""
	s.done = nil
	s.err = nil
	s.mu.Unlock()
	return name, true, err
}
