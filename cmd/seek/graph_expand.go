package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
)

// The graph expansion turns the resolved operands into the final nodes of a
// search before any index build or route decision. A node is a folder root or
// a Git work tree. The expansion runs once per search, in parallel, and finds
// each node once, keyed by the file identity of its root.
// Each edge kind follows one rule:
//
//   - Folder -> Folder: the walk descends into each subdirectory. A folder
//     belongs to the node whose walk reaches it.
//   - Folder -> Git: a subdirectory whose `.git` directory holds HEAD,
//     objects, and refs, or whose `.git` file points to a Git directory that
//     belongs to it (detectGitBoundary). Git decides any other `.git` entry.
//   - Git -> Git: an untracked nested repository (Git lists it as one
//     "? dir/" entry) and a checked-out gitlink (index mode 160000).
//   - A folder inside a Git node belongs to that node.
//   - Git -> Git-ignored folder: no edge. Name the folder as an operand to
//     search it.
//   - Symlink: no edge. Only an operand is resolved through a symlink.
//   - Bare repository: no edge, no content, and no Git process.
//   - Mount point: crossed.
//
// A `.git` entry that Git cannot use (unreadable, dubious owner, corrupt, or a
// broken `gitdir:` line) makes its directory a failed node: one warning, and
// no other node indexes its files. A stray `.git` entry that names no
// repository (strayGitEntry) is not a boundary. No walk indexes a `.git`
// entry.

// graphExpansion holds the nodes that the expansion found. Every task runs on
// one errgroup; slots bounds the tasks that run Git or walk at the same time.
type graphExpansion struct {
	ctx     context.Context
	group   *errgroup.Group
	slots   chan struct{}
	mu      sync.Mutex
	git     map[string]*externalGitRoot // by root identity
	folders map[corpusID]*node
}

