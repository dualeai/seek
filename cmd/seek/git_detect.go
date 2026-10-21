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

// gitBoundary describes a confirmed git repository boundary. The fields give
// the walker and planner enough data to construct a corpusPlan without a
// subprocess.
type gitBoundary struct {
	RepoDir   string   // working tree root
	CommonDir string   // shared .git common dir; the .git directory for normal repos
	Mode      rootType // rootTypeDirectory for normal repos, rootTypeWorktree for file-form .git
}

// detectStatus is the three-state result of detectGitBoundary.
//
// boundaryConfirmed: definitive evidence of a git repo (HEAD + objects + refs all present).
// notBoundary: definitive evidence there is no boundary here.
// ambiguous: insufficient evidence; callers ask Git (operandRepo,
// classifyGitDir).
type detectStatus int

const (
	notBoundary detectStatus = iota
	boundaryConfirmed
	ambiguous
)

// maxGitFileBytes bounds one read of a `.git` pointer file or a Git metadata
// file (commondir, gitdir, config). It is a hostile-input guard, not a path
// limit: PATH_MAX (4,096 on Linux, 1,024 on darwin) does not limit path length
// on disk, so a valid `gitdir:` line can exceed it. A `.git` file of many GiB
// cannot exhaust memory.
const maxGitFileBytes = 64 << 10

// detectGitBoundary classifies a directory as a git-repo boundary using
// only filesystem syscalls (no subprocess). Walker-safe.
//
// Algorithm:
//
//	L1: Lstat(<dir>/.git). Missing -> notBoundary. A symlink or a stat error
//	    -> ambiguous: Git follows a symlinked .git, so Git decides.
//	L2a: .git is a directory -> validate <gitDir>/HEAD (regular file) AND
//	     <gitDir>/objects/ (dir) AND <gitDir>/refs/ (dir). All three present
//	     -> boundaryConfirmed (rootTypeDirectory). Missing any, or a
//	     <gitDir>/commondir file -> ambiguous.
//	L2b: .git is a regular file -> detectGitFileBoundary.
func detectGitBoundary(absDir string) (gitBoundary, detectStatus) {
	gitPath := filepath.Join(absDir, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Most directories have no `.git`: one syscall decides.
			return gitBoundary{}, notBoundary
		}
		// Permission denied, I/O error, etc. — cannot prove negative.
		return gitBoundary{}, ambiguous
	}
	mode := fi.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return gitBoundary{}, ambiguous
	case mode.IsDir():
		if !hasGitTriad(gitPath) {
			return gitBoundary{}, ambiguous
		}
		if _, err := os.Lstat(filepath.Join(gitPath, "commondir")); err == nil {
			// The objects and refs live elsewhere: Git decides.
			return gitBoundary{}, ambiguous
		}
		return gitBoundary{
			RepoDir:   absDir,
			CommonDir: gitPath,
			Mode:      rootTypeDirectory,
		}, boundaryConfirmed
	case mode.IsRegular():
		return detectGitFileBoundary(absDir, gitPath)
	default:
		return gitBoundary{}, notBoundary
	}
}

// detectGitFileBoundary confirms a `.git` file whose target proves that it
// belongs to absDir: a linked worktree whose back-reference names this `.git`
// file, a submodule Git directory whose core.worktree names absDir, or an
// in-tree Git directory, each with the full triad. Any other target is
// ambiguous, so Git decides.
func detectGitFileBoundary(absDir, gitFile string) (gitBoundary, detectStatus) {
	resolved, err := parseGitFilePointer(gitFile, absDir)
	if err != nil {
		return gitBoundary{}, ambiguous
	}
	resolvedReal, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return gitBoundary{}, ambiguous
	}
	commonDir := readCommonDir(resolved)
	if commonDir != resolved {
		if !hasLinkedWorktreeBackref(resolvedReal, commonDir, gitFile) || !hasGitWorktreeTriad(resolvedReal, commonDir) {
			return gitBoundary{}, ambiguous
		}
		return gitBoundary{RepoDir: absDir, CommonDir: commonDir, Mode: rootTypeWorktree}, boundaryConfirmed
	}
	inTree := pathWithin(realOrClean(absDir), resolvedReal)
	if (!inTree && !moduleWorktreeIs(resolvedReal, absDir)) || !hasGitTriad(resolvedReal) {
		return gitBoundary{}, ambiguous
	}
	return gitBoundary{RepoDir: absDir, CommonDir: resolved, Mode: rootTypeWorktree}, boundaryConfirmed
}

