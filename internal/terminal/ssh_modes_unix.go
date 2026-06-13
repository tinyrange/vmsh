//go:build darwin || linux

package terminal

import "golang.org/x/crypto/ssh"

func addControlModes(modes ssh.TerminalModes, cc []uint8, mapping map[uint8]int) {
	for mode, index := range mapping {
		if index >= 0 && index < len(cc) {
			modes[mode] = uint32(cc[index])
		}
	}
}

func addFlagModes[T ~uint32 | ~uint64, U ~uint32 | ~uint64](modes ssh.TerminalModes, flags T, mapping map[uint8]U) {
	flagBits := uint64(flags)
	for mode, flag := range mapping {
		if flagBits&uint64(flag) != 0 {
			modes[mode] = 1
		} else {
			modes[mode] = 0
		}
	}
}