// expandGraph returns the nodes of a search: the seed nodes and every node
// below them, sorted by root. A failed node, a seed too, is one warning.
func expandGraph(ctx context.Context, seeds plannedExternalOperands) ([]node, error) {
	group, gctx := errgroup.WithContext(ctx)
	x := &graphExpansion{
		ctx:     gctx,
		group:   group,
		slots:   make(chan struct{}, indexParallelism()),
		git:     make(map[string]*externalGitRoot),
		folders: make(map[corpusID]*node),
	}
	for _, root := range seeds.gitRoots {
		x.addGitNode(root.paths, root.operands)
	}
	for _, root := range seeds.roots {
		x.addFolderNode(root)
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Wait canceled the group context. The tasks are done, so the warnings of
	// the plan step use the search context.
	x.ctx = ctx
	return x.plans()
}

// run starts one expansion task. The task waits for a slot inside its own
// goroutine, so a running task can start more tasks without blocking.
func (x *graphExpansion) run(task func() error) {
	x.group.Go(func() error {
		select {
		case x.slots <- struct{}{}:
			defer func() { <-x.slots }()
		case <-x.ctx.Done():
			return x.ctx.Err()
		}
		return task()
	})
}

// nodeError skips a failed node, a seed node too, with one warning: the
// search goes on without it. A cancellation and an environment
// failure stop the expansion.
func (x *graphExpansion) nodeError(root string, err error) error {
	if x.ctx.Err() != nil {
		return x.ctx.Err()
	}
	if interruptedChild(err) {
		return context.Canceled
	}
	if environmentFailure(err) {
		return err
	}
	warnSkippedNode(x.ctx, root, err)
	return nil
}

// addGitNode adds a Git node, or widens the scope of a node already found.
// operands are the paths of the node that the search covers; the root path
// covers the whole node. A node reached through another spelling of its root
// (letter case, a symlink) keeps its first root: the operands move onto it.
func (x *graphExpansion) addGitNode(paths gitPaths, operands []string) {
	root := canonicalCorpusPath(paths.RepoDir)
	key, err := filesystemIdentity(root)
	if err != nil {
		x.group.Go(func() error {
			return x.nodeError(root, fmt.Errorf("stat Git root: %w", err))
		})
		return
	}
	x.mu.Lock()
	gitRoot, found := x.git[key]
	if !found {
		gitRoot = &externalGitRoot{paths: paths}
		x.git[key] = gitRoot
	}
	nodeRoot := canonicalCorpusPath(gitRoot.paths.RepoDir)
	widened := false
	for _, operand := range operands {
		if rel, ok := relWithin(root, operand); ok {
			operand = filepath.Join(nodeRoot, rel)
		}
		if !slices.ContainsFunc(gitRoot.operands, func(covering string) bool { return pathWithin(covering, operand) }) {
			gitRoot.operands = append(gitRoot.operands, operand)
			widened = true
		}
	}
	// Only a wider scope can hold a child that is not added yet.
	var children []string
	if widened {
		children = gitRoot.childrenInScope()
	}
	x.mu.Unlock()
	if !found {
		x.run(func() error { return x.expandGitNode(gitRoot) })
	}
	for _, child := range children {
		x.addGitChild(nodeRoot, child)
	}
}

// expandGitNode captures a Git node and adds the nested repositories inside
// its scope.
func (x *graphExpansion) expandGitNode(node *externalGitRoot) error {
	state, children, err := captureGitNode(x.ctx, node.paths)
	x.mu.Lock()
	if err == nil {
		node.state = state
		node.children = children
		node.captured = true
	}
	inScope := node.childrenInScope()
	x.mu.Unlock()
	if err != nil {
		return x.nodeError(node.paths.RepoDir, gitNodeError(x.ctx, node.paths, err))
	}
	for _, child := range inScope {
		x.addGitChild(canonicalCorpusPath(node.paths.RepoDir), child)
	}
	return nil
}

// gitNodeError gives a capture failure the same context as an index failure:
// the work tree and its index directory. A canceled search keeps its context
// error.
func gitNodeError(ctx context.Context, paths gitPaths, err error) error {
	if ctx.Err() != nil {
		return err
	}
	indexDir := ""
	if plan, planErr := buildGitCorpusPlan(paths.RepoDir, paths.CommonDir); planErr == nil {
		indexDir = plan.indexDir
	}
	return gitCorpusError(paths.RepoDir, indexDir, err)
}

// addGitChild adds the repository that Git listed at dir below parentRoot.
// A child is a real directory strictly below its parent: a symlink is no edge,
// and a gitlink replaced by a file is content of the parent.
func (x *graphExpansion) addGitChild(parentRoot, dir string) {
	x.run(func() error {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return nil
		}
		paths, ok, err := classifyGitDir(x.ctx, dir)
		if err != nil {
			return x.nodeError(dir, err)
		}
		childRoot := canonicalCorpusPath(paths.RepoDir)
		if !ok || childRoot == parentRoot || !pathWithin(parentRoot, childRoot) {
			return nil
		}
		x.addGitNode(paths, []string{childRoot})
		return nil
	})
}

// addFolderNode adds a folder seed. The walk of a directory seed finds the Git
// nodes below it.
func (x *graphExpansion) addFolderNode(root externalRoot) {
	plan, err := planFolderCorpus(root.path, root.info)
	if err != nil {
		x.group.Go(func() error { return x.nodeError(root.path, err) })
		return
	}
	x.mu.Lock()
	_, found := x.folders[plan.id]
	if !found {
		x.folders[plan.id] = &plan
	}
	x.mu.Unlock()
	if found {
		return
	}
	// A file root is captured too: its read binds the vectors through the
	// capture's state.
	x.run(func() error {
		capture, gitRoots, err := captureFolderNode(x.ctx, plan)
		if err != nil {
			// A folder that cannot be listed fails its node; the plan goes,
			// and the other nodes answer.
			x.mu.Lock()
			delete(x.folders, plan.id)
			x.mu.Unlock()
			return x.nodeError(plan.root, folderCorpusError(plan, err))
		}
		x.mu.Lock()
		x.folders[plan.id].folder = capture
		x.mu.Unlock()
		for _, paths := range gitRoots {
			x.addGitNode(paths, []string{canonicalCorpusPath(paths.RepoDir)})
		}
		return nil
	})
}

