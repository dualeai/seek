package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// errGitDirectoryOperand rejects a path operand at or below a Git directory: a
// .git directory, a bare repository, or anything inside one. Its files are Git
// internals, not content.
var errGitDirectoryOperand = errors.New("path is inside a Git directory")

// plannedExternalOperands are the seed nodes of the graph expansion.
type plannedExternalOperands struct {
	gitRoots []externalGitRoot
	roots    []externalRoot
}

// externalGitRoot is a Git node of the graph expansion.
type externalGitRoot struct {
	paths gitPaths
	// operands are the canonical paths that the search covers.
	operands []string
	// state and children are the node capture: its git status, and the
	// nested repositories that git status and git ls-files listed. They are
	// set when captured is true. A failed node has no plan.
	state    repoState
	children []string
	emitted  map[string]struct{}
	captured bool
	failed   bool
}

// externalRoot is a folder seed: a directory or a single file.
type externalRoot struct {
	path string
	info os.FileInfo
}

// resolvedOperand is an operand after its canonical path and owning Git
// worktree (if any) have been determined, but before the ignore-aware
// routing decision is made.
type resolvedOperand struct {
	canonical string
	info      os.FileInfo
	repo      *gitPaths // owning worktree, or nil when outside any repo
}

type pathOperandOperation string

const (
	pathOperandResolve pathOperandOperation = "resolve"
	pathOperandRead    pathOperandOperation = "read"
)

// pathOperandError preserves the exact operand and operation while wrapping
// the file-system cause for errors.Is and verbose logs.
type pathOperandError struct {
	operation pathOperandOperation
	operand   string
	cause     error
}

func (e *pathOperandError) Error() string {
	return fmt.Sprintf("%s path %q: %v", e.operation, e.operand, e.cause)
}

func (e *pathOperandError) Unwrap() error {
	return e.cause
}

func newPathOperandError(operation pathOperandOperation, operand string, cause error) error {
	return &pathOperandError{operation: operation, operand: operand, cause: cause}
}

