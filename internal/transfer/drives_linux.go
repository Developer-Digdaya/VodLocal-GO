//go:build linux

package transfer

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ListDrives lists mounted filesystems below the usual removable-media roots
// on Linux. Auto-mounting is deliberately left to udev/systemd/udisks; this
// function only reports what is already mounted.
func ListDrives() ([]Drive, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("transfer: baca /proc/self/mountinfo: %w", err)
	}
	defer func() { _ = f.Close() }()

	seen := make(map[string]struct{})
	drives := make([]Drive, 0, 8)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		mount, fstype, ok := parseMountInfo(scanner.Text())
		if !ok || !isMediaMount(mount) {
			continue
		}
		if _, ok := seen[mount]; ok {
			continue
		}
		if err := unix.Access(mount, unix.R_OK); err != nil {
			continue
		}
		writable := unix.Access(mount, unix.W_OK) == nil
		var st unix.Statfs_t
		if err := unix.Statfs(mount, &st); err != nil {
			continue
		}
		seen[mount] = struct{}{}
		total := st.Blocks * uint64(st.Bsize)
		free := st.Bavail * uint64(st.Bsize)
		drives = append(drives, Drive{
			Path:       mount,
			Label:      filepath.Base(mount),
			Filesystem: fstype,
			TotalBytes: total,
			FreeBytes:  free,
			Writable:   writable,
			Packages:   countPackages(mount),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return drives, nil
}

func isMediaMount(p string) bool {
	clean := filepath.Clean(p)
	for _, root := range []string{"/mnt", "/media", "/run/media"} {
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return true
		}
	}
	return false
}
