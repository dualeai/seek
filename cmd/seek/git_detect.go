package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// gitBoundary describes a confirmed git repository boundary. The fields
// mirror the subset of gitPaths the walker / planner needs to construct a
// corpusPlan without spawning a subprocess.
type gitBoundary struct {
	RepoDir   string   // working tree root
	GitDir    string   // resolved .git directory (worktree marker resolved)
	CommonDir string   // shared .git common dir; equals GitDir for non-worktree repos
	Mode      rootType // rootTypeDirectory for normal repos, rootTypeWorktree for file-form .git
}

// detectStatus is the three-state result of detectGitBoundary.
//
// boundaryConfirmed: definitive evidence of a git repo (HEAD + objects + refs all present).
// notBoundary: definitive evidence there is no boundary here.
// ambiguous: insufficient evidence; callers may fall back to subprocess (CLI) or
// treat as non-boundary (walker).
type detectStatus int

const (
	notBoundary detectStatus = iota
	boundaryConfirmed
	ambiguous
)

func (s detectStatus) String() string {
	switch s {
	case notBoundary:
		return "notBoundary"
	case boundaryConfirmed:
		return "boundaryConfirmed"
	case ambiguous:
		return "ambiguous"
	default:
		return fmt.Sprintf("detectStatus(%d)", int(s))
	}
}

// maxGitFileBytes caps reads of a `.git` worktree pointer file. The git
// format spec is a single short `gitdir: <path>` line; a pathological or
// hostile `.git` file can be arbitrary size. Reading more than this is
// pointless and would enable a memory-exhaustion DoS via os.ReadFile.
const maxGitFileBytes = 1024

// detectGitBoundary classifies a directory as a git-repo boundary using
// only filesystem syscalls (no subprocess). Walker-safe.
//
// Algorithm:
//
//	L1: Lstat(<dir>/.git). Symlink -> notBoundary. Missing -> notBoundary.
//	L2a: .git is a directory -> validate <gitDir>/HEAD (regular file) AND
//	     <gitDir>/objects/ (dir) AND <gitDir>/refs/ (dir). All three present
//	     -> boundaryConfirmed (rootTypeDirectory). Missing any -> ambiguous.
//	L2b: .git is a regular file -> parse worktree pointer (streaming, ≤1KiB).
//	     Resolve pointer; verify <resolved>/HEAD regular + objects + refs.
//	     Validate pointer is within scanRoot OR within an accepted commondir.
//	     -> boundaryConfirmed (rootTypeWorktree). Any failure -> ambiguous.
//
// scanRoot is the upper bound that worktree pointers must not escape.
// Callers pass the corpus scan root; a pointer that resolves outside it is
// treated as ambiguous (with a debug log at the call site).
func detectGitBoundary(absDir, scanRoot string) (gitBoundary, detectStatus) {
	gitPath := filepath.Join(absDir, ".git")
	absDirReal := ""
	scanRootReal := ""
	fi, err := os.Lstat(gitPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Hot path for the walker: most subdirs have no `.git` and
			// no triad. Return notBoundary in one syscall. Bare-repo
			// detection is deferred to detectBareRepoAt which corpus-
			// root entry calls once — moving it out of this hot path
			// shaved ~1 µs per call on the per-subdir probe.
			return gitBoundary{}, notBoundary
		}
		// Permission denied, I/O error, etc. — cannot prove negative.
		return gitBoundary{}, ambiguous
	}
	mode := fi.Mode()
	if mode&os.ModeSymlink != 0 {
		return gitBoundary{}, notBoundary
	}

	switch {
	case mode.IsDir():
		if !hasGitTriad(gitPath) {
			return gitBoundary{}, ambiguous
		}
		return gitBoundary{
			RepoDir:   absDir,
			GitDir:    gitPath,
			CommonDir: gitPath,
			Mode:      rootTypeDirectory,
		}, boundaryConfirmed
	case mode.IsRegular():
		resolved, err := parseGitFilePointer(gitPath, absDir)
		if err != nil {
			return gitBoundary{}, ambiguous
		}
		resolvedReal, err := filepath.EvalSymlinks(resolved)
		if err != nil {
			return gitBoundary{}, ambiguous
		}
		if absDirReal == "" {
			absDirReal = realOrClean(absDir)
		}
		if scanRoot != "" && scanRootReal == "" {
			scanRootReal = realOrClean(scanRoot)
		}
		if pointerWithinScopeReal(resolvedReal, absDirReal, scanRootReal) && !commonDirFileMaybePresent(resolvedReal) {
			if !hasGitTriad(resolvedReal) {
				return gitBoundary{}, ambiguous
			}
			return gitBoundary{
				RepoDir:   absDir,
				GitDir:    resolved,
				CommonDir: resolved,
				Mode:      rootTypeWorktree,
			}, boundaryConfirmed
		}
		commonDir := readCommonDir(resolved)
		commonDirReal := resolvedReal
		if commonDir != resolved {
			commonDirReal, err = filepath.EvalSymlinks(commonDir)
			if err != nil {
				return gitBoundary{}, ambiguous
			}
		}
		if !trustedGitPointerTargetReal(resolvedReal, commonDirReal, absDirReal, scanRootReal) {
			return gitBoundary{}, ambiguous
		}
		if !hasGitWorktreeTriad(resolvedReal, commonDirReal) {
			return gitBoundary{}, ambiguous
		}
		return gitBoundary{
			RepoDir:   absDir,
			GitDir:    resolved,
			CommonDir: commonDir,
			Mode:      rootTypeWorktree,
		}, boundaryConfirmed
	default:
		return gitBoundary{}, notBoundary
	}
}

