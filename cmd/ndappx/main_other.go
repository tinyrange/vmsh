//go:build !darwin && !windows

package main

func platformArguments(args []string) []string {
	if len(args) == 0 {
		return []string{defaultNeurodesktopImage}
	}
	return args
}