// plans builds one node per root. A folder seed that another folder
// seed's walk reaches is part of that node, so it is dropped.
func (x *graphExpansion) plans() ([]node, error) {
	plans := make([]node, 0, len(x.git)+len(x.folders))
	for _, root := range x.git {
		if !root.captured {
			// The node failed: its capture gave one warning.
			continue
		}
		plan, err := planGitCorpus(root.paths, root.operands)
		if err != nil {
			if err := x.nodeError(root.paths.RepoDir, err); err != nil {
				return nil, err
			}
			continue
		}
		plan.gitState = &capturedGitState{state: root.state}
		plans = append(plans, plan)
	}
	for _, plan := range x.folders {
		if folderNodeCovered(*plan, x.folders) {
			continue
		}
		warnUnreadableFolders(*plan)
		plans = append(plans, *plan)
	}
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].root != plans[j].root {
			return plans[i].root < plans[j].root
		}
		return plans[i].kind < plans[j].kind
	})
	return plans, nil
}

// folderNodeCovered reports whether the walk of another folder node reaches
// the root of plan: the root is below that node and below none of the
// directories that its walk did not enter.
func folderNodeCovered(plan node, folders map[corpusID]*node) bool {
	for _, other := range folders {
		if other.id == plan.id || other.folder == nil || !pathWithin(other.root, plan.root) {
			continue
		}
		reached := true
		for dir := range other.folder.skipDirs {
			if pathWithin(dir, plan.root) {
				reached = false
				break
			}
		}
		if reached {
			return true
		}
	}
	return false
}

// childrenInScope returns the captured children inside the operands of node
// that were not returned before, so each child is added once. The caller holds
// the expansion lock.
func (node *externalGitRoot) childrenInScope() []string {
	if !node.captured {
		return nil
	}
	var out []string
	for _, child := range node.children {
		if _, done := node.emitted[child]; done {
			continue
		}
		if slices.ContainsFunc(node.operands, func(operand string) bool { return pathWithin(operand, child) }) {
			if node.emitted == nil {
				node.emitted = make(map[string]struct{})
			}
			node.emitted[child] = struct{}{}
			out = append(out, child)
		}
	}
	return out
}

// captureGitNode runs one git status and one git ls-files for a Git node. It
// returns the status and the directories of the nested repositories: the
// "? dir/" entries and changed gitlinks of git status, and the gitlinks of git
// ls-files.
func captureGitNode(ctx context.Context, paths gitPaths) (repoState, []string, error) {
	state, err := gitRepoStateIn(ctx, paths.RepoDir)
	if err != nil {
		return repoState{}, nil, err
	}
	cmd := newGitCommand(ctx, paths.RepoDir, "ls-files", "-z", "--stage")
	out, err := gitOutput(ctx, cmd, "git ls-files")
	if err != nil {
		return repoState{}, nil, err
	}
	root := canonicalCorpusPath(paths.RepoDir)
	seen := make(map[string]struct{})
	var children []string
	add := func(rel string) {
		if _, ok := seen[rel]; ok || rel == "" {
			return
		}
		seen[rel] = struct{}{}
		children = append(children, filepath.Join(root, filepath.FromSlash(rel)))
	}
	for _, rel := range state.Children {
		add(rel)
	}
	for record := range strings.SplitSeq(string(out), "\x00") {
		if !strings.HasPrefix(record, gitlinkMode+" ") {
			continue
		}
		if _, rel, ok := strings.Cut(record, "\t"); ok {
			add(rel)
		}
	}
	return state, children, nil
}

