//go:build linux

package main

import "syscall"

// Linux statfs f_type magic numbers for network filesystems.
// FUSE is included conservatively because sshfs/rclone/etc. surface as FUSE
// and exhibit similar non-local semantics (no flock guarantees, non-atomic
// rename across clients, mtime skew).
const (
	nfsSuperMagic  = 0x6969
	smbSuperMagic  = 0x517B
	smb2MagicNum   = 0xFE534D42
	cifsMagicNum   = 0xFF534D42
	fuseSuperMagic = 0x65735546
)

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
