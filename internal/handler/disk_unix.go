//go:build !windows

package handler

import "syscall"

// diskUsage returns (used, total) bytes for the filesystem hosting "/".
func diskUsage() (used, total float64) {
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) != nil {
		return 0, 0
	}
	total = float64(st.Blocks) * float64(st.Bsize)
	used = total - float64(st.Bfree)*float64(st.Bsize)
	return used, total
}
