//go:build windows

package shell

import (
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type codexFileRenameInformation struct {
	flags          uint32
	rootDirectory  windows.Handle
	fileNameLength uint32
	fileName       [1]uint16
}

func replaceCodexActivationLink(src, dst string) error {
	srcName, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		srcName,
		windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	dstDirName, err := windows.UTF16PtrFromString(filepath.Dir(dst))
	if err != nil {
		return err
	}
	dstDir, err := windows.CreateFile(
		dstDirName,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(dstDir)

	dstName, err := windows.UTF16FromString(filepath.Base(dst))
	if err != nil {
		return err
	}
	nameBytes := (len(dstName) - 1) * 2
	var layout codexFileRenameInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.fileName))+nameBytes)
	info := (*codexFileRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.rootDirectory = dstDir
	info.fileNameLength = uint32(nameBytes)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.fileName[0]))[:len(dstName)-1], dstName)
	return windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer)))
}
