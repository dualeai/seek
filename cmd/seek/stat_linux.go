package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// statMtimeNano returns the modification time in nanoseconds from a Stat_t.
func statMtimeNano(s syscall.Stat_t) int64 {
	return s.Mtim.Nano()
}

// statCtimeNano returns the status change time in nanoseconds from a Stat_t.
func statCtimeNano(s syscall.Stat_t) int64 {
	return s.Ctim.Nano()
}

// statDataless reports false: Linux has no cloud-only file flag.
func statDataless(*unix.Stat_t) bool {
	return false
}
