//go:build windows

package handler

import "golang.org/x/sys/windows"

// diskUsage returns (used, total) bytes for the filesystem hosting the
// current working directory (the drive dumbodb is running on).
func diskUsage() (used, total float64) {
	root, err := windows.UTF16PtrFromString(".")
	if err != nil {
		return 0, 0
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(root, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, 0
	}
	total = float64(totalBytes)
	used = total - float64(totalFreeBytes)
	return used, total
}
