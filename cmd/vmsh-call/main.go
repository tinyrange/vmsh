package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/tinyrange/vmsh/internal/trusted"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: vmsh-call host ACTION [ARG ...]")
		os.Exit(2)
	}
	config, err := trusted.LoadGuestConfig("/run/vmsh/context.json")
	if err == nil && os.Args[1] != "host" && os.Args[1] != config.TargetID {
		err = fmt.Errorf("target %q is not granted", os.Args[1])
	}
	if err == nil {
		err = trusted.Call(context.Background(), config, os.Args[2], os.Args[3:], os.Stdout, os.Stderr)
	}
	if err == nil {
		return
	}
	var exitError trusted.ExitError
	if errors.As(err, &exitError) {
		os.Exit(exitError.Code)
	}
	fmt.Fprintln(os.Stderr, "vmsh-call:", err)
	os.Exit(1)
}
