//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// diskUsage melaporkan byte terpakai dan kapasitas total pada volume yang
// menampung path, memakai GetDiskFreeSpaceExW.
func diskUsage(path string) (usedBytes, totalBytes int64, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}

	var freeToCaller, total, totalFree uint64
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	r1, _, callErr := proc.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 {
		return 0, 0, callErr
	}

	used := int64(total - totalFree)
	return used, int64(total), nil
}
