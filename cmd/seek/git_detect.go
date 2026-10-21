package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// detectStatus is the three-state result of detectGitBoundary.
//
// boundaryConfirmed: definitive evidence of a git repo (HEAD + objects +
// refs all present).
// notBoundary: definitive evidence there is no boundary here.
// ambiguous: insufficient evidence; the caller, classifyGitDir, asks Git.
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
// only filesystem syscalls (no subprocess). Walker-safe. A confirmed boundary
// comes with the Git paths of its node: absDir and the common Git directory.
//
// Algorithm:
//
//	L1: Lstat(<dir>/.git). Missing -> notBoundary. A symlink or a stat error
//	    -> ambiguous: Git follows a symlinked .git, so Git decides.
//	L2a: .git is a directory -> validate <gitDir>/HEAD (regular file) AND
//	     <gitDir>/objects/ (dir) AND <gitDir>/refs/ (dir). All three present
//	     -> boundaryConfirmed, with <dir>/.git as the common directory.
//	     Missing any, or a <gitDir>/commondir file -> ambiguous.
//	L2b: .git is a regular file -> detectGitFileBoundary.
func detectGitBoundary(absDir string) (gitPaths, detectStatus) {
	gitPath := filepath.Join(absDir, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Most directories have no `.git`: one syscall decides.
			return gitPaths{}, notBoundary
		}
		// Permission denied, I/O error, etc. — cannot prove negative.
		return gitPaths{}, ambiguous
	}
	mode := fi.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return gitPaths{}, ambiguous
	case mode.IsDir():
		if !hasGitTriad(gitPath, gitPath) {
			return gitPaths{}, ambiguous
		}
		if _, err := os.Lstat(filepath.Join(gitPath, "commondir")); err == nil {
			// The objects and refs live elsewhere: Git decides.
			return gitPaths{}, ambiguous
		}
		return gitPaths{RepoDir: absDir, CommonDir: gitPath}, boundaryConfirmed
	case mode.IsRegular():
		return detectGitFileBoundary(absDir, gitPath)
	default:
		return gitPaths{}, notBoundary
	}
}

// detectGitFileBoundary confirms a `.git` file whose target proves that it
// belongs to absDir: a linked worktree whose back-reference names this `.git`
// file, a submodule Git directory whose core.worktree names absDir, or an
// in-tree Git directory, each with the full triad. Any other target is
// ambiguous, so Git decides.
func detectGitFileBoundary(absDir, gitFile string) (gitPaths, detectStatus) {
	resolved, err := parseGitFilePointer(gitFile, absDir)
	if err != nil {
		return gitPaths{}, ambiguous
	}
	resolvedReal, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return gitPaths{}, ambiguous
	}
	commonDir := readCommonDir(resolved)
	if commonDir != resolved {
		if !hasLinkedWorktreeBackref(resolvedReal, commonDir, gitFile) || !hasGitTriad(resolvedReal, commonDir) {
			return gitPaths{}, ambiguous
		}
		return gitPaths{RepoDir: absDir, CommonDir: commonDir}, boundaryConfirmed
	}
	inTree := pathWithin(canonicalCorpusPath(absDir), resolvedReal)
	if (!inTree && !moduleWorktreeIs(resolvedReal, absDir)) || !hasGitTriad(resolvedReal, resolvedReal) {
		return gitPaths{}, ambiguous
	}
	return gitPaths{RepoDir: absDir, CommonDir: resolved}, boundaryConfirmed
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
	text, err := readGitFileNoFollow(path)
	if err != nil {
		return "", false
	}
	inCore := false
	for line := range strings.Lines(text) {
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

// hasGitTriad verifies the structural minimum Git uses to recognize a
// repository: a HEAD regular file and a refs directory in gitDir, and an
// objects directory in commonDir (gitDir itself for a normal repository).
// It uses Lstat for each entry: a symlinked entry cannot redirect the check
// to files outside the repository.
func hasGitTriad(gitDir, commonDir string) bool {
	head, err := os.Lstat(filepath.Join(gitDir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	refs, err := os.Lstat(filepath.Join(gitDir, "refs"))
	if err != nil || !refs.IsDir() {
		return false
	}
	objects, err := os.Lstat(filepath.Join(commonDir, "objects"))
	return err == nil && objects.IsDir()
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

// readGitFileNoFollow opens path with O_NOFOLLOW (defense against a TOCTOU
// Lstat→Open symlink swap on `.git`-name entries) and reads at most
// maxGitFileBytes, so a pathological multi-GB file cannot OOM seek.
func readGitFileNoFollow(path string) (string, error) {
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
	return string(buf[:n]), nil
}

// readFirstLineNoFollow reads path (readGitFileNoFollow) and returns its
// first line trimmed of trailing \r\n + whitespace. Empty string is returned
// on any read error so callers can branch: parseGitFilePointer treats it as
// fatal; readGitPathFile treats it as no path.
//
// NUL-byte rejection is NOT done here — parseGitFilePointer surfaces
// it as a specific error while readGitPathFile silently degrades.
func readFirstLineNoFollow(path string) (string, error) {
	text, err := readGitFileNoFollow(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimRight(line, "\r\n \t"), nil
}

// readGitPathFile reads the path that the Git metadata file dir/name holds on
// its first line; a relative path is joined to dir. It reports false when the
// file cannot be read, or when the line is empty or holds a NUL byte.
func readGitPathFile(dir, name string) (string, bool) {
	line, err := readFirstLineNoFollow(filepath.Join(dir, name))
	if err != nil || line == "" || strings.ContainsRune(line, 0) {
		return "", false
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(dir, line)
	}
	return line, true
}

// hasLinkedWorktreeBackref reports a linked worktree whose private Git
// directory names gitFile in its gitdir back-reference. The compare is by file
// identity, so the typed letter case or a symlink in the path does not matter.
func hasLinkedWorktreeBackref(gitDirReal, commonDir, gitFile string) bool {
	worktreesDir := filepath.Join(canonicalCorpusPath(commonDir), "worktrees")
	if gitDirReal == worktreesDir || !pathWithin(worktreesDir, gitDirReal) {
		return false
	}
	line, ok := readGitPathFile(gitDirReal, "gitdir")
	if !ok {
		return false
	}
	backref, err := os.Stat(line)
	if err != nil {
		return false
	}
	self, err := os.Stat(gitFile)
	return err == nil && os.SameFile(backref, self)
}

// readCommonDir reads <gitDir>/commondir (a single short line containing
// a path) and returns the resolved common directory. Returns gitDir
// unchanged if commondir is absent or unreadable. This is the common case for
// a normal repository, where the resolved .git directory is the common dir.
func readCommonDir(gitDir string) string {
	if commonDir, ok := readGitPathFile(gitDir, "commondir"); ok {
		return filepath.Clean(commonDir)
	}
	return gitDir
}
