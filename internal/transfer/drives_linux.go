//go:build linux

package transfer

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ListDrives lists mounted filesystems below the usual removable-media roots
// on Linux. Auto-mounting is deliberately left to udev/systemd/udisks; this
// function only reports what is already mounted.
func ListDrives() ([]Drive, error) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil, fmt.Errorf("transfer: baca /proc/self/mounts: %w", err)
	}
	defer func() { _ = f.Close() }()

	seen := make(map[string]struct{})
	drives := make([]Drive, 0, 8)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mount := unescapeMount(fields[1])
		if !isMediaMount(mount) {
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
			Filesystem: fields[2],
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

// unescapeMount decodes the octal escapes used by /proc/self/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
