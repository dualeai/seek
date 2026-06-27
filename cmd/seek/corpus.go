package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

type corpusKind string
type rootType string
type corpusID string

const (
	corpusKindGit    corpusKind = "git"
	corpusKindFolder corpusKind = "folder"

	rootTypeWorktree  rootType = "worktree"
	rootTypeDirectory rootType = "directory"
	rootTypeFile      rootType = "file"

	seekIndexGeneration = "v2"
	// v3 bumped when the "git-boundary" namespace marker landed: it
	// changes the folder state-hash formula for any parent containing
	// nested git repos. Without the bump, users upgrading would inherit
	// a stale state file and miss discovery's effect on the indexed
	// byte budget.
	seekCacheLayoutVersion    = "v3"
	seekDocumentNamingVersion = "slash-relative-v1"
	zoektCompatibilityVersion = "zoekt-a0f5789d25cb"
)

type corpusPlan struct {
	id          corpusID
	kind        corpusKind
	rootType    rootType
	root        string
	displayRoot string
	// selectedFiles are corpus-relative (slash) names the user named
	// EXPLICITLY as file operands. Results whose FileName is in this set
	// render as a basename in single-corpus mode so the output reads as the
	// file the user selected. Formatter-only — never part of the corpus ID.
	selectedFiles []string
	cacheDir      string
	indexDir      string
	scope         query.Q
	gitPaths      *gitPaths
	dirtyScope    *gitDirtyScope
	// Scoped Git directory plans use separate committed and dirty layers
	// keyed by the selected Git pathspecs so tracked or dirty siblings
	// outside the selected path cannot cap the search.
	committedCacheDir string
	committedIndexDir string
	dirtyCacheDir     string
	dirtyIndexDir     string
	// Expected layer state hashes are populated after refresh and
	// validated under shared search locks before loading scoped shards.
	committedStateHash string
	dirtyStateHash     string
	// excludeRoots are explicit child owners that a folder plan must not
	// descend into. This is separate from dynamic nested-git discovery:
	// explicit child Git roots must stay carved out even when discovery is
	// disabled.
	excludeRoots []string
	// userExplicit distinguishes plans the user asked for on the CLI
	// (failure is fatal) from plans the folder walker discovered via
	// nested-git detection (failure is logged + skipped). The pool
	// worker wrapper honours the policy at corpus_pool.go:Enqueue.
	userExplicit bool
	// discover is the dynamic-enqueue callback the corpusPool installs
	// on folder plans before invoking the worker. The walker calls
	// discover(boundary) when detectGitBoundary confirms a nested git
	// repo; the callback returns true when the boundary was accepted
	// into the pool or already covered by another plan. false means no
	// corpus owns the boundary, so the walker should descend as plain
	// folder. nil means discovery is disabled for this plan.
	discover func(gitBoundary) bool
}

type corpusSearchResult struct {
	corpusID    corpusID
	kind        corpusKind
	displayRoot string
	// displayName is the single-corpus header: a basename for an explicitly
	// selected file, otherwise the corpus-relative FileName.
	displayName string
	file        zoekt.FileMatch
}

type corpusIndexState uint8

const (
	corpusSearchable corpusIndexState = iota
	corpusKnownEmpty
)

func planCorpora(ctx context.Context, paths *gitPaths, operands []string) ([]corpusPlan, error) {
	if len(operands) == 0 {
		if paths == nil {
			return nil, fmt.Errorf("not a git repository")
		}
		return planGitOperands(ctx, *paths, canonicalCorpusPath(paths.RepoDir))
	}

	external, err := collectExternalOperands(ctx, operands)
	if err != nil {
		return nil, err
	}

	plans := make([]corpusPlan, 0, 1+len(external.gitRoots)+len(external.roots))
	plans, err = appendGitRootPlans(plans, external.gitRoots)
	if err != nil {
		return nil, err
	}

	for _, root := range external.roots {
		plan, err := planFolderCorpusWithExclusions(root.path, root.info, root.excludes)
		if err != nil {
			return nil, err
		}
		plan.userExplicit = true
		plans = append(plans, plan)
	}

	if len(plans) == 0 {
		return nil, fmt.Errorf("no searchable corpus")
	}
	return plans, nil
}

