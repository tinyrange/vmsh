package wait

import (
	"context"
	"errors"
	"time"

	"github.com/tinyrange/vmsh/internal/termui/progress"
)

var ErrInterrupted = errors.New("wait interrupted")

type DisplayedError struct {
	Err error
}

func (e DisplayedError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e DisplayedError) Unwrap() error {
	return e.Err
}

func Displayed(err error) error {
	if err == nil {
		return nil
	}
	return DisplayedError{Err: err}
}

func IsDisplayed(err error) bool {
	var displayed DisplayedError
	return errors.As(err, &displayed)
}

type Result int

const (
	ResultCompleted Result = iota
	ResultInterrupted
	ResultFailed
)

type Spec struct {
	Message                 string
	ResponsivenessThreshold time.Duration
	DetailAfter             time.Duration
	UpdateInterval          time.Duration
	Timeout                 time.Duration
	Progress                *progress.Reporter
	CompletionMessage       string
	InterruptMessage        string
	FailurePrefix           string
}

func (s Spec) DetailThreshold() time.Duration {
	if s.DetailAfter <= 0 {
		return 500 * time.Millisecond
	}
	return s.DetailAfter
}

func (s Spec) FirstStatusAfter() time.Duration {
	if s.ResponsivenessThreshold <= 0 {
		return 50 * time.Millisecond
	}
	return s.ResponsivenessThreshold
}

func (s Spec) TickInterval() time.Duration {
	if s.UpdateInterval <= 0 {
		return 100 * time.Millisecond
	}
	return s.UpdateInterval
}

func (s Spec) MessageText() string {
	if s.Message == "" {
		return "working"
	}
	return s.Message
}

func Context(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}
