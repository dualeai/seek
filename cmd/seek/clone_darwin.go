package main

import "golang.org/x/sys/unix"

// tryCloneFile attempts a copy-on-write clone using macOS clonefile(2).
// Returns true if the clone succeeded, false if unsupported or failed.
func tryCloneFile(src, dst string) bool {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) == nil
}