// classifyGitDir decides whether dir, which holds a `.git` entry, is the root
// of a Git node. It returns false and no error for a stray `.git` entry: dir
// is plain content. An error means Git cannot use the `.git` entry.
func classifyGitDir(ctx context.Context, dir string) (gitPaths, bool, error) {
	paths, status := detectGitBoundary(dir)
	switch status {
	case boundaryConfirmed:
		return paths, true, nil
	case notBoundary:
		return gitPaths{}, false, nil
	}
	if strayGitEntry(dir) {
		return gitPaths{}, false, nil
	}
	if _, err := os.Lstat(filepath.Join(dir, gitEntryName)); errors.Is(err, fs.ErrPermission) {
		// The directory cannot be read: Git would fail on it too, and its
		// message would blame Git.
		return gitPaths{}, false, newPathOperandError(pathOperandRead, dir, err)
	}
	if owner, ok := foreignGitDir(dir); ok {
		return gitPaths{}, false, fmt.Errorf("%s points to the Git directory of %s", filepath.Join(dir, gitEntryName), owner)
	}
	paths, err := resolveGitPaths(ctx, dir)
	if err != nil {
		return gitPaths{}, false, err
	}
	if !sameFile(paths.RepoDir, dir) {
		return gitPaths{}, false, fmt.Errorf("git does not use %s", filepath.Join(dir, ".git"))
	}
	// Git prints the on-disk letter case; the node keeps the walked spelling,
	// so it stays below its parent by path.
	paths.RepoDir = dir
	return paths, true, nil
}

// strayGitEntry reports a `.git` entry that names no repository: a file
// without a `gitdir:` line, or a readable directory that Git's
// is_git_directory rejects (no HEAD; or, with no commondir file, no objects or
// refs directory). Git ignores such a directory and goes on to the parent.
// Git's own message cannot tell these apart from an unreadable repository, so
// only a missing entry counts, never an unreadable one.
func strayGitEntry(dir string) bool {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return false
	}
	switch {
	case info.Mode().IsRegular():
		line, err := readFirstLineNoFollow(gitPath)
		return err == nil && !strings.HasPrefix(line, "gitdir:")
	case info.IsDir():
		missing := func(name string) bool {
			_, err := os.Lstat(filepath.Join(gitPath, name))
			return errors.Is(err, fs.ErrNotExist)
		}
		if missing("HEAD") {
			return true
		}
		return missing("commondir") && (missing("objects") || missing("refs"))
	default:
		return false
	}
}

// foreignGitDir reports a `.git` entry whose Git directory belongs to another
// work tree: the `.git` directory of another work tree, the admin directory of
// a linked worktree whose back-reference names another `.git` file, or a `.git`
// directory whose commondir names another repository. Git would accept it and
// make dir a second work tree of that repository, which shows that
// repository's files here and rewrites its index. Symlinks in the target are
// resolved first. A separate Git directory names no owner, so Git decides.
func foreignGitDir(dir string) (string, bool) {
	gitPath := filepath.Join(dir, gitEntryName)
	info, err := os.Lstat(gitPath)
	if err != nil {
		return "", false
	}
	target := gitPath
	switch {
	case info.Mode()&os.ModeSymlink != 0:
	case info.Mode().IsRegular():
		if target, err = parseGitFilePointer(gitPath, dir); err != nil {
			return "", false
		}
	case info.IsDir():
		// A `.git` directory takes its objects and refs from its commondir:
		// a hand-made one would read another repository.
		if common := readCommonDir(gitPath); common != gitPath {
			return common, true
		}
		return "", false
	default:
		return "", false
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", false
	}
	if strings.EqualFold(filepath.Base(real), gitEntryName) {
		owner := filepath.Dir(real)
		return owner, ownsWorkTree(owner, dir)
	}
	if readCommonDir(real) != real {
		if owner, ok := worktreeAdminOwner(real); ok && !sameFile(owner, gitPath) {
			return filepath.Dir(owner), true
		}
	}
	return "", false
}

// ownsWorkTree reports an owner directory that is another work tree: outside
// dir, and holding more than its `.git` entry. A store of Git directories, or a
// Git directory kept inside dir, is no other work tree.
func ownsWorkTree(owner, dir string) bool {
	if sameFile(owner, dir) || pathWithin(canonicalCorpusPath(dir), canonicalCorpusPath(owner)) {
		return false
	}
	entries, err := os.ReadDir(owner)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !isGitEntry(owner, entry.Name()) {
			return true
		}
	}
	return false
}

