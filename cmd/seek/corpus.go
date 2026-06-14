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

	seekIndexGeneration       = "v2"
	seekCacheLayoutVersion    = "v2"
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
		return []corpusPlan{plan}, nil
	}

	var gitOperands []string
	external, err := collectExternalOperands(ctx, paths, operands)
	if err != nil {
		return nil, err
	}
	gitOperands = external.gitOperands

	plans := make([]corpusPlan, 0, 1+len(external.gitRoots)+len(external.roots))
	if len(gitOperands) > 0 {
		if paths == nil {
			return nil, fmt.Errorf("not a git repository")
		}
		plan, err := planCurrentGitCorpusWithOperands(*paths, gitOperands)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	for _, externalGit := range external.gitRoots {
		plan, err := planCurrentGitCorpusWithOperands(externalGit.paths, externalGit.operands)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	for _, root := range external.roots {
		plan, err := planFolderCorpus(root.path, root.info)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	if len(plans) == 0 {
		return nil, fmt.Errorf("no searchable corpus")
	}
	return plans, nil
}

type plannedExternalOperands struct {
	gitOperands []string
	gitRoots    []externalGitRoot
	roots       []externalRoot
}

type externalGitRoot struct {
	paths    gitPaths
	operands []string
}

type externalRoot struct {
	path string
	info os.FileInfo
}

func collectExternalOperands(ctx context.Context, paths *gitPaths, operands []string) (plannedExternalOperands, error) {
	var result plannedExternalOperands
	root := ""
	if paths != nil {
		root = canonicalCorpusPath(paths.RepoDir)
	}

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
		if root != "" && pathWithin(root, canonical) {
			result.gitOperands = append(result.gitOperands, operand)
			continue
		}
		if gitPaths, ok := resolveExternalGitOperand(ctx, canonical, info); ok {
			gitRoot := canonicalCorpusPath(gitPaths.RepoDir)
			group := externalGit[gitRoot]
			if group == nil {
				group = &externalGitRoot{paths: gitPaths}
				externalGit[gitRoot] = group
			}
			group.operands = append(group.operands, canonical)
			continue
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return result, fmt.Errorf("unsupported path operand: %s", operand)
		}
		external[canonical] = externalRoot{path: canonical, info: info}
	}

	result.gitRoots = sortedExternalGitRoots(externalGit)
	result.roots = collapseExternalRoots(external)
	return result, nil
}

func resolveExternalGitOperand(ctx context.Context, canonical string, info os.FileInfo) (gitPaths, bool) {
	dir := canonical
	if !info.IsDir() {
		dir = filepath.Dir(canonical)
	}
	paths, err := resolveGitPaths(ctx, dir)
	if err != nil {
		return gitPaths{}, false
	}
	if !pathWithin(canonicalCorpusPath(paths.RepoDir), canonical) {
		return gitPaths{}, false
	}
	return paths, true
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
		if coveredByExternalDir(path, selected) {
			continue
		}
		selected = append(selected, root)
	}
	return selected
}

func coveredByExternalDir(path string, roots []externalRoot) bool {
	for _, root := range roots {
		if !root.info.IsDir() {
			continue
		}
		if path == root.path || strings.HasPrefix(path, root.path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func planCurrentGitCorpus(paths gitPaths) (corpusPlan, error) {
	root := canonicalCorpusPath(paths.RepoDir)
	commonDir := canonicalCorpusPath(paths.CommonDir)
	plan, err := newCorpusPlan(
		corpusKindGit,
		rootTypeWorktree,
		root,
		"git",
		"git_worktree", root,
		"git_common_dir", commonDir,
	)
	if err != nil {
		return corpusPlan{}, err
	}
	plan.gitPaths = &paths
	return plan, nil
}

func planFolderCorpus(root string, info os.FileInfo) (corpusPlan, error) {
	rt := rootTypeFile
	if info.IsDir() {
		rt = rootTypeDirectory
	}

	return newCorpusPlan(corpusKindFolder, rt, root, "folder")
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
	plan, err := planCurrentGitCorpus(paths)
	if err != nil {
		return corpusPlan{}, err
	}
	if len(operands) == 0 {
		return plan, nil
	}
	scope, err := buildCurrentGitScope(plan.root, operands)
	if err != nil {
		return corpusPlan{}, err
	}
	plan.scope = scope
	return plan, nil
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
