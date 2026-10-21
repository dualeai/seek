package main

/*
#include <stdlib.h>
#include <sys/attr.h>
#include <unistd.h>
*/
import "C"

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// onDiskPath returns path with the letter case that the file system stores.
// A directory takes it from one open and one F_GETPATH, with no directory
// listing. Any failure returns path unchanged. A file takes the case of its
// directory and its stored name from getattrlist, with no open (an open can
// download a cloud-only file) and, on APFS and HFS+, no listing (a listing of
// a large directory is slow).
func onDiskPath(path string) string {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		dir := onDiskPath(filepath.Dir(path))
		name, ok := "", false
		if namesFromEntries(path) {
			name, ok = storedName(path)
		}
		if !ok {
			// FAT32 and exFAT give getattrlist the name that the first lookup
			// typed: read the directory listing there.
			entries, _ := os.ReadDir(dir)
			name = realCaseEntry(dir, filepath.Base(path), entries)
		}
		return filepath.Join(dir, name)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return path
	}
	defer func() { _ = unix.Close(fd) }()
	var buf [unix.PathMax]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return path
	}
	if s := unix.ByteSliceToString(buf[:]); s != "" {
		return s
	}
	return path
}

// storedName returns the name that the file system stores for the last
// component of path: getattrlist(2) with ATTR_CMN_NAME reads it from the
// metadata, with no open and no directory listing.
func storedName(path string) (string, bool) {
	target := C.CString(path)
	defer C.free(unsafe.Pointer(target))
	attrs := C.struct_attrlist{bitmapcount: C.ATTR_BIT_MAP_COUNT, commonattr: C.ATTR_CMN_NAME}
	// The reply: a uint32 length, then an attrreference_t (int32 offset from
	// the reference, uint32 length with the NUL), then the name.
	var reply [4 + 8 + 4*unix.PathMax]byte
	if C.getattrlist(target, unsafe.Pointer(&attrs), unsafe.Pointer(&reply[0]), C.size_t(len(reply)), C.FSOPT_NOFOLLOW) != 0 {
		return "", false
	}
	offset := int(*(*int32)(unsafe.Pointer(&reply[4])))
	length := int(*(*uint32)(unsafe.Pointer(&reply[8])))
	start := 4 + offset
	if offset < 8 || length < 1 || start+length > len(reply) {
		return "", false
	}
	name := string(reply[start : start+length-1])
	if name == "" {
		return "", false
	}
	return name, true
}

// namesFromEntries reports a file system whose getattrlist name comes from the
// directory entry, so it is the stored name: APFS and HFS+.
func namesFromEntries(path string) bool {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return false
	}
	name := unix.ByteSliceToString(fs.Fstypename[:])
	return name == "apfs" || name == "hfs"
}
