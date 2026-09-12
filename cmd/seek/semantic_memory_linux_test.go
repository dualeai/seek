//go:build linux

package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseLinuxMemAvailable(t *testing.T) {
	content := []byte("MemTotal: 1000 kB\nMemAvailable: 750 kB\n")
	if got := parseLinuxMemAvailable(content); got != 750*1024 {
		t.Fatalf("available memory = %d, want %d", got, 750*1024)
	}
}

func TestLinuxMemoryCgroupFiles(t *testing.T) {
	files := linuxMemoryCgroupFiles([]byte("0::/worker.slice/job\n"))
	want := [][2]string{
		{
			filepath.Join("/sys/fs/cgroup", "worker.slice/job", "memory.max"),
			filepath.Join("/sys/fs/cgroup", "worker.slice/job", "memory.current"),
		},
		{
			filepath.Join("/sys/fs/cgroup", "worker.slice", "memory.max"),
			filepath.Join("/sys/fs/cgroup", "worker.slice", "memory.current"),
		},
		{
			filepath.Join("/sys/fs/cgroup", "memory.max"),
			filepath.Join("/sys/fs/cgroup", "memory.current"),
		},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("cgroup files = %v, want %v", files, want)
	}
}

func TestLinuxMemoryCgroupFilesRejectsParentTraversal(t *testing.T) {
	files := linuxMemoryCgroupFiles([]byte("0::/../../outside\n"))
	want := filepath.Join("/sys/fs/cgroup", "outside", "memory.max")
	if len(files) != 2 || files[0][0] != want {
		t.Fatalf("cgroup limit = %v, want %s", files, want)
	}
}
