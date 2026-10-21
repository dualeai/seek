package main

import "golang.org/x/sys/unix"

// isOnNFS reports whether path is on a network or FUSE file system: NFS, SMB,
// WebDAV, AFP, or FUSE. A stat error reports false.
func isOnNFS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	switch unix.ByteSliceToString(st.Fstypename[:]) {
	case "nfs", "smbfs", "webdav", "afpfs", "osxfuse", "macfuse", "fuse":
		return true
	}
	return false
}
