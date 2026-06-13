//go:build darwin || linux

package shell

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/tinyrange/vmsh/internal/terminal"
	"golang.org/x/sys/unix"
)

type ptyInputCanceller struct {
	read  *os.File
	write *os.File
}

func newPTYInputCanceller(*os.File) (*ptyInputCanceller, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &ptyInputCanceller{read: read, write: write}, nil
}

func (c *ptyInputCanceller) cancel() {
	if c == nil || c.write == nil {
		return
	}
	_, _ = c.write.Write([]byte{1})
}

func (c *ptyInputCanceller) close() {
	if c == nil {
		return
	}
	if c.read != nil {
		_ = c.read.Close()
	}
	if c.write != nil {
		_ = c.write.Close()
	}
}

func readPTYInput(file *os.File, buf []byte, done <-chan struct{}, cancel *ptyInputCanceller) (int, error) {
	if cancel != nil && terminal.IsTerminalFD(int(file.Fd())) {
		pollFDs := []unix.PollFd{
			{Fd: int32(file.Fd()), Events: unix.POLLIN},
			{Fd: int32(cancel.read.Fd()), Events: unix.POLLIN},
		}
		for {
			select {
			case <-done:
				return 0, nil
			default:
			}
			n, err := unix.Poll(pollFDs, -1)
			if err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}
				return 0, err
			}
			if n == 0 {
				continue
			}
			if pollFDs[1].Revents != 0 {
				return 0, nil
			}
			if pollFDs[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return 0, os.ErrClosed
			}
			if pollFDs[0].Revents&unix.POLLIN != 0 {
				return file.Read(buf)
			}
		}
	}

	for {
		select {
		case <-done:
			return 0, nil
		default:
		}
		n, err := file.Read(buf)
		if err != nil && (errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)) {
			sleepOrDone(done, 10*time.Millisecond)
			continue
		}
		return n, err
	}
}