func planGitOperands(ctx context.Context, paths gitPaths, operand string) ([]corpusPlan, error) {
	externalGit := make(map[string]*externalGitRoot)
	addExternalGitOperand(externalGit, paths, operand)
	addVisibleNestedGitOperands(ctx, externalGit, paths, operand)
	gitRoots := sortedExternalGitRoots(externalGit)
	addGitChildExclusions(gitRoots, nil)
	return appendGitRootPlans(make([]corpusPlan, 0, len(gitRoots)), gitRoots)
}

// appendGitRootPlans builds one git corpus plan per root and appends it to
// plans, propagating each root's userExplicit flag.
func appendGitRootPlans(plans []corpusPlan, gitRoots []externalGitRoot) ([]corpusPlan, error) {
	for _, gitRoot := range gitRoots {
		plan, err := planCurrentGitCorpusWithExclusions(gitRoot.paths, gitRoot.operands, gitRoot.excludes)
		if err != nil {
			return nil, err
		}
		plan.userExplicit = gitRoot.userExplicit
		plans = append(plans, plan)
	}
	return plans, nil
}

type plannedExternalOperands struct {
	gitRoots []externalGitRoot
	roots    []externalRoot
}

type externalGitRoot struct {
	paths        gitPaths
	operands     []string
	excludes     []string
	userExplicit bool
}

type externalRoot struct {
	path     string
	info     os.FileInfo
	excludes []string
}

// resolvedOperand is an operand after its canonical path and owning Git
// worktree (if any) have been determined, but before the ignore-aware
// routing decision is made.
type resolvedOperand struct {
	canonical string
	info      os.FileInfo
	repo      *gitPaths // owning worktree, or nil when outside any repo
}

