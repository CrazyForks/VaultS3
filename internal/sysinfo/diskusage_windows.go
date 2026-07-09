//go:build windows

package sysinfo

import (
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// statfsPath returns total and available bytes for the volume containing path
// via GetDiskFreeSpaceEx (no external dependency), plus the drive letter as a
// best-effort dedup key.
func statfsPath(path string) (total, free, dev uint64, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0, err
	}
	var freeAvail, totalBytes, totalFree uint64
	r, _, e := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, 0, 0, e
	}
	if len(path) > 0 {
		dev = uint64(path[0]) // drive letter
	}
	return totalBytes, freeAvail, dev, nil
}
