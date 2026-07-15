package main

import (
	"fmt"
	"os"

	"github.com/tinyrange/vmsh/internal/shell"
	"github.com/tinyrange/vmsh/internal/trusted"
)

func main() {
	if runInternalCCVMFromEnv() {
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "profile" && os.Args[2] == "seal" {
		profile, err := trusted.SealProfile(os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, "vmsh profile seal:", err)
			os.Exit(1)
		}
		fmt.Println(profile.Digest)
		return
	}
	if err := shell.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vmsh:", err)
		os.Exit(1)
	}
}