func collectExternalOperands(ctx context.Context, operands []string) (plannedExternalOperands, error) {
	var result plannedExternalOperands

	// Phase 1 — resolve each operand to its canonical path and owning Git
	// worktree. A file operand is resolved through its parent directory so a
	// tracked file routes to its repo exactly like the directory containing
	// it. Operands inside a repo are grouped by repo root for a single
	// ignore check below.
	resolved := make([]resolvedOperand, 0, len(operands))
	pathsByRepo := make(map[string][]string)
	for _, operand := range operands {
		abs, err := filepath.Abs(operand)
		if err != nil {
			return result, fmt.Errorf("resolve path %q: %w", operand, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return result, fmt.Errorf("read path %q: %w", operand, err)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return result, fmt.Errorf("unsupported path operand: %s", operand)
		}

		canonical := canonicalCorpusPath(abs)
		probe := canonical
		if !info.IsDir() {
			probe = filepath.Dir(canonical)
		}
		ro := resolvedOperand{canonical: canonical, info: info}
		if paths, ok := resolveGitDirectoryOperand(ctx, probe); ok {
			repo := paths
			ro.repo = &repo
			repoRoot := canonicalCorpusPath(paths.RepoDir)
			// Correct the operand to its real on-disk byte name before it
			// drives the byte-exact Git scope. On case- or normalization-
			// insensitive filesystems os.Stat accepts a mistyped name, but
			// the ls-tree pathspec / scope.contains / zoekt FileNameSet are
			// exact, so the wrong-case name would silently match nothing.
			ro.canonical = realCaseWithin(repoRoot, canonical)
			pathsByRepo[repoRoot] = append(pathsByRepo[repoRoot], ro.canonical)
		}
		resolved = append(resolved, ro)
	}

	// Phase 2 — one ignore check per repo root decides, for every in-repo
	// operand, whether the Git index covers it (route to the scoped Git
	// corpus) or a .gitignore rule excludes it (fall back to a folder/file
	// corpus so its content is still searched).
	vis := make(map[string]visibility)
	for repoRoot, paths := range pathsByRepo {
		decided, err := classifyVisibility(ctx, repoRoot, paths)
		if err != nil {
			return result, err
		}
		for path, v := range decided {
			vis[path] = v
		}
	}

	// Phase 3 — route. A file and a directory inside a worktree take the
	// same decision; only the nested-Git discovery walk is directory-only
	// (a file has no children).
	external := make(map[string]externalRoot)
	externalGit := make(map[string]*externalGitRoot)
	for _, ro := range resolved {
		// Comma-ok rather than `vis[...] == visGit`: visGit is the zero
		// value, so a missing key (should be impossible — classifyVisibility
		// returns every input) must NOT silently route to the Git corpus.
		if v, ok := vis[ro.canonical]; ro.repo != nil && ok && v == visGit {
			addExternalGitOperand(externalGit, *ro.repo, ro.canonical)
			if ro.info.IsDir() {
				addVisibleNestedGitOperands(ctx, externalGit, *ro.repo, ro.canonical)
			}
			continue
		}
		external[ro.canonical] = externalRoot{path: ro.canonical, info: ro.info}
	}

	result.roots = collapseExternalRoots(external)
	broadenGitOperandsCoveredByFolders(externalGit, result.roots)
	addVisibleNestedGitOperandsForGroups(ctx, externalGit)
	result.gitRoots = sortedExternalGitRoots(externalGit)
	addGitChildExclusions(result.gitRoots, result.roots)
	addFolderChildExclusions(result.roots, result.gitRoots)
	return result, nil
}

func addVisibleNestedGitOperandsForGroups(ctx context.Context, groups map[string]*externalGitRoot) {
	for {
		roots := sortedExternalGitRoots(groups)
		before := len(groups)
		for _, root := range roots {
			repoRoot := canonicalCorpusPath(root.paths.RepoDir)
			for _, operand := range root.operands {
				if operand == repoRoot {
					addVisibleNestedGitOperands(ctx, groups, root.paths, repoRoot)
					break
				}
			}
		}
		if len(groups) == before {
			return
		}
	}
}

func resolveGitDirectoryOperand(ctx context.Context, canonical string) (gitPaths, bool) {
	if b, status := detectGitBoundary(canonical, ""); status == boundaryConfirmed {
		paths := b.toGitPaths()
		if pathWithin(canonicalCorpusPath(paths.RepoDir), canonical) {
			return paths, true
		}
		return gitPaths{}, false
	}
	paths, err := resolveGitPaths(ctx, canonical)
	if err != nil {
		return gitPaths{}, false
	}
	if !pathWithin(canonicalCorpusPath(paths.RepoDir), canonical) {
		return gitPaths{}, false
	}
	return paths, true
}

func addExternalGitOperand(groups map[string]*externalGitRoot, paths gitPaths, operand string) {
	addExternalGitOperandWithSource(groups, paths, operand, true)
}

func addDiscoveredExternalGitOperand(groups map[string]*externalGitRoot, paths gitPaths, operand string) {
	addExternalGitOperandWithSource(groups, paths, operand, false)
}

func addExternalGitOperandWithSource(groups map[string]*externalGitRoot, paths gitPaths, operand string, userExplicit bool) {
	gitRoot := canonicalCorpusPath(paths.RepoDir)
	group := groups[gitRoot]
	if group == nil {
		group = &externalGitRoot{paths: paths}
		groups[gitRoot] = group
	}
	if userExplicit {
		group.userExplicit = true
	}
	group.operands = append(group.operands, operand)
}

func sortedExternalGitRoots(groups map[string]*externalGitRoot) []externalGitRoot {
	if len(groups) == 0 {
		return nil
	}
	roots := make([]string, 0, len(groups))
	for root := range groups {
		roots = append(roots, root)
	}
	sort.Strings(roots)

	result := make([]externalGitRoot, 0, len(roots))
	for _, root := range roots {
		group := groups[root]
		sort.Strings(group.operands)
		result = append(result, *group)
	}
	return result
}

func broadenGitOperandsCoveredByFolders(groups map[string]*externalGitRoot, roots []externalRoot) {
	for _, root := range roots {
		if !root.info.IsDir() {
			continue
		}
		for gitRoot, group := range groups {
			if pathWithin(root.path, gitRoot) {
				// The folder plan will exclude this child Git root, so the
				// child Git plan must own the whole repo rather than only a
				// narrower operand below it.
				group.operands = append(group.operands, gitRoot)
			}
		}
	}
}

func addVisibleNestedGitOperands(ctx context.Context, groups map[string]*externalGitRoot, paths gitPaths, operand string) {
	type scan struct {
		paths   gitPaths
		operand string
	}
	queue := []scan{{paths: paths, operand: operand}}
	scanned := make(map[string]struct{})
	queuedRoots := map[string]struct{}{
		canonicalCorpusPath(paths.RepoDir): {},
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		key := canonicalCorpusPath(current.paths.RepoDir) + "\x00" + canonicalCorpusPath(current.operand)
		if _, ok := scanned[key]; ok {
			continue
		}
		scanned[key] = struct{}{}
		for _, child := range addVisibleNestedGitOperandsOnce(ctx, groups, current.paths, current.operand) {
			childRoot := canonicalCorpusPath(child.RepoDir)
			if _, ok := queuedRoots[childRoot]; ok {
				continue
			}
			queuedRoots[childRoot] = struct{}{}
			queue = append(queue, scan{paths: child, operand: childRoot})
		}
	}
}

func addVisibleNestedGitOperandsOnce(ctx context.Context, groups map[string]*externalGitRoot, paths gitPaths, operand string) []gitPaths {
	repoRoot := canonicalCorpusPath(paths.RepoDir)
	rel, ok := relWithin(repoRoot, operand)
	if !ok {
		return nil
	}

	cmd := gitCmd(ctx,
		"status",
		"--porcelain=v2",
		"--no-renames",
		"--no-ahead-behind",
		"--untracked-files=all",
		"-z",
		"--",
		gitLiteralPathspec(rel),
	)
	cmd.Dir = paths.RepoDir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	seen := make(map[string]struct{})
	var discovered []gitPaths
	for _, statusPath := range parseGitStatusV2(string(out)).Files {
		candidate := canonicalCorpusPath(filepath.Join(repoRoot, filepath.FromSlash(statusPath)))
		if child, ok := addVisibleNestedGitOperand(groups, repoRoot, paths.CommonDir, operand, candidate, seen); ok {
			discovered = append(discovered, child)
		}
	}
	discovered = append(discovered, addGitlinkNestedGitOperands(ctx, groups, paths, operand, rel, seen)...)
	return discovered
}

func addGitlinkNestedGitOperands(
	ctx context.Context,
	groups map[string]*externalGitRoot,
	paths gitPaths,
	operand, rel string,
	seen map[string]struct{},
) []gitPaths {
	cmd := gitCmd(ctx, "ls-files", "-z", "--stage", "--", gitLiteralPathspec(rel))
	cmd.Dir = paths.RepoDir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	repoRoot := canonicalCorpusPath(paths.RepoDir)
	var discovered []gitPaths
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" || !strings.HasPrefix(record, "160000 ") {
			continue
		}
		tab := strings.IndexByte(record, '\t')
		if tab < 0 || tab == len(record)-1 {
			continue
		}
		candidate := canonicalCorpusPath(filepath.Join(repoRoot, filepath.FromSlash(record[tab+1:])))
		if child, ok := addVisibleNestedGitOperand(groups, repoRoot, paths.CommonDir, operand, candidate, seen); ok {
			discovered = append(discovered, child)
		}
	}
	return discovered
}

