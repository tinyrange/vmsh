//go:build windows

package ptyterm

import "golang.org/x/sys/windows"

func ptyTermHelperSize() (int, int, error) {
	handle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return 0, 0, err
	}
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(handle, &info); err != nil {
		return 0, 0, err
	}
	rows := int(info.Window.Bottom-info.Window.Top) + 1
	cols := int(info.Window.Right-info.Window.Left) + 1
	return rows, cols, nil
}
