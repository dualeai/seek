package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// For the common non-worktree case, it walks up the directory tree to find
// .git, avoiding a ~5-10ms subprocess call. Falls back to git rev-parse for
// worktrees (where .git is a file, not a directory) and edge cases.
func resolveGitPathsFromCWD(ctx context.Context) (gitPaths, error) {
	if paths, ok := fastResolveGitPaths(); ok {
		return paths, nil
	}
	return resolveGitPaths(ctx, "")
}

// fastResolveGitPaths attempts to resolve git paths without spawning a
// subprocess. Walks up from CWD looking for a .git directory. Returns false
// when .git is a file (worktree), CWD cannot be determined, or no .git is
// found, causing the caller to fall back to git rev-parse.
func fastResolveGitPaths() (gitPaths, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return gitPaths{}, false
	}
	for {
		gitPath := filepath.Join(dir, ".git")
		fi, err := os.Lstat(gitPath)
		if err == nil {
			if fi.IsDir() {
				return gitPaths{
					RepoDir:     dir,
					GitDir:      gitPath,
					CommonDir:   gitPath,
					ExcludePath: filepath.Join(gitPath, "info", "exclude"),
					ConfigPath:  filepath.Join(gitPath, "config"),
				}, true
			}
			// .git is a file → worktree or submodule; fall back
			return gitPaths{}, false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return gitPaths{}, false
		}
		dir = parent
	}
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

// ensureFSMonitor enables the built-in filesystem monitor daemon if not
// already configured. The fsmonitor daemon uses OS-level file watchers
// (FSEvents on macOS, inotify on Linux) so git status can query a socket
// instead of lstat'ing every tracked file. On large repos this reduces
// git status from hundreds of milliseconds to single-digit ms. The
// setting is safe, reversible, and stored in .git/config (per-repo only).
//
// Requires Git 2.36+ where core.fsmonitor=true means "use the built-in
// daemon". On older versions this key expects a hook script path, and
// setting it to "true" would be misinterpreted (on Unix, /usr/bin/true
// exists and would silently disable change detection).
//
// Only called on first run (when no cached state exists), so the
// subprocess cost of version detection is amortized.
func ensureFSMonitor(ctx context.Context, paths gitPaths) {
	configPath := paths.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	if strings.Contains(string(data), "fsmonitor") {
		return
	}
	// Gate on Git 2.36+ to avoid misinterpretation of the boolean value.
	if !gitVersionAtLeast(ctx, paths.RepoDir, 2, 36) {
		return
	}
	cmd := gitCmd(ctx, "config", "core.fsmonitor", "true")
	cmd.Dir = paths.RepoDir
	_ = cmd.Run()
}

// gitVersionAtLeast returns true if the installed git version is at least
// major.minor. Returns false on any parse error (conservative).
func gitVersionAtLeast(ctx context.Context, dir string, major, minor int) bool {
	cmd := gitCmd(ctx, "version")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	// Parse "git version 2.43.0" or "git version 2.43.0.windows.1"
	s := strings.TrimSpace(string(out))
	s = strings.TrimPrefix(s, "git version ")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return maj > major || (maj == major && min >= minor)
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
