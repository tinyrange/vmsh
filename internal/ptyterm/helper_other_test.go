//go:build !windows

package ptyterm

import "errors"

func ptyTermHelperSize() (int, int, error) {
	return 0, 0, errors.New("terminal size helper is only used on Windows")
}
