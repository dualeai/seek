package main

// onDiskPath returns path unchanged. Seek takes a Linux file system as case
// sensitive, so a path that resolves has the stored letter case.
func onDiskPath(path string) string {
	return path
}
