package ptyterm

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
)

const ptyTermHelperEnv = "VMSH_PTYTERM_HELPER=1"

func ptyTermTestHelperCommand(mode string) []string {
	return []string{os.Args[0], "-test.run=^TestPTYTermHelperProcess$", "--", mode}
}

func TestPTYTermHelperProcess(t *testing.T) {
	if os.Getenv("VMSH_PTYTERM_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "echo":
		_, _ = fmt.Fprint(os.Stdout, "ready")
		_ = os.Stdout.Sync()
	case "size":
		rows, cols, err := ptyTermHelperSize()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "query terminal size: %v", err)
			os.Exit(2)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%d %d\r\n", rows, cols)
		_ = os.Stdout.Sync()
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown PTY helper mode %q", mode)
		os.Exit(2)
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "read terminal input: %v", err)
		os.Exit(2)
	}
	line = strings.TrimRight(line, "\r\n")
	if mode == "echo" {
		_, _ = fmt.Fprintf(os.Stdout, "\r\ngot:%s\r\n", line)
	} else {
		rows, cols, err := ptyTermHelperSize()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "query resized terminal: %v", err)
			os.Exit(2)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%d %d\r\n", rows, cols)
	}
	_ = os.Stdout.Sync()
	os.Exit(0)
}
