package ptyterm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

type Options struct {
	Command      []string
	Dir          string
	Env          []string
	Size         Size
	HistoryLimit int
	Recorder     Recorder
}

type Result struct {
	ExitCode int
	Err      error
}

var ErrUnsupported = errors.New("ptyterm: PTY ownership is unsupported on this platform")

type ptyIO interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}

type ptyProcess struct {
	io     ptyIO
	wait   func() Result
	resize func(Size) error
	kill   func() error
}

type Session struct {
	pty ptyIO
	emu *Emulator

	done     chan struct{}
	readDone chan struct{}

	mu          sync.Mutex
	closed      bool
	result      Result
	resultSet   bool
	lastResize  *Size
	resizeCount int
	recorder    Recorder
	bytesStdin  atomic.Int64
	waitProc    func() Result
	resizePTY   func(Size) error
}

func Start(ctx context.Context, opts Options) (*Session, error) {
	if len(opts.Command) == 0 || strings.TrimSpace(opts.Command[0]) == "" {
		return nil, fmt.Errorf("command is required")
	}
	size := normalizeSize(opts.Size)
	cmd := exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	cmd.Dir = opts.Dir
	if len(opts.Env) != 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	proc, err := startPTY(cmd, size)
	if err != nil {
		return nil, err
	}
	s := &Session{
		pty:       proc.io,
		waitProc:  proc.wait,
		resizePTY: proc.resize,
		emu:       NewEmulator(size, opts.HistoryLimit),
		done:      make(chan struct{}),
		readDone:  make(chan struct{}),
		recorder:  opts.Recorder,
	}
	if s.recorder != nil {
		s.recorder.Metadata("ptyterm.start", map[string]any{
			"command": append([]string(nil), opts.Command...),
			"cols":    size.Cols,
			"rows":    size.Rows,
		})
	}
	go s.readLoop()
	go s.waitLoop()
	if proc.kill != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = proc.kill()
			case <-s.done:
			}
		}()
	}
	return s, nil
}

func (s *Session) Write(data []byte) (int, error) {
	if s == nil || s.pty == nil {
		return 0, os.ErrClosed
	}
	n, err := s.pty.Write(data)
	s.bytesStdin.Add(int64(n))
	return n, err
}

func (s *Session) Resize(size Size) error {
	if s == nil || s.pty == nil {
		return os.ErrClosed
	}
	size = normalizeSize(size)
	if s.resizePTY == nil {
		return os.ErrInvalid
	}
	if err := s.resizePTY(size); err != nil {
		return err
	}
	s.mu.Lock()
	s.lastResize = &Size{Cols: size.Cols, Rows: size.Rows}
	s.resizeCount++
	recorder := s.recorder
	s.mu.Unlock()
	if recorder != nil {
		recorder.Metadata("ptyterm.resize", map[string]any{"cols": size.Cols, "rows": size.Rows})
	}
	s.emu.Resize(size)
	return nil
}

func (s *Session) AttachRecorder(recorder Recorder) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.recorder = recorder
	s.mu.Unlock()
}

func (s *Session) Snapshot() Snapshot {
	if s == nil || s.emu == nil {
		return Snapshot{}
	}
	snap := s.emu.Snapshot()
	s.mu.Lock()
	if s.lastResize != nil {
		size := *s.lastResize
		snap.LastResize = &size
	}
	snap.ResizeCount = s.resizeCount
	s.mu.Unlock()
	snap.BytesStdin = s.bytesStdin.Load()
	select {
	case <-s.done:
		select {
		case <-s.readDone:
			result := s.resultSnapshot()
			snap.Exited = true
			snap.ExitCode = result.ExitCode
			if result.Err != nil {
				snap.ExitError = result.Err.Error()
			}
		default:
		}
	default:
	}
	return snap
}

func (s *Session) Wait(ctx context.Context) Result {
	if s == nil {
		return Result{Err: os.ErrInvalid}
	}
	select {
	case <-s.done:
		select {
		case <-s.readDone:
			return s.resultSnapshot()
		case <-ctx.Done():
			return Result{Err: ctx.Err()}
		}
	case <-ctx.Done():
		return Result{Err: ctx.Err()}
	}
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	return s.closePTY()
}

func (s *Session) closePTY() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ptyFile := s.pty
	s.mu.Unlock()
	if ptyFile != nil {
		return ptyFile.Close()
	}
	return nil
}

func (s *Session) readLoop() {
	defer close(s.readDone)
	defer s.closePTY()
	buf := make([]byte, 32<<10)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			_, _ = s.emu.Write(buf[:n])
			s.recordOutput(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) waitLoop() {
	result := Result{Err: os.ErrInvalid}
	if s.waitProc != nil {
		result = s.waitProc()
	}
	s.mu.Lock()
	s.result = result
	s.resultSet = true
	recorder := s.recorder
	s.mu.Unlock()
	if recorder != nil {
		recorder.Metadata("ptyterm.exit", map[string]any{
			"exit_code": result.ExitCode,
			"error":     result.Err != nil,
		})
	}
	close(s.done)
}

func (s *Session) recordOutput(data []byte) {
	s.mu.Lock()
	recorder := s.recorder
	s.mu.Unlock()
	if recorder != nil {
		recorder.Output(data)
	}
}

func (s *Session) resultSnapshot() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.resultSet {
		return Result{Err: io.ErrUnexpectedEOF}
	}
	return s.result
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
