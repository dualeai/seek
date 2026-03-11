//go:build !darwin && !linux

package main

// tryCloneFile is a no-op on platforms without clonefile/FICLONE support.
// The caller falls through to hardlink, then copy.
func tryCloneFile(_, _ string) bool {
	return false
}
