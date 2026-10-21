//go:build !windows

package main

import "syscall"

// diskUsage melaporkan byte terpakai dan kapasitas total pada filesystem yang
// menampung path, memakai statfs.
func diskUsage(path string) (usedBytes, totalBytes int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}

	total := int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bavail) * int64(st.Bsize)
	return total - free, total, nil
}
