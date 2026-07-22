//go:build windows

package operations

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type platformDiskProbe struct{}

func (platformDiskProbe) FreeBytes(path string) (uint64, error) {
	root := filepath.VolumeName(filepath.Clean(path)) + `\`
	if root == `\` {
		root = filepath.Clean(path)
	}
	pointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errDiskProbe, err)
	}
	var freeAvailable uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &freeAvailable, nil, nil); err != nil {
		return 0, fmt.Errorf("%w: %v", errDiskProbe, err)
	}
	return freeAvailable, nil
}