// hasGitTriad verifies the structural minimum git uses internally to
// recognize a repo: HEAD regular file + objects/ dir + refs/ dir. All three
// are required. Matches `git rev-parse`'s internal sanity check. Uses
// Lstat (does NOT follow) for all three: refusing symlinked entries
// prevents attacker-controlled redirection to readable files/dirs outside
// the repo subtree.
func hasGitTriad(gitDir string) bool {
	head, err := os.Lstat(filepath.Join(gitDir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	for _, sub := range []string{"objects", "refs"} {
		fi, err := os.Lstat(filepath.Join(gitDir, sub))
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

func hasGitWorktreeTriad(gitDir, commonDir string) bool {
	head, err := os.Lstat(filepath.Join(gitDir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	refs, err := os.Lstat(filepath.Join(gitDir, "refs"))
	if err != nil || !refs.IsDir() {
		return false
	}
	objects, err := os.Lstat(filepath.Join(commonDir, "objects"))
	if err != nil || !objects.IsDir() {
		return false
	}
	return true
}

// detectBareRepoAt reports whether absDir is a bare git repository
// (HEAD + objects/ + refs/ at the directory itself, no `.git` child).
// Callers — typically the corpus-root entry point of a folder scan —
// use this to reject bare repos cleanly instead of silently indexing
// pack files as text. detectGitBoundary intentionally does NOT do this
// check on its hot path; running 3 extra Lstats per subdir during a
// walker would burn measurable wall-clock on large trees.
func detectBareRepoAt(absDir string) bool {
	if _, err := os.Lstat(filepath.Join(absDir, ".git")); err == nil {
		return false
	}
	return hasGitTriad(absDir)
}

// openNoFollow opens path read-only without following a terminal symlink.
// Defense-in-depth for the TOCTOU window between Lstat (which classified
// the entry as a regular file) and the subsequent Open (which would
// otherwise follow if an attacker swapped the entry into a symlink).
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// parseGitFilePointer reads a worktree-form `.git` file and returns the
// resolved absolute gitdir. Bounded to one fixed 1 KiB read, so a
// pathological multi-GB `.git` file cannot OOM seek.
//
// Spec: a single line of the form `gitdir: <path>\n`. Path may be relative
// (resolved against the directory holding the `.git` file) or absolute.
// Tolerate `\r\n` line endings and trailing whitespace.
func parseGitFilePointer(gitFile, parentDir string) (string, error) {
	line, err := readFirstLineNoFollow(gitFile)
	if err != nil {
		return "", err
	}
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("missing gitdir: prefix")
	}
	ptr := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if ptr == "" {
		return "", fmt.Errorf("empty gitdir pointer")
	}
	if strings.ContainsRune(ptr, 0) {
		return "", fmt.Errorf("NUL byte in gitdir pointer")
	}
	if !filepath.IsAbs(ptr) {
		ptr = filepath.Join(parentDir, ptr)
	}
	return filepath.Clean(ptr), nil
}

// readFirstLineNoFollow opens `path` with O_NOFOLLOW (defense against
// a TOCTOU Lstat→Open symlink swap on `.git`-name entries) and reads
// at most maxGitFileBytes into a fixed stack buffer, so a pathological
// multi-GB file cannot OOM seek. Returns the first line trimmed of
// trailing \r\n + whitespace. Empty string is returned on any read
// error so callers can branch: parseGitFilePointer treats it as
// fatal; readCommonDir treats it as "no commondir file, use gitDir".
//
// NUL-byte rejection is NOT done here — parseGitFilePointer surfaces
// it as a specific error while readCommonDir silently degrades.
func readFirstLineNoFollow(path string) (string, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var buf [maxGitFileBytes]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	line := buf[:n]
	if end := bytes.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	return strings.TrimRight(string(line), "\r\n \t"), nil
}

func commonDirFileMaybePresent(gitDir string) bool {
	_, err := os.Lstat(filepath.Join(gitDir, "commondir"))
	return err == nil || !os.IsNotExist(err)
}

// pointerWithinScopeReal reports whether a worktree pointer resolves inside a
// trustworthy in-scan location. Accepted scopes:
//
//   - inside the repo's own subtree (absDir, the directory holding `.git`)
//   - inside scanRoot (the corpus scan root passed by the caller)
//
// Normal linked worktrees where `.git` points at
// `<parentRepo>/.git/worktrees/<name>` are admitted by
// trustedGitPointerTargetReal after verifying Git's back-reference.
//
// scanRoot may be empty for CLI/operand callers that don't have a scan bound;
// in that case only pointers inside the worktree subtree are accepted by this
// helper. Out-of-tree pointers must prove they are real linked worktrees via
// trustedGitPointerTargetReal; otherwise detectGitBoundary returns ambiguous and
// CLI callers can fall back to git rev-parse.
func pointerWithinScopeReal(resolvedReal, absDirReal, scanRootReal string) bool {
	if pathWithin(absDirReal, resolvedReal) {
		return true
	}
	if scanRootReal == "" {
		return false
	}
	if pathWithin(scanRootReal, resolvedReal) {
		return true
	}
	return false
}

func trustedGitPointerTargetReal(resolvedReal, commonDirReal, absDirReal, scanRootReal string) bool {
	if resolvedReal != commonDirReal {
		return hasLinkedWorktreeBackrefReal(resolvedReal, commonDirReal, filepath.Join(absDirReal, ".git"))
	}
	return pointerWithinScopeReal(resolvedReal, absDirReal, scanRootReal)
}

func hasLinkedWorktreeBackrefReal(gitDir, commonDir, gitFileReal string) bool {
	if gitDir == commonDir {
		return false
	}
	worktreesDir := filepath.Join(commonDir, "worktrees")
	if gitDir == worktreesDir || !pathWithin(worktreesDir, gitDir) {
		return false
	}
	line, err := readFirstLineNoFollow(filepath.Join(gitDir, "gitdir"))
	if err != nil || line == "" || strings.ContainsRune(line, 0) {
		return false
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(gitDir, line)
	}
	return realOrClean(line) == gitFileReal
}

func realOrClean(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// readCommonDir reads <gitDir>/commondir (a single short line containing
// a path) and returns the resolved common directory. Returns gitDir
// unchanged if commondir is absent or unreadable — the common case for
// non-worktree repos where GitDir already IS the common dir.
func readCommonDir(gitDir string) string {
	line, err := readFirstLineNoFollow(filepath.Join(gitDir, "commondir"))
	if err != nil || line == "" || strings.ContainsRune(line, 0) {
		return gitDir
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(gitDir, line)
	}
	return filepath.Clean(line)
}

// toGitPaths upgrades a gitBoundary to the gitPaths shape used by older
// code paths. ConfigPath is derived from CommonDir at the call site
// without a subprocess.
func (b gitBoundary) toGitPaths() gitPaths {
	return gitPaths{
		RepoDir:    b.RepoDir,
		GitDir:     b.GitDir,
		CommonDir:  b.CommonDir,
		ConfigPath: filepath.Join(b.CommonDir, "config"),
	}
}
