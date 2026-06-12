package main

import "syscall"

// statMtimeNano returns the modification time in nanoseconds from a Stat_t.
func statMtimeNano(s syscall.Stat_t) int64 {
	return s.Mtim.Nano()
}