func addVisibleNestedGitOperand(
	groups map[string]*externalGitRoot,
	repoRoot, commonDir, operand, candidate string,
	seen map[string]struct{},
) (gitPaths, bool) {
	dir := candidate
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	for pathWithin(operand, dir) {
		if dir == repoRoot {
			return gitPaths{}, false
		}
		b, status := detectGitBoundary(dir, commonDir)
		if status == boundaryConfirmed {
			paths := b.toGitPaths()
			childRoot := canonicalCorpusPath(paths.RepoDir)
			if childRoot == repoRoot || !pathWithin(operand, childRoot) {
				return gitPaths{}, false
			}
			if _, ok := seen[childRoot]; ok {
				return gitPaths{}, false
			}
			seen[childRoot] = struct{}{}
			addDiscoveredExternalGitOperand(groups, paths, childRoot)
			return paths, true
		}
		next := filepath.Dir(dir)
		if next == dir {
			return gitPaths{}, false
		}
		dir = next
	}
	return gitPaths{}, false
}

func addGitChildExclusions(gitRoots []externalGitRoot, roots []externalRoot) {
	for i := range gitRoots {
		parent := canonicalCorpusPath(gitRoots[i].paths.RepoDir)
		excludes := make(map[string]struct{})
		for _, root := range roots {
			if root.path != parent && pathWithin(parent, root.path) {
				excludes[root.path] = struct{}{}
			}
		}
		for j := range gitRoots {
			if i == j {
				continue
			}
			child := canonicalCorpusPath(gitRoots[j].paths.RepoDir)
			if child != parent && pathWithin(parent, child) {
				excludes[child] = struct{}{}
			}
		}
		gitRoots[i].excludes = sortedKeys(excludes)
	}
}

