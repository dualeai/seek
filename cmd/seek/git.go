package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	RepoDir    string
	CommonDir  string
	ConfigPath string
}

type gitUnavailableError struct {
	cause error
}

func (e *gitUnavailableError) Error() string {
	return fmt.Sprintf("Git executable is unavailable: %v", e.cause)
}

func (e *gitUnavailableError) Unwrap() error {
	return e.cause
}

// gitCmd creates a Git command that uses a graceful signal on cancellation so
// Git can release its locks. It does not set GIT_OPTIONAL_LOCKS=0 because Git
// must refresh its stat cache to detect some same-second edits.
func gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	if errors.Is(cmd.Err, exec.ErrNotFound) {
		cmd.Err = &gitUnavailableError{cause: cmd.Err}
	}
	cmd.Env = sanitizedGitEnv(os.Environ())
	cmd.Cancel = func() error {
		return cmd.Process.Signal(gitCancelSignal())
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

// gitLocationEnv names the variables that redirect git at a different
// repository than the one Seek resolved from the filesystem. Seek removes them
// from all Git commands.
var gitLocationEnv = [5]string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR",
}

func sanitizedGitEnv(env []string) []string {
	filtered := env[:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS",
			"GIT_TERMINAL_PROMPT", "LC_ALL", "LANG":
			continue
		// Corpus identity comes from the filesystem. A location variable could
		// make Git describe a different repository from the indexed corpus.
		case gitLocationEnv[0], gitLocationEnv[1], gitLocationEnv[2], gitLocationEnv[3], gitLocationEnv[4]:
			continue
		default:
			filtered = append(filtered, kv)
		}
	}
	filtered = append(filtered,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"LANG=C",
	)
	return filtered
}

// resolveGitPathsFromCWD resolves Git paths from the current directory. It
// first checks .git directories and worktree .git files without a subprocess.
// It uses git rev-parse when that check cannot give a clear result.
func resolveGitPathsFromCWD(ctx context.Context) (gitPaths, error) {
	if paths, ok := fastResolveGitPaths(); ok {
		return paths, nil
	}
	return resolveGitPaths(ctx, "")
}

// fastResolveGitPaths walks from the current directory to its ancestors and
// checks each Git boundary without a subprocess. It returns false when the
// current directory is unreadable, no boundary exists, or detection is unclear.
func fastResolveGitPaths() (gitPaths, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return gitPaths{}, false
	}
	for {
		b, status := detectGitBoundary(dir, "")
		if status == boundaryConfirmed {
			return b.toGitPaths(), true
		}
		if status == ambiguous {
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
	cmd := gitCmd(ctx,
		"rev-parse",
		"--path-format=absolute",
		"--show-toplevel",
		"--git-common-dir",
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
	if len(lines) != 3 {
		return gitPaths{}, fmt.Errorf("unexpected git rev-parse output: got %d lines", len(lines))
	}

	paths := gitPaths{
		RepoDir:    strings.TrimSpace(lines[0]),
		CommonDir:  strings.TrimSpace(lines[1]),
		ConfigPath: strings.TrimSpace(lines[2]),
	}
	return paths, nil
}

// gitRepoStateIn returns the repository state for a specific directory.
// Used when the CWD may not be inside the target repository.
func gitRepoStateIn(ctx context.Context, dir string) (repoState, error) {
	cmd := gitCmd(ctx, "status", "--porcelain=v2", "--branch", "--no-renames", "--no-ahead-behind", "--untracked-files=all", "-z")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return repoState{}, ctxErr
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return repoState{}, fmt.Errorf("git status: %w", err)
		}
		return repoState{}, fmt.Errorf("git status: %w: %s", err, msg)
	}
	return parseGitStatusV2(string(out)), nil
}

func gitHeadTreeish(ctx context.Context, dir string) (string, error) {
	cmd := gitCmd(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "no-head", nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("git rev-parse HEAD: %w", err)
		}
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// parseGitStatusV2 parses git status --porcelain=v2 --branch --no-renames -z output.
// With -z, ALL records (headers and entries) are NUL-terminated.
func parseGitStatusV2(raw string) repoState {
	state := repoState{
		HeadSHA:   "no-head",
		RawOutput: raw,
	}

	seen := make(map[string]struct{})
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
				if state.HeadSHA == "(initial)" {
					state.HeadSHA = "no-head"
				}
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

		if path != "" {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			state.Files = append(state.Files, path)
		}
	}

	return state
}

// ensureUntrackedCache enables core.untrackedCache if not already set.
// The untracked cache stores directory mtimes in the git index so that
// git status can skip unchanged directories. The setting is stored in the
// repository's .git/config and can be disabled there. Read that file directly
// before running `git config`.
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
