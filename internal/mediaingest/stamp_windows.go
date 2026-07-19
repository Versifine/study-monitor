//go:build windows

package mediaingest

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	Attributes     uint32
	_              uint32
}

func platformFileStamp(path string, _ os.FileInfo) (string, int64, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", 0, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return "", 0, err
	}
	defer windows.CloseHandle(handle)
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &identity); err != nil {
		return "", 0, err
	}
	var basic fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)),
		uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return "", 0, err
	}
	fileIdentity := fmt.Sprintf("%08x:%08x%08x", identity.VolumeSerialNumber, identity.FileIndexHigh, identity.FileIndexLow)
	return fileIdentity, basic.ChangeTime, nil
}