func addFolderChildExclusions(roots []externalRoot, gitRoots []externalGitRoot) {
	for i := range roots {
		if !roots[i].info.IsDir() {
			continue
		}
		parent := roots[i].path
		excludes := make(map[string]struct{})
		for _, gitRoot := range gitRoots {
			child := canonicalCorpusPath(gitRoot.paths.RepoDir)
			if child != parent && pathWithin(parent, child) {
				excludes[child] = struct{}{}
			}
		}
		roots[i].excludes = sortedKeys(excludes)
	}
}

func collapseExternalRoots(roots map[string]externalRoot) []externalRoot {
	if len(roots) == 0 {
		return nil
	}

	paths := make([]string, 0, len(roots))
	for path := range roots {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	selected := make([]externalRoot, 0, len(paths))
	for _, path := range paths {
		root := roots[path]
		if coveredByExternalDir(root, selected) {
			continue
		}
		selected = append(selected, root)
	}
	return selected
}

func coveredByExternalDir(path externalRoot, roots []externalRoot) bool {
	for _, root := range roots {
		if !root.info.IsDir() {
			continue
		}
		if path.path == root.path {
			return true
		}
		if strings.HasPrefix(path.path, root.path+string(filepath.Separator)) {
			if crossesGitBoundary(root.path, path) {
				continue
			}
			return true
		}
	}
	return false
}

func crossesGitBoundary(parent string, child externalRoot) bool {
	dir := child.path
	if !child.info.IsDir() {
		dir = filepath.Dir(child.path)
	}
	for dir != parent && pathWithin(parent, dir) {
		if _, status := detectGitBoundary(dir, ""); status == boundaryConfirmed {
			return true
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		dir = next
	}
	return false
}

// buildGitCorpusPlan is the shared constructor for both the
// explicit-operand path (planCurrentGitCorpus) and the walker-discovery
// path (planDiscoveredGitCorpus). Both flows mint a corpus with the
// same identity (kind, root, dev:ino) so the same physical repo
// reached via multiple paths — e.g. CLI operand AND walker
// discovery — collapses to one cache dir.
//
// rootTypeWorktree is the convention regardless of the on-disk
// layout: the corpus models the working tree, not the .git layout.
// Using gitBoundary.Mode (which can be rootTypeDirectory for a normal
// .git/ repo) would mint a different corpusID and defeat dedup.
func buildGitCorpusPlan(repoDir, commonDir string, extraIDParts ...string) (corpusPlan, error) {
	root := canonicalCorpusPath(repoDir)
	cdir := canonicalCorpusPath(commonDir)
	idParts := []string{"git_worktree", root, "git_common_dir", cdir}
	idParts = append(idParts, extraIDParts...)
	return newCorpusPlan(corpusKindGit, rootTypeWorktree, root, "git", idParts...)
}

// planDiscoveredGitCorpus builds a plan for a repo the folder walker
// found mid-flight via detectGitBoundary. userExplicit stays at zero
// value (false) so the pool's worker wrapper logs and swallows failures
// instead of aborting the user's search. gitPaths is derived from the
// boundary without a subprocess.
func planDiscoveredGitCorpus(b gitBoundary) (corpusPlan, error) {
	return planDiscoveredGitPaths(b.toGitPaths())
}

func planDiscoveredGitPaths(paths gitPaths) (corpusPlan, error) {
	plan, err := buildGitCorpusPlan(paths.RepoDir, paths.CommonDir)
	if err != nil {
		return corpusPlan{}, err
	}
	plan.gitPaths = &paths
	return plan, nil
}

func planCurrentGitCorpus(paths gitPaths) (corpusPlan, error) {
	plan, err := buildGitCorpusPlan(paths.RepoDir, paths.CommonDir)
	if err != nil {
		return corpusPlan{}, err
	}
	plan.gitPaths = &paths
	return plan, nil
}

func planFolderCorpus(root string, info os.FileInfo) (corpusPlan, error) {
	return planFolderCorpusWithExclusions(root, info, nil)
}

func planFolderCorpusWithExclusions(root string, info os.FileInfo, excludes []string) (corpusPlan, error) {
	rt := rootTypeFile
	if info.IsDir() {
		rt = rootTypeDirectory
	}

	excludes = sortedCanonicalRoots(excludes)
	extraIDParts := []string(nil)
	if len(excludes) > 0 {
		extraIDParts = append(extraIDParts, "folder_exclude_roots")
		extraIDParts = append(extraIDParts, excludes...)
	}
	plan, err := newCorpusPlan(corpusKindFolder, rt, root, "folder", extraIDParts...)
	if err != nil {
		return corpusPlan{}, err
	}
	plan.excludeRoots = excludes
	if rt == rootTypeFile {
		// A single-file corpus indexes the file under its basename. Render
		// results relative to the parent dir so the header reads as the file
		// the user selected, and multi-corpus mode still joins to an
		// absolute, directly-openable path.
		plan.displayRoot = filepath.Dir(root)
		plan.selectedFiles = []string{filepath.Base(root)}
	}
	return plan, nil
}

func newCorpusPlan(kind corpusKind, rt rootType, root, statSubject string, extraIDParts ...string) (corpusPlan, error) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return corpusPlan{}, err
	}

	root = canonicalCorpusPath(root)
	rootID, err := filesystemIdentity(root)
	if err != nil {
		return corpusPlan{}, fmt.Errorf("stat %s root: %w", statSubject, err)
	}

	idParts := []string{
		"kind", string(kind),
		"root_type", string(rt),
		"root", root,
		"root_identity", rootID,
		"index_generation", seekIndexGeneration,
		"cache_layout", seekCacheLayoutVersion,
		"document_naming", seekDocumentNamingVersion,
		"zoekt_compatibility", zoektCompatibilityVersion,
		"index_options", indexOptionsHash(),
	}
	idParts = append(idParts, extraIDParts...)
	id := newCorpusID(idParts...)
	cacheDir := filepath.Join(cacheRoot, "corpora", string(id))
	return corpusPlan{
		id:          id,
		kind:        kind,
		rootType:    rt,
		root:        root,
		displayRoot: root,
		cacheDir:    cacheDir,
		indexDir:    filepath.Join(cacheDir, "index"),
	}, nil
}

