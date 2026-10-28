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
//   - boundaryConfirmed: proof of a Git repository (HEAD, objects, and refs
//     are all present).
//   - notBoundary: proof that no boundary is here.
//   - ambiguous: no proof either way; the caller, classifyGitDir, asks Git.
type detectStatus int

const (
	notBoundary detectStatus = iota
	boundaryConfirmed
	ambiguous
)

// maxGitFileBytes bounds one read of a `.git` pointer file or a Git metadata
// file (commondir, gitdir, config). It guards against hostile input and is not
// a path limit. PATH_MAX (4,096 on Linux, 1,024 on darwin) does not limit the
// length of a path on disk, so a valid `gitdir:` line can be longer. A `.git`
// file of many GiB cannot use up the memory.
const maxGitFileBytes = 64 << 10

// detectGitBoundary classifies a directory as a Git repository boundary. It
// uses only file system calls and starts no process. So the graph walk can
// call it for each directory that holds a `.git` entry (classifyGitDir). A
// confirmed boundary comes with the Git paths of its node: absDir and the
// common Git directory.
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
//	L2c: .git is any other file type -> notBoundary.
func detectGitBoundary(absDir string) (gitPaths, detectStatus) {
	gitPath := filepath.Join(absDir, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Most directories have no `.git`: one syscall decides.
			return gitPaths{}, notBoundary
		}
		// Permission denied, an I/O error, and so on: Seek cannot prove that
		// there is no `.git`.
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
// belongs to absDir. Each such target has the full triad. It is a linked
// worktree whose back-reference names this `.git` file, or a submodule Git
// directory whose core.worktree names absDir, or an in-tree Git directory.
// Any other target is ambiguous, so Git decides.
func detectGitFileBoundary(absDir, gitFile string) (gitPaths, detectStatus) {
	resolved, err := parseGitFilePointer(gitFile)
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

// hasGitTriad checks the minimum structure of a repository. gitDir holds a
// HEAD regular file and a refs directory. commonDir holds an objects
// directory; it is gitDir itself for a normal repository. It uses Lstat for
// each entry, so a symlinked entry cannot send the check to files outside the
// repository.
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

// detectBareRepoAt reports whether absDir is a bare Git repository: HEAD,
// objects/, and refs/ in the directory itself, and no `.git` child. The
// operand check and the Git status parse use it. The folder walk reads the
// same entries from the directory listing that it already has
// (gitListingMarks), with no check of the HEAD content.
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

// openNoFollow opens path read-only and does not follow a symlink in the last
// component. It is a second guard for the time between the Lstat that found a
// regular file and the open. When the entry became a symlink in that time,
// the open fails and does not follow it.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// parseGitFilePointer reads a worktree-form `.git` file and returns the
// resolved absolute gitdir. It reads at most maxGitFileBytes, so a `.git` file
// of many GB cannot use up the memory of Seek.
//
// The file holds one line of the form `gitdir: <path>\n`. The path can be
// relative to the directory that holds the `.git` file, or absolute. A `\r\n`
// line end and spaces around the path are accepted. A missing prefix, an
// empty path, or a NUL byte in the path is an error.
func parseGitFilePointer(gitFile string) (string, error) {
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
		ptr = filepath.Join(filepath.Dir(gitFile), ptr)
	}
	return filepath.Clean(ptr), nil
}

// readGitFileNoFollow opens path with O_NOFOLLOW (openNoFollow) and returns
// at most its first maxGitFileBytes. A longer file gives no error, and a file
// of many GB cannot use up the memory of Seek.
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
// first line with no trailing \r, \n, space, or tab. On a read error it returns
// an empty string and the error: parseGitFilePointer returns the error, and
// readGitPathFile reports no path.
//
// NUL-byte rejection is NOT done here: parseGitFilePointer returns a specific
// error for it, and readGitPathFile reports no path.
func readFirstLineNoFollow(path string) (string, error) {
	text, err := readGitFileNoFollow(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimRight(line, "\r\n \t"), nil
}

// readGitPathFile reads the path that the Git metadata file dir/name holds on
// its first line, and joins a relative path to dir. It reports false when the
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
// directory, below <commonDir>/worktrees, names gitFile in its gitdir
// back-reference. The compare is by file identity, so the typed letter case or
// a symlink in the path does not matter.
func hasLinkedWorktreeBackref(gitDirReal, commonDir, gitFile string) bool {
	worktreesDir := filepath.Join(canonicalCorpusPath(commonDir), "worktrees")
	if gitDirReal == worktreesDir || !pathWithin(worktreesDir, gitDirReal) {
		return false
	}
	line, ok := readGitPathFile(gitDirReal, "gitdir")
	return ok && sameFile(line, gitFile)
}

// readCommonDir reads <gitDir>/commondir (one short line that holds a path)
// and returns the resolved common directory. It returns gitDir unchanged when
// commondir is absent, unreadable, empty, or holds a NUL byte. A normal
// repository has no commondir: its resolved .git directory is the common
// directory.
func readCommonDir(gitDir string) string {
	if commonDir, ok := readGitPathFile(gitDir, "commondir"); ok {
		return filepath.Clean(commonDir)
	}
	return gitDir
}
