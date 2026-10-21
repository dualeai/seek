package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// statMtimeNano returns the modification time in nanoseconds from a Stat_t.
func statMtimeNano(s syscall.Stat_t) int64 {
	return s.Mtimespec.Nano()
}

// statCtimeNano returns the status change time in nanoseconds from a Stat_t.
func statCtimeNano(s syscall.Stat_t) int64 {
	return s.Ctimespec.Nano()
}

// statFlags returns the file flags (st_flags) from a Stat_t.
func statFlags(s syscall.Stat_t) uint32 {
	return s.Flags
}

// sfDataless is SF_DATALESS (sys/stat.h): a cloud-only file whose content is
// not on disk.
const sfDataless = 0x40000000

// statDatalessFlags reports whether flags hold SF_DATALESS.
func statDatalessFlags(flags uint32) bool {
	return flags&sfDataless != 0
}

// statDataless reports whether st is a dataless file.
func statDataless(st *unix.Stat_t) bool {
	return statDatalessFlags(st.Flags)
}
