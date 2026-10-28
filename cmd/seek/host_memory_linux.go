package main

import "golang.org/x/sys/unix"

// hostPhysicalMemory returns the host's physical memory in bytes, or zero when
// the system does not report it. It is one sysinfo call, so process start stays
// fast.
func hostPhysicalMemory() int64 {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0
	}
	return int64(uint64(info.Totalram) * uint64(info.Unit))
}
