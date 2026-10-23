//go:build windows

package transfer

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// ListDrives lists local fixed and removable Windows drives. Network drives
// are intentionally excluded because copying a package through SMB is slow and
// can fail halfway.
func ListDrives() ([]Drive, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("transfer: list drive: %w", err)
	}
	drives := make([]Drive, 0, 8)
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		kind := windows.GetDriveType(windows.StringToUTF16Ptr(root))
		if kind != windows.DRIVE_FIXED && kind != windows.DRIVE_REMOVABLE {
			continue
		}

		var free, total uint64
		if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(root), &free, &total, nil); err != nil {
			continue
		}

		var (
			labelBuf [261]uint16
			fsBuf    [261]uint16
		)
		_ = windows.GetVolumeInformation(
			windows.StringToUTF16Ptr(root),
			&labelBuf[0], uint32(len(labelBuf)),
			nil, nil, nil,
			&fsBuf[0], uint32(len(fsBuf)),
		)
		drives = append(drives, Drive{
			Path:       root,
			Label:      strings.TrimSpace(windows.UTF16ToString(labelBuf[:])),
			Filesystem: strings.TrimSpace(windows.UTF16ToString(fsBuf[:])),
			TotalBytes: total,
			FreeBytes:  free,
			Removable:  kind == windows.DRIVE_REMOVABLE,
			Writable:   true,
			Packages:   countPackages(root),
		})
	}
	return drives, nil
}
