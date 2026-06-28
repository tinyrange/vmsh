//go:build windows

package shell

import (
	"os"
	"time"

	"github.com/tinyrange/vmsh/internal/terminal"
)

type ptyInputCanceller struct {
	file *os.File
}

func newPTYInputCanceller(file *os.File) (*ptyInputCanceller, error) {
	return &ptyInputCanceller{file: file}, nil
}

func (c *ptyInputCanceller) cancel() {
	if c == nil {
		return
	}
	terminal.InterruptRead(c.file)
}

func (c *ptyInputCanceller) close() {}

func readPTYInput(file *os.File, buf []byte, done <-chan struct{}, _ *ptyInputCanceller) (int, error) {
	for {
		select {
		case <-done:
			return 0, nil
		default:
		}
		n, err := file.Read(buf)
		if err != nil {
			return n, err
		}
		if n > 0 {
			return n, nil
		}
		sleepOrDone(done, 10*time.Millisecond)
	}
}
