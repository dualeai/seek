package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// repoState holds the parsed result of a single git status call.
type repoState struct {
	HeadSHA   string   // commit SHA from # branch.oid, or "no-head"
	RawOutput string   // full raw output for state hashing
	Files     []string // paths of changed/untracked files
}

// gitCmd creates an exec.Cmd for git with graceful shutdown and lock safety.
// Sets GIT_OPTIONAL_LOCKS=0 to prevent index lock contention, and uses
// a graceful signal on context cancellation so git can release locks.
func gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	cmd.Cancel = func() error {
		return cmd.Process.Signal(gitCancelSignal())
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

// gitRepoRoot returns the absolute path to the git repository root.
func gitRepoRoot(ctx context.Context) (string, error) {
	out, err := gitCmd(ctx, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRepoState returns the current repository state using a single
// git status --porcelain=v2 --branch -z command. This eliminates
// the TOCTOU window between separate git rev-parse and git status calls.
func gitRepoState(ctx context.Context) repoState {
	out, err := gitCmd(ctx, "status", "--porcelain=v2", "--branch", "-z").Output()
	if err != nil {
		return repoState{HeadSHA: "no-head"}
	}
	return parseGitStatusV2(string(out))
}

// parseGitStatusV2 parses git status --porcelain=v2 --branch -z output.
// Header lines (# ...) are LF-terminated. Entry records are NUL-terminated.
// Rename entries (type 2) have two NUL-terminated path fields.
func parseGitStatusV2(raw string) repoState {
	state := repoState{
		HeadSHA:   "no-head",
		RawOutput: raw,
	}

	seen := make(map[string]bool)
	pos := 0

	// Parse header lines (LF-terminated, start with #)
	for pos < len(raw) && raw[pos] == '#' {
		end := strings.IndexByte(raw[pos:], '\n')
		if end < 0 {
			break
		}
		line := raw[pos : pos+end]
		pos += end + 1

		if strings.HasPrefix(line, "# branch.oid ") {
			state.HeadSHA = line[len("# branch.oid "):]
		}
	}

	// Skip any stray whitespace between headers and entries
	for pos < len(raw) && (raw[pos] == '\n' || raw[pos] == '\r') {
		pos++
	}

	// Parse NUL-terminated entries
	for pos < len(raw) {
		end := strings.IndexByte(raw[pos:], 0)
		if end < 0 {
			break
		}
		entry := raw[pos : pos+end]
		pos += end + 1

		if len(entry) < 2 {
			continue
		}

		var path string
		switch entry[0] {
		case '?': // untracked: "? <path>"
			path = entry[2:]
		case '!': // ignored
			continue
		case '1': // changed: "1 XY sub mH mI mW hH hI <path>"
			path = extractV2Path(entry, 8)
		case '2': // renamed/copied: "2 XY sub mH mI mW hH hI Xscore <path>"
			path = extractV2Path(entry, 9)
			// Skip the origPath (next NUL-terminated field)
			nextEnd := strings.IndexByte(raw[pos:], 0)
			if nextEnd >= 0 {
				pos += nextEnd + 1
			}
		case 'u': // unmerged: "u XY sub m1 m2 m3 mW h1 h2 h3 <path>"
			path = extractV2Path(entry, 10)
		}

		if path != "" && !seen[path] {
			seen[path] = true
			state.Files = append(state.Files, path)
		}
	}

	return state
}

// extractV2Path extracts the path from a porcelain v2 entry by skipping
// the given number of space-separated fields.
func extractV2Path(entry string, skipFields int) string {
	idx := 0
	for i := 0; i < skipFields; i++ {
		space := strings.IndexByte(entry[idx:], ' ')
		if space < 0 {
			return ""
		}
		idx += space + 1
	}
	return entry[idx:]
}