func planCurrentGitCorpusWithOperands(paths gitPaths, operands []string) (corpusPlan, error) {
	return planCurrentGitCorpusWithExclusions(paths, operands, nil)
}

func planCurrentGitCorpusWithExclusions(paths gitPaths, operands, excludes []string) (corpusPlan, error) {
	plan, err := planCurrentGitCorpus(paths)
	if err != nil {
		return corpusPlan{}, err
	}
	includeScope, err := buildCurrentGitScope(plan.root, operands)
	if err != nil {
		return corpusPlan{}, err
	}
	excludeScope, err := buildCurrentGitScope(plan.root, excludes)
	if err != nil {
		return corpusPlan{}, err
	}
	plan.scope = combineGitScope(includeScope, excludeScope)
	dirtyScope, err := buildGitDirtyScope(plan.root, operands, excludes)
	if err != nil {
		return corpusPlan{}, err
	}
	if dirtyScope != nil {
		plan, err = buildGitCorpusPlan(paths.RepoDir, paths.CommonDir, "git_search_scope", dirtyScope.key)
		if err != nil {
			return corpusPlan{}, err
		}
		plan.gitPaths = &paths
		plan.scope = combineGitScope(includeScope, excludeScope)
		plan.dirtyScope = dirtyScope
		committed, err := buildGitCorpusPlan(paths.RepoDir, paths.CommonDir, "git_layer", "committed", "git_scope", dirtyScope.key)
		if err != nil {
			return corpusPlan{}, err
		}
		dirty, err := buildGitCorpusPlan(paths.RepoDir, paths.CommonDir, "git_layer", "dirty", "git_dirty_scope", dirtyScope.key)
		if err != nil {
			return corpusPlan{}, err
		}
		plan.committedCacheDir = committed.cacheDir
		plan.committedIndexDir = committed.indexDir
		plan.dirtyCacheDir = dirty.cacheDir
		plan.dirtyIndexDir = dirty.indexDir
	}
	spec, err := buildGitScopeSpec(plan.root, operands)
	if err != nil {
		return corpusPlan{}, err
	}
	plan.selectedFiles = spec.files
	return plan, nil
}