// statOperand resolves one path operand and reads its type. A file operand is
// opened once, so a file that exists but cannot be read is a node failure like
// an unreadable folder. abs is set when the path resolves.
func statOperand(operand string) (string, os.FileInfo, error) {
	abs, err := filepath.Abs(operand)
	if err != nil {
		return "", nil, newPathOperandError(pathOperandResolve, operand, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return abs, nil, newPathOperandError(pathOperandRead, operand, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return abs, nil, fmt.Errorf("unsupported path operand: %s", operand)
	}
	// The open proves read access. A cloud-only file is not opened: the open
	// would download it, and the build never reads its bytes.
	var st unix.Stat_t
	if info.Mode().IsRegular() && (unix.Stat(abs, &st) != nil || !fileIsDataless(abs, &st)) {
		file, err := os.Open(abs)
		if err != nil {
			return abs, nil, newPathOperandError(pathOperandRead, operand, err)
		}
		_ = file.Close()
	}
	return abs, info, nil
}

// resolveOperands resolves each explicit operand to its canonical path, its
// owning Git worktree, and its Git visibility, then groups the operands into
// the seeds of the graph expansion: Git roots and folder roots.
func resolveOperands(ctx context.Context, operands []string) (plannedExternalOperands, error) {
	var result plannedExternalOperands

	// Resolve each operand to its canonical path and owning Git worktree. A file
	// operand is resolved through its parent directory. Group in-repo operands
	// by repository so visibility needs one Git check per root.
	resolved := make([]resolvedOperand, 0, len(operands))
	pathsByRepo := make(map[string][]string)
	listings := make(dirListings)
	for _, operand := range operands {
		abs, info, err := statOperand(operand)
		if errors.Is(err, fs.ErrPermission) {
			// The operand exists but cannot be read: a node failure, not an
			// operand error.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			root := operand
			if abs != "" {
				root = canonicalCorpusPath(abs)
			}
			warnSkippedNode(ctx, root, err)
			continue
		}
		if err != nil {
			return result, err
		}
		// The on-disk case gives one root form for headers and nodes.
		canonical := onDiskPath(canonicalCorpusPath(abs))
		probe := canonical
		if !info.IsDir() {
			probe = filepath.Dir(canonical)
		}
		if gitDirectoryOperand(probe) {
			return result, newPathOperandError(pathOperandResolve, operand, errGitDirectoryOperand)
		}
		ro := resolvedOperand{canonical: canonical, info: info}
		paths, ok, err := operandRepo(ctx, probe)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			warnSkippedNode(ctx, canonical, err)
			continue
		}
		if ok {
			repo := paths
			ro.repo = &repo
			repoRoot := canonicalCorpusPath(paths.RepoDir)
			// Correct the operand to its real on-disk byte name before it
			// drives the byte-exact Git scope. On case- or normalization-
			// insensitive filesystems os.Stat accepts a mistyped name, but
			// the Git scope query (buildCurrentGitScope) is exact, so the
			// wrong-case name would silently match nothing.
			ro.canonical = realCaseWithin(repoRoot, canonical, listings)
			pathsByRepo[repoRoot] = append(pathsByRepo[repoRoot], ro.canonical)
		}
		resolved = append(resolved, ro)
	}

	// Classify each in-repo operand. Visible paths use the scoped Git corpus;
	// ignored paths use a folder or file corpus so their content stays visible.
	vis := make(map[string]visibility)
	failedRepos := make(map[string]bool)
	for repoRoot, paths := range pathsByRepo {
		decided, err := classifyVisibility(ctx, repoRoot, paths)
		if err != nil {
			// Git cannot decide the operands of this repository: the node
			// fails with one warning, and its operands are never searched as
			// folders.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			warnSkippedNode(ctx, repoRoot, err)
			failedRepos[repoRoot] = true
			continue
		}
		for path, v := range decided {
			vis[path] = v
		}
	}

	// Group the seeds. Files and directories use the same visibility rule.
	gitIndex := make(map[string]int)
	for _, ro := range resolved {
		if ro.repo != nil && failedRepos[canonicalCorpusPath(ro.repo.RepoDir)] {
			continue
		}
		// Comma-ok rather than `vis[...] == visGit`: visGit is the zero
		// value, so a missing key (should be impossible — classifyVisibility
		// returns every input) must NOT silently route to the Git corpus.
		if v, ok := vis[ro.canonical]; ro.repo != nil && ok && v == visGit {
			repoRoot := canonicalCorpusPath(ro.repo.RepoDir)
			i, found := gitIndex[repoRoot]
			if !found {
				i = len(result.gitRoots)
				gitIndex[repoRoot] = i
				result.gitRoots = append(result.gitRoots, externalGitRoot{paths: *ro.repo})
			}
			result.gitRoots[i].operands = append(result.gitRoots[i].operands, ro.canonical)
			continue
		}
		// ro.canonical holds the on-disk letter case, so a folder seed compares
		// with the other seeds by path (nested seeds).
		result.roots = append(result.roots, externalRoot{path: ro.canonical, info: ro.info})
	}
	return result, nil
}

// operandRepo returns the Git node that holds probe. It walks up and decides
// each `.git` entry by the expansion's rule (classifyGitDir): a confirmed
// boundary needs no subprocess; a stray entry names no repository, so the walk
// goes on to the parent; an entry that points to another work tree's Git
// directory, or that Git refuses, is a node failure and never a folder. Git
// runs in the boundary directory, so every operand of one repository reads its
// driver list once.
func operandRepo(ctx context.Context, probe string) (gitPaths, bool, error) {
	for dir := probe; ; {
		boundary, status := detectGitBoundary(dir)
		switch status {
		case boundaryConfirmed:
			return boundary.toGitPaths(), true, nil
		case ambiguous:
			if paths, ok, err := classifyGitDir(ctx, dir); ok || err != nil {
				return paths, ok, err
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return gitPaths{}, false, nil
		}
		dir = parent
	}
}

// findGitBoundaryUp walks from dir to the file-system root and returns the
// first confirmed or ambiguous Git boundary, or notBoundary when none exists.
func findGitBoundaryUp(dir string) (gitBoundary, detectStatus) {
	for {
		boundary, status := detectGitBoundary(dir)
		if status != notBoundary {
			return boundary, status
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return gitBoundary{}, notBoundary
		}
		dir = parent
	}
}

// gitDirectoryOperand reports a path at or below a Git directory: a .git
// directory, a bare repository (HEAD that Git accepts, objects, and refs), or
// anything inside one. The walk up stops at the first directory that holds a
// `.git` entry: below it lies a work tree, even inside a bare repository. The
// rule is the folder walk's file-system rule, with no Git process: a Git
// directory that Git refuses or cannot read stays a Git directory.
func gitDirectoryOperand(dir string) bool {
	for current := dir; ; {
		if isGitEntry(filepath.Dir(current), filepath.Base(current)) || detectBareRepoAt(current) {
			return true
		}
		if _, err := os.Lstat(filepath.Join(current, gitEntryName)); err == nil {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}