// worktreeAdminOwner returns the `.git` file that the back-reference of a
// linked worktree's admin directory names, when that file exists.
func worktreeAdminOwner(admin string) (string, bool) {
	owner, ok := readGitPathFile(admin, "gitdir")
	if !ok {
		return "", false
	}
	if _, err := os.Stat(owner); err != nil {
		return "", false
	}
	return owner, true
}

// sameFile reports whether a and b, after symlinks, name the same file.
func sameFile(a, b string) bool {
	aInfo, err := os.Stat(a)
	if err != nil {
		return false
	}
	bInfo, err := os.Stat(b)
	return err == nil && os.SameFile(aInfo, bInfo)
}

// folderBoundaries records what the walk of a folder node found below its
// root. The walk only records; the expansion turns the Git roots into nodes.
type folderBoundaries struct {
	mu       sync.Mutex
	gitRoots []gitPaths
	skipDirs map[string]struct{}
}

// claim reports whether dir, a directory below the walk root, is not content
// of the walked node: a Git node, a failed node, or a bare repository. It
// records each such directory. entries is the listing of dir.
func (b *folderBoundaries) claim(ctx context.Context, dir string, entries []os.DirEntry) bool {
	gitEntry, bare := gitListingMarks(dir, entries)
	// A HEAD file that names no ref and no commit makes a plain folder, as for
	// Git.
	bare = bare && validGitHead(filepath.Join(dir, "HEAD"))
	if !gitEntry && !bare {
		return false
	}
	var paths gitPaths
	isNode := false
	if gitEntry {
		var err error
		paths, isNode, err = classifyGitDir(ctx, dir)
		if err != nil {
			warnSkippedNode(ctx, dir, err)
		} else if !isNode {
			return false
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if isNode {
		b.gitRoots = append(b.gitRoots, paths)
	}
	b.skipDirs[dir] = struct{}{}
	return true
}

// gitListingMarks reads the Git marks of the listing of dir: a `.git` entry,
// or the HEAD file and objects and refs directories of a bare repository.
func gitListingMarks(dir string, entries []os.DirEntry) (gitEntry, bare bool) {
	var head, objects, refs bool
	for _, entry := range entries {
		switch name := entry.Name(); {
		case isGitEntry(dir, name):
			gitEntry = true
		case name == "HEAD":
			head = entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0
		case name == "objects":
			objects = entry.IsDir()
		case name == "refs":
			refs = entry.IsDir()
		}
	}
	return gitEntry, !gitEntry && head && objects && refs
}

// validGitHead reports a HEAD file that Git accepts (validate_headref): a
// symbolic ref below refs/, or a line that starts with an object ID in hex of
// either case. Text after the ID does not matter.
func validGitHead(path string) bool {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// An old-style symbolic HEAD.
		target, err := os.Readlink(path)
		return err == nil && strings.HasPrefix(target, "refs/")
	}
	f, err := openNoFollow(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var buf [256]byte
	n, _ := io.ReadFull(f, buf[:])
	line := string(buf[:n])
	if ref, ok := strings.CutPrefix(line, "ref:"); ok {
		// Git skips any white space after "ref:", newlines too.
		return strings.HasPrefix(strings.TrimLeft(ref, " \t\r\n\v\f"), "refs/")
	}
	const sha1HexDigits = 40
	if len(line) < sha1HexDigits {
		return false
	}
	return strings.Trim(line[:sha1HexDigits], "0123456789abcdefABCDEF") == ""
}

// isGitEntry reports the Git entry of dir: ".git", or another spelling that
// the file system maps to it (a case-insensitive file system).
func isGitEntry(dir, name string) bool {
	if name == gitEntryName {
		return true
	}
	if !strings.EqualFold(name, gitEntryName) {
		return false
	}
	entry, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	canonical, err := os.Lstat(filepath.Join(dir, gitEntryName))
	return err == nil && os.SameFile(entry, canonical)
}
