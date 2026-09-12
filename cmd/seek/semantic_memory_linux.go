//go:build linux

package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func semanticHostAvailableMemory() int64 {
	content, err := os.ReadFile("/proc/meminfo")
	var hostAvailable int64
	if err == nil {
		hostAvailable = parseLinuxMemAvailable(content)
	}
	cgroupAvailable := linuxCgroupAvailableMemory()
	switch {
	case hostAvailable > 0 && cgroupAvailable > 0:
		return min(hostAvailable, cgroupAvailable)
	case cgroupAvailable > 0:
		return cgroupAvailable
	default:
		return hostAvailable
	}
}

func parseLinuxMemAvailable(content []byte) int64 {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "MemAvailable:" {
			continue
		}
		kilobytes, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kilobytes <= 0 || kilobytes > (1<<63-1)/1024 {
			return 0
		}
		return kilobytes * 1024
	}
	return 0
}

func linuxCgroupAvailableMemory() int64 {
	content, _ := os.ReadFile("/proc/self/cgroup")
	var available int64
	for _, pair := range linuxMemoryCgroupFiles(content) {
		limit, limitOK := readLinuxMemoryValue(pair[0])
		used, usedOK := readLinuxMemoryValue(pair[1])
		if !limitOK || !usedOK {
			continue
		}
		remaining := limit - used
		if used >= limit {
			remaining = 1
		}
		if available == 0 || remaining < available {
			available = remaining
		}
	}
	return available
}

func linuxMemoryCgroupFiles(content []byte) [][2]string {
	const root = "/sys/fs/cgroup"
	var v1Path string
	for _, line := range strings.Split(string(content), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		relative := strings.TrimPrefix(filepath.Clean("/"+parts[2]), "/")
		if parts[0] == "0" && parts[1] == "" {
			return linuxMemoryHierarchyFiles(root, relative, "memory.max", "memory.current")
		}
		for _, controller := range strings.Split(parts[1], ",") {
			if controller == "memory" {
				v1Path = relative
				break
			}
		}
	}
	files := [][2]string{{filepath.Join(root, "memory.max"), filepath.Join(root, "memory.current")}}
	return append(files, linuxMemoryHierarchyFiles(
		filepath.Join(root, "memory"),
		v1Path,
		"memory.limit_in_bytes",
		"memory.usage_in_bytes",
	)...)
}

func linuxMemoryHierarchyFiles(root, relative, limitName, usedName string) [][2]string {
	relative = strings.TrimPrefix(filepath.Clean("/"+relative), "/")
	files := make([][2]string, 0, strings.Count(relative, string(filepath.Separator))+1)
	for {
		dir := filepath.Join(root, relative)
		files = append(files, [2]string{
			filepath.Join(dir, limitName),
			filepath.Join(dir, usedName),
		})
		if relative == "" || relative == "." {
			return files
		}
		parent := filepath.Dir(relative)
		if parent == "." {
			parent = ""
		}
		relative = parent
	}
}

func readLinuxMemoryValue(path string) (int64, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value := strings.TrimSpace(string(content))
	if value == "max" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	// cgroup v1 uses values near MaxInt64 for an unlimited group.
	if err != nil || parsed < 0 || parsed >= 1<<60 {
		return 0, false
	}
	return parsed, true
}
