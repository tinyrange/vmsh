//go:build !darwin

package main

func platformCompatibilityError() error {
	return nil
}
