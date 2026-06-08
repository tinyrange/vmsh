package main

import (
	"fmt"
	"os"

	"github.com/tinyrange/vmsh/internal/shell"
)

func main() {
	if runInternalCCVMFromEnv() {
		return
	}
	if err := shell.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vmsh:", err)
		os.Exit(1)
	}
}
