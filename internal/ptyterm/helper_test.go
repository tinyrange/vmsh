package ptyterm

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
)

const ptyTermHelperEnv = "VMSH_PTYTERM_HELPER=1"

func ptyTermTestHelperCommand(mode string, args ...string) []string {
	command := []string{os.Args[0], "-test.run=^TestPTYTermHelperProcess$", "--", mode}
	return append(command, args...)
}

func TestPTYTermHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	mode := os.Args[separator+1]
	args := os.Args[separator+2:]
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
	case "print":
		if len(args) != 1 {
			_, _ = fmt.Fprint(os.Stderr, "print helper requires one argument")
			os.Exit(2)
		}
		_, _ = fmt.Fprint(os.Stdout, args[0])
		_ = os.Stdout.Sync()
		os.Exit(0)
	case "alt-screen":
		_, _ = fmt.Fprint(os.Stdout, "main\x1b[?1049hALT\x1b[?1049lRESTORE")
		_ = os.Stdout.Sync()
		os.Exit(0)
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
