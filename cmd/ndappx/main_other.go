//go:build !darwin && !windows

package main

func platformArguments(args []string) []string {
	return args
}
