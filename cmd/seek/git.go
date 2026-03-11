package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitRepoRoot returns the absolute path to the git repository root.
func gitRepoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitHeadSHA returns the current HEAD SHA. Falls back to "no-head" on error.
func gitHeadSHA(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "no-head"
	}
	return strings.TrimSpace(string(out))
}

// gitStatusPorcelain returns the raw output of git status --porcelain.
func gitStatusPorcelain(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// parseGitStatusFiles extracts file paths from git status --porcelain output.
func parseGitStatusFiles(porcelainOutput string) []string {
	if porcelainOutput == "" {
		return nil
	}

	seen := make(map[string]bool)
	var files []string

	for _, line := range strings.Split(porcelainOutput, "\n") {
		if len(line) < 4 {
			continue
		}
		statusCode := line[0]
		path := line[3:]

		// Handle renames and copies: "R old -> new" or "C old -> new"
		if statusCode == 'R' || statusCode == 'C' {
			if idx := strings.Index(path, " -> "); idx >= 0 {
				path = path[idx+4:]
			}
		}

		// Handle quoted filenames
		path = unquoteGitPath(path)

		if path != "" && !seen[path] {
			seen[path] = true
			files = append(files, path)
		}
	}

	return files
}

// unquoteGitPath strips surrounding quotes and unescapes git-quoted filenames.
func unquoteGitPath(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	s = s[1 : len(s)-1]

	var buf bytes.Buffer
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				buf.WriteByte('\n')
			case 't':
				buf.WriteByte('\t')
			case '\\':
				buf.WriteByte('\\')
			case '"':
				buf.WriteByte('"')
			case 'a':
				buf.WriteByte('\a')
			case 'b':
				buf.WriteByte('\b')
			case 'f':
				buf.WriteByte('\f')
			case 'r':
				buf.WriteByte('\r')
			case 'v':
				buf.WriteByte('\v')
			default:
				// Octal escape: \NNN
				if s[i] >= '0' && s[i] <= '7' {
					val := int(s[i] - '0')
					for j := 0; j < 2 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '7'; j++ {
						i++
						val = val*8 + int(s[i]-'0')
					}
					buf.WriteByte(byte(val))
				} else {
					buf.WriteByte('\\')
					buf.WriteByte(s[i])
				}
			}
		} else {
			buf.WriteByte(s[i])
		}
	}
	return buf.String()
}

// deriveRepoPrefix computes the repository prefix from the git remote URL.
func deriveRepoPrefix(ctx context.Context, repoDir string) string {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
	out, err := cmd.Output()
	if err != nil {
		return filepath.Base(repoDir)
	}

	url := strings.TrimSpace(string(out))
	return stripRemoteURL(url)
}

// stripRemoteURL derives a repo prefix from a git remote URL.
func stripRemoteURL(url string) string {
	// Strip protocols
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://", "file://"} {
		url = strings.TrimPrefix(url, prefix)
	}

	// Strip user@ prefix (e.g., git@github.com:org/repo)
	if at := strings.Index(url, "@"); at >= 0 {
		url = url[at+1:]
	}

	// Replace colon with slash (git@github.com:org/repo -> github.com/org/repo)
	url = strings.Replace(url, ":", "/", 1)

	// Strip trailing .git
	url = strings.TrimSuffix(url, ".git")

	// Strip trailing slash
	url = strings.TrimSuffix(url, "/")

	return url
}
