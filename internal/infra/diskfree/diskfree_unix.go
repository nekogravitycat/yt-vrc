//go:build !windows

package diskfree

import "syscall"

// Bytes returns free bytes available to an unprivileged user on the
// filesystem containing path, or 0 when it cannot be determined.
func Bytes(path string) int64 {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0
	}
	return int64(uint64(fs.Bavail) * uint64(fs.Bsize))
}
