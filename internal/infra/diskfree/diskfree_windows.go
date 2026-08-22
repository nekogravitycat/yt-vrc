//go:build windows

package diskfree

import "golang.org/x/sys/windows"

// Bytes returns free bytes available to the calling user on the volume
// containing path, or 0 when it cannot be determined.
func Bytes(path string) int64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0
	}
	return int64(free)
}
