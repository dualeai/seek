package main

import (
	"context"
	"fmt"
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

type gitPaths struct {
	RepoDir     string
	GitDir      string
	CommonDir   string
	ExcludePath string
	ConfigPath  string
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

// resolveGitPathsFromCWD resolves git paths from the current working directory.
func resolveGitPathsFromCWD(ctx context.Context) (gitPaths, error) {
	return resolveGitPaths(ctx, "")
}

func resolveGitPaths(ctx context.Context, dir string) (gitPaths, error) {
	// --path-format=absolute requires Git 2.31+. Older Git falls back to
	// the legacy path assumption in fallbackGitPaths.
	cmd := gitCmd(ctx,
		"rev-parse",
		"--path-format=absolute",
		"--show-toplevel",
		"--git-dir",
		"--git-common-dir",
		"--git-path", "info/exclude",
		"--git-path", "config",
	)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return gitPaths{}, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 5 {
		return gitPaths{}, fmt.Errorf("unexpected git rev-parse output: got %d lines", len(lines))
	}

	paths := gitPaths{
		RepoDir:     strings.TrimSpace(lines[0]),
		GitDir:      strings.TrimSpace(lines[1]),
		CommonDir:   strings.TrimSpace(lines[2]),
		ExcludePath: strings.TrimSpace(lines[3]),
		ConfigPath:  strings.TrimSpace(lines[4]),
	}
	return paths, nil
}

func fallbackGitPaths(repoDir string) gitPaths {
	absRepoDir, err := filepath.Abs(repoDir)
	if err != nil {
		absRepoDir = repoDir
	}
	gitDir := filepath.Join(absRepoDir, ".git")
	return gitPaths{
		RepoDir:     absRepoDir,
		GitDir:      gitDir,
		CommonDir:   gitDir,
		ExcludePath: filepath.Join(gitDir, "info", "exclude"),
		ConfigPath:  filepath.Join(gitDir, "config"),
	}
}

// gitRepoState returns the current repository state using a single
// git status --porcelain=v2 --branch --no-renames -z command. This eliminates
// the TOCTOU window between separate git rev-parse and git status calls.
func gitRepoState(ctx context.Context) repoState {
	out, err := gitCmd(ctx, "status", "--porcelain=v2", "--branch", "--no-renames", "--no-ahead-behind", "-z").Output()
	if err != nil {
		return repoState{HeadSHA: "no-head"}
	}
	return parseGitStatusV2(string(out))
}

// gitRepoStateIn returns the repository state for a specific directory.
// Used when the CWD may not be inside the target repository.
func gitRepoStateIn(ctx context.Context, dir string) repoState {
	cmd := gitCmd(ctx, "status", "--porcelain=v2", "--branch", "--no-renames", "--no-ahead-behind", "-z")
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
		case '2': // renamed/copied: "2 XY sub mH mI mW hH hI Xscore <path>"
			// With -z, rename entries emit an additional NUL-terminated
			// record for the original path. Consume it so it's not
			// misinterpreted as a separate entry. This should not occur
			// with --no-renames but handles it defensively.
			path = extractV2Path(entry, 9)
			if nextEnd := strings.IndexByte(raw[pos:], 0); nextEnd >= 0 {
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

// ensureGitExclude adds the cache directory to .git/info/exclude if not
// already present. This prevents the cache from appearing as untracked in
// git status, which would pollute the state hash. Uses .git/info/exclude
// rather than .gitignore to avoid modifying the user's working tree.
func ensureGitExclude(paths gitPaths, pattern string) {
	infoDir := filepath.Dir(paths.ExcludePath)
	excludePath := paths.ExcludePath

	data, _ := os.ReadFile(excludePath)
	needle := "/" + pattern
	// Check for exact line match, not substring. Substring matching
	// could false-positive on patterns like /.seek-cache-old when
	// looking for /.seek-cache.
	for _, line := range strings.Split(string(data), "\n") {
		if line == needle {
			return
		}
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

// ensureUntrackedCache enables core.untrackedCache if not already set.
// The untracked cache stores directory mtimes in the git index so that
// git status can skip scanning unchanged directories. On a 17k-file repo
// this reduces git status from ~400ms to ~70ms (6-7x). The setting is
// safe, reversible, and stored in .git/config (per-repo only).
//
// Reads .git/config directly (~14µs) instead of spawning git config
// (~8ms) to avoid subprocess overhead on the hot path.
func ensureUntrackedCache(ctx context.Context, paths gitPaths) {
	configPath := paths.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	if strings.Contains(string(data), "untrackedCache") {
		return
	}
	cmd := gitCmd(ctx, "config", "core.untrackedCache", "true")
	cmd.Dir = paths.RepoDir
	_ = cmd.Run()
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
