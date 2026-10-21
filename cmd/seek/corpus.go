package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
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

	// v3 bumped when the published-family manifest landed: a corpus built by an
	// older binary has no .family-v1, and treating that absence as damage would
	// rebuild every corpus on every machine at once. Rotating the id instead
	// means a manifest-writing binary only ever meets corpora it built, and it
	// also stops two binary versions sharing one corpus directory — the version
	// skew that would otherwise republish without a manifest on every other run.
	seekIndexGeneration = "v3"
	// Bump once per release when a state-hash formula changes, so a new
	// binary never reads a state file that an old one wrote.
	seekCacheLayoutVersion    = "v3"
	seekDocumentNamingVersion = "slash-relative-v1"
)

// zoektCompatibilityVersion follows Zoekt's documented reindex markers.
var zoektCompatibilityVersion = fmt.Sprintf(
	"zoekt-index-v%d-feature-v%d",
	index.IndexFormatVersion,
	index.FeatureVersion,
)

type corpusPlan struct {
	id          corpusID
	kind        corpusKind
	rootType    rootType
	root        string
	displayRoot string
	// selectedFiles are the corpus-relative (slash) names that the user gave
	// as file operands. A result with one of these names gets its base name
	// as the fallback header (displayHeader). Formatter-only — never part of
	// the corpus ID.
	selectedFiles []string
	cacheDir      string
	indexDir      string
	// scope is the search-time filter for a scoped Git search. Both scoped
	// and unscoped searches of one repo resolve to the SAME combined
	// whole-repo index (cacheDir/indexDir); scope just restricts which loaded
	// docs match. The filter adds no BM25 term (executeParsedSearchScopedDirs),
	// so each matched doc gets the score of the unscoped search, and a scoped
	// result set equals the unscoped result set intersected with it.
	scope    query.Q
	gitPaths *gitPaths
	// folder is the graph expansion's capture of a folder node: its walk
	// fingerprint and the directories below its root that hold other nodes.
	// nil for a Git node and for a file root.
	folder *folderCapture
	// gitState is the graph expansion's git status of a Git node. Every index
	// preparation of the command uses it instead of a second status, until a
	// route sees the tree change.
	gitState *capturedGitState
}

// capturedGitState is the git status that the graph expansion captured. It
// serves every preparation of the command until a route sees the tree change.
type capturedGitState struct {
	state repoState
	stale atomic.Bool
}

// current returns the capture while no route has seen a change.
func (c *capturedGitState) current() (repoState, bool) {
	if c == nil || c.stale.Load() {
		return repoState{}, false
	}
	return c.state, true
}

// invalidate marks the capture stale; a nil capture does nothing.
func (c *capturedGitState) invalidate() {
	if c != nil {
		c.stale.Store(true)
	}
}

// treeChanged drops the node captures of plan, so every later preparation
// reads the tree again, and reports the change.
func treeChanged(plan corpusPlan) error {
	plan.gitState.invalidate()
	plan.folder.invalidate()
	return errGitIndexStateChanged
}

type corpusSearchResult struct {
	corpusID    corpusID
	kind        corpusKind
	displayRoot string
	// displayName is the header when no header is set (displayHeader): the
	// base name of a selected file, otherwise the corpus-relative FileName.
	displayName string
	file        zoekt.FileMatch
	// rankOverride is one-based. Zero keeps the normal BM25 ordering.
	rankOverride int
	// also holds the other paths whose content and match equal this result's,
	// in tie order.
	also []corpusCopy
	// header is the display path (applyHeaders); empty keeps the node-relative
	// form.
	header string
}

// corpusCopy is one collapsed copy: the formatter prints its header, the same
// path form as the result's header.
type corpusCopy struct {
	displayRoot string
	path        string
	header      string
}

func (c corpusCopy) displayPath() string {
	return filepath.Join(c.displayRoot, c.path)
}

type corpusIndexState uint8

const (
	corpusSearchable corpusIndexState = iota
	corpusKnownEmpty
	// corpusStale is a node whose build failed: its prior index stays
	// searchable with a warning, and it holds no captured state, so the
	// search reads its text index only.
	corpusStale
)

