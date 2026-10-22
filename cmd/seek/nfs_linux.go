package main

import "syscall"

// Linux statfs f_type magic numbers of network file systems. The list includes
// FUSE because sshfs, rclone, and other FUSE file systems act as network file
// systems do: no flock guarantee, no atomic rename across clients, and mtime
// skew.
const (
	nfsSuperMagic  = 0x6969
	smbSuperMagic  = 0x517B
	smb2MagicNum   = 0xFE534D42
	cifsMagicNum   = 0xFF534D42
	fuseSuperMagic = 0x65735546
)

// isOnNFS reports whether path is on a file system with one of the magic
// numbers above. A stat error reports false.
func isOnNFS(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	switch uint32(st.Type) {
	case nfsSuperMagic, smbSuperMagic, smb2MagicNum, cifsMagicNum, fuseSuperMagic:
		return true
	}
	return false
}
