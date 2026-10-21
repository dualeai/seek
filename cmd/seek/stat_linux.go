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

// statFlags returns 0: a Linux Stat_t has no file flags.
func statFlags(syscall.Stat_t) uint32 {
	return 0
}

// statDatalessFlags reports false: Linux has no cloud-only file flag.
func statDatalessFlags(uint32) bool {
	return false
}

// statDataless reports false: Linux has no cloud-only file flag.
func statDataless(*unix.Stat_t) bool {
	return false
}
