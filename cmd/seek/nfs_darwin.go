//go:build darwin

package main

import "golang.org/x/sys/unix"

func isOnNFS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	name := fstypename(st.Fstypename[:])
	switch name {
	case "nfs", "smbfs", "webdav", "afpfs", "osxfuse", "macfuse", "fuse":
		return true
	}
	return false
}

func fstypename(buf []byte) string {
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}
