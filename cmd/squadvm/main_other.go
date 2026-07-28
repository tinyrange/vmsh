//go:build !darwin && !windows

package main

func platformArguments(args []string) []string {
	if len(args) == 0 {
		return []string{defaultSquadVMImage}
	}
	return args
}

func platformDefaultCPUs() int {
	return 4
}

func platformDefaultEphemeralHome() bool {
	return false
}