// planCorpora resolves the path operands and expands the graph from them.
// The caller gives "." when the user gave no path.
func planCorpora(ctx context.Context, operands []string) ([]corpusPlan, error) {
	seeds, err := resolveOperands(ctx, operands)
	if err != nil {
		return nil, err
	}
	plans, err := expandGraph(ctx, seeds)
	if err != nil {
		return nil, err
	}
	// Every node failed, each with its warning: the search has no result.
	if len(plans) == 0 {
		return nil, errNoMatch
	}
	return plans, nil
}

// buildGitCorpusPlan gives one physical repository one corpus identity, so
// every route to it uses one cache directory.
//
// git_committed_backend=native-v1 is the committed-reader migration barrier.
// It makes the first native request use a new cache, so native delta cannot
// seed a shard family made by the removed reader.
//
// rootTypeWorktree is the convention regardless of the on-disk layout: the
// corpus models the working tree, not the .git layout. The identity is the
// file identity (dev:ino) of the worktree and of the common directory, never a
// path: the typed letter case or a symlink in the path does not change it.
func buildGitCorpusPlan(repoDir, commonDir string, extraIDParts ...string) (corpusPlan, error) {
	root := canonicalCorpusPath(repoDir)
	commonID, err := filesystemIdentity(canonicalCorpusPath(commonDir))
	if err != nil {
		return corpusPlan{}, fmt.Errorf("stat git common dir: %w", err)
	}
	idParts := []string{"git_common_dir", commonID, "git_committed_backend", "native-v1"}
	idParts = append(idParts, extraIDParts...)
	return newCorpusPlan(corpusKindGit, rootTypeWorktree, root, "git", idParts...)
}

// planFolderCorpus plans a folder node: a directory or a single file.
func planFolderCorpus(root string, info os.FileInfo) (corpusPlan, error) {
	rt := rootTypeFile
	if info.IsDir() {
		rt = rootTypeDirectory
	}
	plan, err := newCorpusPlan(corpusKindFolder, rt, root, "folder")
	if err != nil {
		return corpusPlan{}, err
	}
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

	// The root identity (dev:ino) names the corpus; the root path is for
	// display only.
	idParts := []string{
		"kind", string(kind),
		"root_type", string(rt),
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

// planGitCorpus plans a Git node. operands are the paths that the search
// covers; none, or the work-tree root, covers the whole node.
func planGitCorpus(paths gitPaths, operands []string) (corpusPlan, error) {
	plan, err := buildGitCorpusPlan(paths.RepoDir, paths.CommonDir)
	if err != nil {
		return corpusPlan{}, err
	}
	plan.gitPaths = &paths
	if len(operands) == 0 {
		return plan, nil
	}
	// A scoped search reads the combined whole-repo index; the scope applies
	// at search time.
	plan.scope, err = buildCurrentGitScope(plan.root, operands)
	if err != nil {
		return corpusPlan{}, err
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

func seekUserCacheRoot() (string, error) {
	// SEEK_CACHE_DIR overrides the default user-cache location.
	// Benchmarks, tests, and package checks use it for an isolated cache.
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
// or Unicode normalization than Git stored; the Git scope
// (buildCurrentGitScope) matches names byte for byte, so without this a
// tracked file would be silently missed. Matching is by os.SameFile (device+inode),
// robust to both case and NFC/NFD. Components are corrected top-down so a
// wrong-case parent is fixed before its children are read. Best-effort: any
// stat/read error leaves that component as typed. listings holds the
// directories already read, so many operands in one directory read it once.
func realCaseWithin(root, path string, listings dirListings) string {
	rel, ok := relWithin(root, path)
	if !ok || rel == "." {
		return path
	}
	cur := root
	for _, comp := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, realCaseEntry(cur, comp, listings.read(cur)))
	}
	return cur
}

// dirListings caches directory listings for the case lookups of one command.
// A read error is cached as an empty listing.
type dirListings map[string][]os.DirEntry

func (listings dirListings) read(dir string) []os.DirEntry {
	entries, ok := listings[dir]
	if !ok {
		entries, _ = os.ReadDir(dir)
		listings[dir] = entries
	}
	return entries
}

// realCaseEntry returns the on-disk name of parent/name, given entries, the
// listing of parent.
func realCaseEntry(parent, name string, entries []os.DirEntry) string {
	// First check for an exact on-disk byte name.
	for _, e := range entries {
		if e.Name() == name {
			return name
		}
	}
	// For a case or normalization mismatch on an insensitive filesystem, find
	// the same file by device and inode.
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
