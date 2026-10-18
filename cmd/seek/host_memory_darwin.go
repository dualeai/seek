//go:build darwin

package main

import "golang.org/x/sys/unix"

// hostPhysicalMemory returns the host's physical memory in bytes, or zero when
// the system does not report it. It is one sysctl call, so process start stays
// fast.
func hostPhysicalMemory() int64 {
	size, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(size)
}
