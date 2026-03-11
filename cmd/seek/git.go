package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// repoState holds the parsed result of a single git status call.
type repoState struct {
	HeadSHA   string   // commit SHA from # branch.oid, or "no-head"
	RawOutput string   // full raw output for state hashing
	Files     []string // paths of changed/untracked files
}

// gitCmd creates an exec.Cmd for git with graceful shutdown.
// Uses a graceful signal on context cancellation so git can release locks.
// Note: we intentionally do NOT set GIT_OPTIONAL_LOCKS=0. While it
// prevents lock contention on .git/index, it also prevents git from
// refreshing its stat cache, which can cause same-second edits to be
// invisible (same mtime + same size = git thinks file is unchanged).
func gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
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
// git status --porcelain=v2 --branch --no-renames -z command. This eliminates
// the TOCTOU window between separate git rev-parse and git status calls.
func gitRepoState(ctx context.Context) repoState {
	out, err := gitCmd(ctx, "status", "--porcelain=v2", "--branch", "--no-renames", "-z").Output()
	if err != nil {
		return repoState{HeadSHA: "no-head"}
	}
	return parseGitStatusV2(string(out))
}

// gitRepoStateIn returns the repository state for a specific directory.
// Used when the CWD may not be inside the target repository (e.g., post-
// indexing verification in runIndexing).
func gitRepoStateIn(ctx context.Context, dir string) repoState {
	cmd := gitCmd(ctx, "status", "--porcelain=v2", "--branch", "--no-renames", "-z")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return repoState{HeadSHA: "no-head"}
	}
	return parseGitStatusV2(string(out))
}

// parseGitStatusV2 parses git status --porcelain=v2 --branch --no-renames -z output.
// With -z, ALL records (headers and entries) are NUL-terminated.
func parseGitStatusV2(raw string) repoState {
	state := repoState{
		HeadSHA:   "no-head",
		RawOutput: raw,
	}

	seen := make(map[string]bool)
	pos := 0

	// Parse NUL-terminated records. With -z, both header lines (# ...)
	// and entry records use NUL as the terminator.
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

		// Header lines start with '#'
		if entry[0] == '#' {
			if strings.HasPrefix(entry, "# branch.oid ") {
				state.HeadSHA = entry[len("# branch.oid "):]
			}
			continue
		}

		var path string
		switch entry[0] {
		case '?': // untracked: "? <path>"
			path = entry[2:]
		case '1': // changed: "1 XY sub mH mI mW hH hI <path>"
			path = extractV2Path(entry, 8)
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

// ensureGitExclude adds the cache directory to .git/info/exclude if not
// already present. This prevents the cache from appearing as untracked in
// git status, which would cause the state hash to drift between pre- and
// post-indexing verification. Uses .git/info/exclude rather than .gitignore
// to avoid modifying the user's working tree.
func ensureGitExclude(repoDir, pattern string) {
	infoDir := filepath.Join(repoDir, ".git", "info")
	excludePath := filepath.Join(infoDir, "exclude")

	data, _ := os.ReadFile(excludePath)
	needle := "/" + pattern
	if strings.Contains(string(data), needle) {
		return
	}

	_ = os.MkdirAll(infoDir, 0o755)
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("Failed to update .git/info/exclude — cache directory may cause repeated re-indexing", "error", err)
		return
	}
	defer func() { _ = f.Close() }()
	// Add newline before if file doesn't end with one
	if len(data) > 0 && data[len(data)-1] != '\n' {
		_, _ = f.WriteString("\n")
	}
	_, _ = f.WriteString(needle + "\n")
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
