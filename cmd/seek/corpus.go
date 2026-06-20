package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	cacheDir    string
	indexDir    string
	scope       query.Q
	gitPaths    *gitPaths
	// excludeRoots are explicit child owners that a folder plan must not
	// descend into. This is separate from dynamic nested-git discovery:
	// explicit child Git roots must stay carved out even when discovery is
	// disabled or capped.
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
	// into the pool (false on dedup, cap, or build failure). nil means
	// discovery is disabled for this plan.
	discover func(gitBoundary) bool
}

type corpusSearchResult struct {
	corpusID    corpusID
	kind        corpusKind
	displayRoot string
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
		plan, err := planCurrentGitCorpus(*paths)
		if err != nil {
			return nil, err
		}
		plan.userExplicit = true
		return []corpusPlan{plan}, nil
	}

	external, err := collectExternalOperands(ctx, operands)
	if err != nil {
		return nil, err
	}

	plans := make([]corpusPlan, 0, 1+len(external.gitRoots)+len(external.roots))
	for _, externalGit := range external.gitRoots {
		plan, err := planCurrentGitCorpusWithExclusions(externalGit.paths, externalGit.operands, externalGit.excludes)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	for _, root := range external.roots {
		plan, err := planFolderCorpusWithExclusions(root.path, root.info, root.excludes)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	if len(plans) == 0 {
		return nil, fmt.Errorf("no searchable corpus")
	}
	// Every plan that flows out of planCorpora was explicitly requested
	// on the CLI; failures must abort the search. planDiscoveredGitCorpus
	// deliberately leaves userExplicit=false on walker-discovered plans.
	for i := range plans {
		plans[i].userExplicit = true
	}
	return plans, nil
}

type plannedExternalOperands struct {
	gitRoots []externalGitRoot
	roots    []externalRoot
}

type externalGitRoot struct {
	paths    gitPaths
	operands []string
	excludes []string
}

type externalRoot struct {
	path     string
	info     os.FileInfo
	excludes []string
}

func collectExternalOperands(ctx context.Context, operands []string) (plannedExternalOperands, error) {
	var result plannedExternalOperands

	external := make(map[string]externalRoot)
	externalGit := make(map[string]*externalGitRoot)
	for _, operand := range operands {
		abs, err := filepath.Abs(operand)
		if err != nil {
			return result, fmt.Errorf("resolve path %q: %w", operand, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return result, fmt.Errorf("read path %q: %w", operand, err)
		}

		canonical := canonicalCorpusPath(abs)
		if !info.IsDir() && !info.Mode().IsRegular() {
			return result, fmt.Errorf("unsupported path operand: %s", operand)
		}
		if info.IsDir() {
			if gitPaths, ok := resolveGitRootOperand(ctx, canonical); ok {
				addExternalGitOperand(externalGit, gitPaths, canonical)
				continue
			}
		}
		external[canonical] = externalRoot{path: canonical, info: info}
	}

	result.gitRoots = sortedExternalGitRoots(externalGit)
	result.roots = collapseExternalRoots(external)
	addGitChildExclusions(result.gitRoots, result.roots)
	addFolderChildExclusions(result.roots, result.gitRoots)
	return result, nil
}

func resolveGitRootOperand(ctx context.Context, canonical string) (gitPaths, bool) {
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
	if canonicalCorpusPath(paths.RepoDir) != canonical {
		return gitPaths{}, false
	}
	return paths, true
}

func addExternalGitOperand(groups map[string]*externalGitRoot, paths gitPaths, operand string) {
	gitRoot := canonicalCorpusPath(paths.RepoDir)
	group := groups[gitRoot]
	if group == nil {
		group = &externalGitRoot{paths: paths}
		groups[gitRoot] = group
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
func buildGitCorpusPlan(repoDir, commonDir string) (corpusPlan, error) {
	root := canonicalCorpusPath(repoDir)
	cdir := canonicalCorpusPath(commonDir)
	return newCorpusPlan(
		corpusKindGit,
		rootTypeWorktree,
		root,
		"git",
		"git_worktree", root,
		"git_common_dir", cdir,
	)
}

// planDiscoveredGitCorpus builds a plan for a repo the folder walker
// found mid-flight via detectGitBoundary. userExplicit stays at zero
// value (false) so the pool's worker wrapper logs and swallows failures
// instead of aborting the user's search. gitPaths is derived from the
// boundary without a subprocess.
func planDiscoveredGitCorpus(b gitBoundary) (corpusPlan, error) {
	plan, err := buildGitCorpusPlan(b.RepoDir, b.CommonDir)
	if err != nil {
		return corpusPlan{}, err
	}
	paths := b.toGitPaths()
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
	return plan, nil
}

func combineGitScope(includeScope, excludeScope query.Q) query.Q {
	if excludeScope == nil {
		return includeScope
	}
	notExcluded := &query.Not{Child: excludeScope}
	if includeScope == nil {
		return notExcluded
	}
	return query.Simplify(query.NewAnd(includeScope, notExcluded))
}

func buildCurrentGitScope(root string, operands []string) (query.Q, error) {
	root = canonicalCorpusPath(root)

	fileSet := make(map[string]struct{})
	dirSet := make(map[string]struct{})
	for _, operand := range operands {
		abs, err := filepath.Abs(operand)
		if err != nil {
			return nil, fmt.Errorf("resolve path %q: %w", operand, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("read path %q: %w", operand, err)
		}

		canonical := canonicalCorpusPath(abs)
		if !pathWithin(root, canonical) {
			return nil, fmt.Errorf("path outside current git worktree: %s", operand)
		}

		rel, err := filepath.Rel(root, canonical)
		if err != nil {
			return nil, fmt.Errorf("scope path %q: %w", operand, err)
		}
		if rel == "." {
			return nil, nil
		}
		name := filepath.ToSlash(rel)
		if info.IsDir() {
			dirSet[name] = struct{}{}
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("unsupported path operand: %s", operand)
		}
		fileSet[name] = struct{}{}
	}

	dirs := sortedKeys(dirSet)
	dirs = collapseDirectoryScopes(dirs)

	files := sortedKeys(fileSet)
	files = dropFilesCoveredByDirs(files, dirs)

	queries := make([]query.Q, 0, 1+len(dirs))
	if len(files) > 0 {
		queries = append(queries, query.NewFileNameSet(files...))
	}
	for _, dir := range dirs {
		dirQ, err := query.RegexpQuery("^"+regexp.QuoteMeta(dir)+"/", false, true)
		if err != nil {
			return nil, fmt.Errorf("build directory scope %q: %w", dir, err)
		}
		queries = append(queries, dirQ)
	}

	switch len(queries) {
	case 0:
		return nil, nil
	case 1:
		return queries[0], nil
	default:
		return query.NewOr(queries...), nil
	}
}

func pathWithin(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
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

func collapseDirectoryScopes(dirs []string) []string {
	if len(dirs) < 2 {
		return dirs
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !coveredByAnyDir(dir, out) {
			out = append(out, dir)
		}
	}
	return out
}

func dropFilesCoveredByDirs(files, dirs []string) []string {
	if len(files) == 0 || len(dirs) == 0 {
		return files
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		if !coveredByAnyDir(file, dirs) {
			out = append(out, file)
		}
	}
	return out
}

func coveredByAnyDir(name string, dirs []string) bool {
	for _, dir := range dirs {
		if name == dir || strings.HasPrefix(name, dir+"/") {
			return true
		}
	}
	return false
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

// corpusHashHexLen is the hex-encoded length of a corpus ID produced by
// newCorpusID — sha256 truncated to corpusHashBytes bytes, then hex-encoded.
// Used by the GC enumerator to filter unrelated entries in the corpora dir.
const (
	corpusHashBytes  = 16
	corpusHashHexLen = corpusHashBytes * 2
)

func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:corpusHashBytes])
}

func newCorpusID(parts ...string) corpusID {
	return corpusID(hashParts(parts...))
}

func indexOptionsHash() string {
	return hashParts(indexOptionsParts()...)
}

func indexOptionsParts() []string {
	return []string{
		"ctags_must_succeed", "true",
		"size_max", strconv.Itoa(maxIndexedDocumentBytes),
		"shard_max", strconv.Itoa(shardMax),
		"document_naming", seekDocumentNamingVersion,
		"zoekt_compatibility", zoektCompatibilityVersion,
	}
}

func gitCorpusFingerprint(paths gitPaths, state repoState) string {
	return stringsJoinNUL(
		"git-state-v2",
		"index_generation", seekIndexGeneration,
		"cache_layout", seekCacheLayoutVersion,
		"document_naming", seekDocumentNamingVersion,
		"zoekt_compatibility", zoektCompatibilityVersion,
		"index_options", indexOptionsHash(),
		"worktree", canonicalCorpusPath(paths.RepoDir),
		"common_dir", canonicalCorpusPath(paths.CommonDir),
		"repo_state", repoStateFingerprint(paths.RepoDir, state),
	)
}

func gitCorpusStateHash(paths gitPaths, state repoState) string {
	return computeStateHash(gitCorpusFingerprint(paths, state))
}

func stringsJoinNUL(parts ...string) string {
	if len(parts) == 0 {
		return ""
	}
	n := len(parts) - 1
	for _, part := range parts {
		n += len(part)
	}
	buf := make([]byte, 0, n)
	for i, part := range parts {
		if i > 0 {
			buf = append(buf, 0)
		}
		buf = append(buf, part...)
	}
	return string(buf)
}
