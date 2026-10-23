//go:build windows

package transfer

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func freeBytes(path string) (uint64, error) {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return 0, fmt.Errorf("transfer: volume tidak dikenal untuk %s", path)
	}
	var free, total uint64
	root := volume + `\`
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(root), &free, &total, nil); err != nil {
		return 0, err
	}
	return free, nil
}