// moduleWorktreeIs reports a Git directory whose core.worktree names dir. Git
// writes this setting for a submodule whose Git directory lives in the
// superproject, in any superproject layout. A `.git` file that points to the
// Git directory of another directory is not confirmed.
func moduleWorktreeIs(gitDir, dir string) bool {
	worktree, ok := readCoreWorktree(filepath.Join(gitDir, "config"))
	if !ok {
		return false
	}
	if !filepath.IsAbs(worktree) {
		worktree = filepath.Join(gitDir, worktree)
	}
	return sameFile(worktree, dir)
}

// readCoreWorktree reads core.worktree from a Git config file. It reads only
// the plain form that Git writes; any other form gives false, and Git decides.
func readCoreWorktree(path string) (string, bool) {
	f, err := openNoFollow(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	var buf [maxGitFileBytes]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false
	}
	inCore := false
	for line := range strings.Lines(string(buf[:n])) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inCore = strings.EqualFold(line, "[core]")
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !inCore || !ok || !strings.EqualFold(strings.TrimSpace(key), "worktree") {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\"\\#;") {
			return "", false
		}
		return value, true
	}
	return "", false
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
// (HEAD + objects/ + refs/ at the directory itself, no `.git` child). The
// operand check and the Git status parse use it; the folder walk reads the
// same marks from the directory listing it already has (gitListingMarks).
func detectBareRepoAt(absDir string) bool {
	if _, err := os.Lstat(filepath.Join(absDir, ".git")); err == nil {
		return false
	}
	for _, sub := range []string{"objects", "refs"} {
		if fi, err := os.Lstat(filepath.Join(absDir, sub)); err != nil || !fi.IsDir() {
			return false
		}
	}
	// HEAD can be a symbolic link into refs/ (validGitHead): this check only
	// makes Seek skip the directory, so it accepts what Git accepts.
	return validGitHead(filepath.Join(absDir, "HEAD"))
}

// openNoFollow opens path read-only without following a terminal symlink.
// Defense-in-depth for the TOCTOU window between Lstat (which classified
// the entry as a regular file) and the subsequent Open (which would
// otherwise follow if an attacker swapped the entry into a symlink).
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// parseGitFilePointer reads a worktree-form `.git` file and returns the
// resolved absolute gitdir. Bounded to one maxGitFileBytes read, so a
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

// hasLinkedWorktreeBackref reports a linked worktree whose private Git
// directory names gitFile in its gitdir back-reference. The compare is by file
// identity, so the typed letter case or a symlink in the path does not matter.
func hasLinkedWorktreeBackref(gitDirReal, commonDir, gitFile string) bool {
	worktreesDir := filepath.Join(realOrClean(commonDir), "worktrees")
	if gitDirReal == worktreesDir || !pathWithin(worktreesDir, gitDirReal) {
		return false
	}
	line, err := readFirstLineNoFollow(filepath.Join(gitDirReal, "gitdir"))
	if err != nil || line == "" || strings.ContainsRune(line, 0) {
		return false
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(gitDirReal, line)
	}
	backref, err := os.Stat(line)
	if err != nil {
		return false
	}
	self, err := os.Stat(gitFile)
	return err == nil && os.SameFile(backref, self)
}

func realOrClean(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// readCommonDir reads <gitDir>/commondir (a single short line containing
// a path) and returns the resolved common directory. Returns gitDir
// unchanged if commondir is absent or unreadable. This is the common case for
// a normal repository, where the resolved .git directory is the common dir.
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

// toGitPaths returns the Git paths of a confirmed boundary without a
// subprocess.
func (b gitBoundary) toGitPaths() gitPaths {
	return gitPaths{RepoDir: b.RepoDir, CommonDir: b.CommonDir}
}
