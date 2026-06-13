//go:build !darwin && !linux

package terminal

import (
	"os"

	"golang.org/x/crypto/ssh"
)

func SSHTerminalModes(*os.File) ssh.TerminalModes {
	return ssh.TerminalModes{}
}
