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

type codexFileRenameInformationLegacy struct {
	replaceIfExists byte
	rootDirectory   windows.Handle
	fileNameLength  uint32
	fileName        [1]uint16
}

const (
	ntFileRenameInformation   = 10
	ntFileRenameInformationEx = 65
)

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
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES,
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
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(handle, &iosb, &buffer[0], uint32(len(buffer)), ntFileRenameInformationEx); err == nil {
		return nil
	}

	var legacyLayout codexFileRenameInformationLegacy
	legacyBuffer := make([]byte, int(unsafe.Offsetof(legacyLayout.fileName))+nameBytes)
	legacy := (*codexFileRenameInformationLegacy)(unsafe.Pointer(&legacyBuffer[0]))
	legacy.replaceIfExists = 1
	legacy.rootDirectory = dstDir
	legacy.fileNameLength = uint32(nameBytes)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&legacy.fileName[0]))[:len(dstName)-1], dstName)
	if err := windows.NtSetInformationFile(handle, &iosb, &legacyBuffer[0], uint32(len(legacyBuffer)), ntFileRenameInformation); err != nil {
		if status, ok := err.(windows.NTStatus); ok {
			return status.Errno()
		}
		return err
	}
	return nil
}
