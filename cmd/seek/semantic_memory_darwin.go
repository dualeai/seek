//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"os/exec"
	"strconv"
	"strings"
)

func semanticHostAvailableMemory() int64 {
	output, err := exec.Command("/usr/bin/vm_stat").Output()
	if err != nil {
		return 0
	}
	pageBytes := int64(4096)
	var pages int64
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics:") {
			if start := strings.Index(line, "page size of "); start >= 0 {
				value := line[start+len("page size of "):]
				if end := strings.IndexByte(value, ' '); end >= 0 {
					value = value[:end]
				}
				if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && parsed > 0 {
					pageBytes = parsed
				}
			}
			continue
		}
		for _, prefix := range []string{
			"Pages free:",
			"Pages inactive:",
			"Pages speculative:",
			"Pages purgeable:",
		} {
			if value, ok := strings.CutPrefix(line, prefix); ok {
				value = strings.TrimSuffix(strings.TrimSpace(value), ".")
				if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && parsed > 0 {
					pages += parsed
				}
				break
			}
		}
	}
	if pages <= 0 || pageBytes <= 0 || pages > (1<<63-1)/pageBytes {
		return 0
	}
	return pages * pageBytes
}