// pathWithin reports whether path is root or a descendant of it (bool-only
// containment; relWithin is the variant that also returns the relative path).
func pathWithin(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// relWithin returns path relative to root and whether path is inside root
// (root itself counts as inside, with rel "."). Callers that need the rel
// value share this instead of repeating the filepath.Rel + ".."-prefix test.
func relWithin(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel, false
	}
	return rel, true
}

func sortedKeys(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedCanonicalRoots(roots []string) []string {
	if len(roots) == 0 {
		return nil
	}
	out := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = canonicalCorpusPath(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	sort.Strings(out)
	return out
}

func seekUserCacheRoot() (string, error) {
	// SEEK_CACHE_DIR overrides the default user-cache location.
	// Primary use case: benchmarks and tests that need a sandboxed
	// cache so they don't wipe the developer's dev cache.
	if dir := os.Getenv("SEEK_CACHE_DIR"); dir != "" {
		return dir, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	if dir == "" {
		return "", fmt.Errorf("resolve user cache directory: empty path")
	}
	return filepath.Join(dir, "seek"), nil
}

func filesystemIdentity(path string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	return strconv.FormatUint(uint64(st.Dev), 10) + ":" +
		strconv.FormatUint(uint64(st.Ino), 10), nil
}

func canonicalCorpusPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// realCaseWithin rewrites the portion of path below root to the actual
// on-disk directory-entry names. On case- or normalization-insensitive
// filesystems (macOS, Windows) an operand may be typed with different case
// or Unicode normalization than Git stored; the Git scope (ls-tree pathspec,
// scope.contains, zoekt FileNameSet) is byte-exact, so without this a tracked
// file would be silently missed. Matching is by os.SameFile (device+inode),
// robust to both case and NFC/NFD. Components are corrected top-down so a
// wrong-case parent is fixed before its children are read. Best-effort: any
// stat/read error leaves that component as typed.
func realCaseWithin(root, path string) string {
	rel, ok := relWithin(root, path)
	if !ok || rel == "." {
		return path
	}
	cur := root
	for _, comp := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, realCaseComponent(cur, comp))
	}
	return cur
}

func realCaseComponent(parent, name string) string {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return name
	}
	// Fast path: the typed name is already the on-disk byte name. This is the
	// common case and the ONLY possibility on a case-sensitive filesystem, so
	// it must cost no per-entry Lstat/Info syscalls — just the name scan.
	for _, e := range entries {
		if e.Name() == name {
			return name
		}
	}
	// Case/normalization mismatch (insensitive FS): find the entry that IS
	// this file by identity (device+inode), robust to case and NFC/NFD.
	target, err := os.Lstat(filepath.Join(parent, name))
	if err != nil {
		return name
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if os.SameFile(target, info) {
			return e.Name()
		}
	}
	return name
}
