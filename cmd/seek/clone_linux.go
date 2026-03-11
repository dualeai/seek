package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// tryCloneFile attempts a copy-on-write clone using ioctl FICLONE.
// Returns true if the clone succeeded, false if unsupported or failed.
func tryCloneFile(src, dst string) bool {
	srcFile, err := os.Open(src)
	if err != nil {
		return false
	}
	defer func() { _ = srcFile.Close() }()

	dstFile, err := os.Create(dst)
	if err != nil {
		return false
	}
	defer func() { _ = dstFile.Close() }()

	err = unix.IoctlFileClone(int(dstFile.Fd()), int(srcFile.Fd()))
	if err != nil {
		// Clean up failed clone attempt
		_ = os.Remove(dst)
		return false
	}
	return true
}
