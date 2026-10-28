package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

	// seekIndexGeneration v3 marks the corpora that have the published-family
	// manifest (.family-v1). A corpus that an older binary built has no
	// manifest. To treat that as damage would rebuild every corpus on every
	// machine at once. With a new ID, a binary that writes the manifest meets
	// only corpora that it built. Two binary versions also do not share one
	// corpus directory, so one of them does not publish with no manifest on
	// every other run.
	seekIndexGeneration = "v3"
	// seekCacheLayoutVersion changes, once per release, when a state-hash
	// formula changes. A new binary then never reads a state file that an old
	// one wrote.
	seekCacheLayoutVersion    = "v3"
	seekDocumentNamingVersion = "slash-relative-v1"
)

// zoektCompatibilityVersion follows Zoekt's documented reindex markers.
var zoektCompatibilityVersion = fmt.Sprintf(
	"zoekt-index-v%d-feature-v%d",
	index.IndexFormatVersion,
	index.FeatureVersion,
)

// corpusStore is the on-disk place of one corpus: cacheDir holds the state
// files and locks; indexDir holds the published shards.
type corpusStore struct {
	cacheDir string
	indexDir string
}

// node is one node of the search graph: a folder root or a Git work tree, with
// its cache place and its per-command captures. kind is the data tag; behavior
// does the kind's work.
type node struct {
	id          corpusID
	kind        corpusKind
	rootType    rootType
	root        string
	displayRoot string
	corpusStore
	// scope is the search-time filter for a scoped Git search. A scoped and an
	// unscoped search of one repo read the same combined whole-repo index
	// (cacheDir/indexDir); scope only limits which loaded documents match. The
	// filter adds no BM25 term (searchShards), so each matched document gets
	// the score of the unscoped search. A scoped result set is the part of the
	// unscoped result set that the scope holds.
	scope query.Q
	// behavior is the kind's work (nodeKind); nil when the node only names a
	// cache place, as for the error of a failed Git node (gitNodeError).
	behavior nodeKind
	// folder is the graph expansion's capture of a folder node: its walk
	// fingerprint and the directories below its root that hold other nodes.
	// nil for a Git node. A file root has a capture too: its read binds the
	// vectors through the capture's state.
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
func treeChanged(plan node) error {
	plan.gitState.invalidate()
	plan.folder.invalidate()
	return errGitIndexStateChanged
}

type corpusSearchResult struct {
	corpusID    corpusID
	kind        corpusKind
	displayRoot string
	file        zoekt.FileMatch
	// rankOverride is one-based. Zero keeps the normal BM25 ordering.
	rankOverride int
	// also holds the other paths whose content and match equal this result's,
	// in tie order.
	also []corpusCopy
	// header is the display path that the formatter prints (applyHeaders).
	header string
	// modelBase is the model path base of the node (modelPathBase).
	modelBase string
}

// corpusCopy is one collapsed copy: the formatter prints its header, the same
// path form as the result's header.
type corpusCopy struct {
	displayRoot string
	path        string
	header      string
}

type corpusIndexState uint8

const (
	corpusSearchable corpusIndexState = iota
	corpusKnownEmpty
	// corpusStale is a node whose build failed (with a warning) or that a
	// peer builds: its prior index stays searchable. That index does not hold
	// the captured state, so the search reads its text index only. A Git
	// node keeps the dirty files of its capture.
	corpusStale
)

// planCorpora resolves the path operands and expands the graph from them.
// The caller gives "." when the user gave no path.
func planCorpora(ctx context.Context, operands []string) ([]node, error) {
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
// The ID part git_committed_backend=native-v1 names the reader of committed
// content. A corpus that another committed reader built has another ID, so a
// native delta never extends a shard family that another reader made.
//
// The root type is rootTypeWorktree for every on-disk layout: the corpus
// models the working tree, not the .git layout. The identity is the file
// identity (dev:ino) of the worktree and of the common directory, never a
// path. So the typed letter case or a symlink in the path does not change it.
func buildGitCorpusPlan(repoDir, commonDir string) (node, error) {
	root := canonicalCorpusPath(repoDir)
	commonID, err := filesystemIdentity(canonicalCorpusPath(commonDir))
	if err != nil {
		return node{}, fmt.Errorf("stat git common dir: %w", err)
	}
	return newCorpusPlan(corpusKindGit, rootTypeWorktree, root,
		"git_common_dir", commonID, "git_committed_backend", "native-v1")
}

// planFolderCorpus plans a folder node: a directory or a single file.
func planFolderCorpus(root string, info os.FileInfo) (node, error) {
	rt := rootTypeFile
	if info.IsDir() {
		rt = rootTypeDirectory
	}
	plan, err := newCorpusPlan(corpusKindFolder, rt, root)
	if err != nil {
		return node{}, err
	}
	plan.behavior = &folderNode{}
	if rt == rootTypeFile {
		// A single-file corpus indexes the file under its base name. Its
		// display root is the parent folder, so the display root joined to
		// the name is the file path (applyHeaders).
		plan.displayRoot = filepath.Dir(root)
	}
	return plan, nil
}

// newCorpusPlan gives the node of one corpus. Its ID hashes the kind, the root
// type, the root's file identity, the format versions, the build options
// (indexOptionsHash), and extraIDParts. A stat error of the root names the
// kind.
func newCorpusPlan(kind corpusKind, rt rootType, root string, extraIDParts ...string) (node, error) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return node{}, err
	}

	root = canonicalCorpusPath(root)
	rootID, err := filesystemIdentity(root)
	if err != nil {
		return node{}, fmt.Errorf("stat %s root: %w", kind, err)
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
	return node{
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
func planGitCorpus(paths gitPaths, operands []string) (node, error) {
	plan, err := buildGitCorpusPlan(paths.RepoDir, paths.CommonDir)
	if err != nil {
		return node{}, err
	}
	plan.behavior = &gitNode{paths: paths}
	if len(operands) == 0 {
		return plan, nil
	}
	// A scoped search reads the combined whole-repo index; the scope applies
	// at search time.
	spec, err := buildGitScopeSpec(plan.root, operands)
	if err != nil {
		return node{}, err
	}
	if plan.scope, err = spec.query(); err != nil {
		return node{}, err
	}
	return plan, nil
}

// pathWithin reports whether path is root or a descendant of it (bool-only
// containment; relWithin is the variant that also returns the relative path).
// The file-system root "/" ends with the separator, so it holds every path
// that starts with it.
func pathWithin(root, path string) bool {
	rest, ok := strings.CutPrefix(path, root)
	return ok && (rest == "" || rest[0] == filepath.Separator || root == string(filepath.Separator))
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

// filesystemIdentity returns the file identity of path as "dev:ino" in
// decimal digits. It follows symlinks.
func filesystemIdentity(path string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	return strconv.FormatUint(uint64(st.Dev), 10) + ":" +
		strconv.FormatUint(st.Ino, 10), nil
}

// canonicalCorpusPath returns path with its symlinks resolved, or the clean
// absolute path when a resolve fails.
func canonicalCorpusPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved // EvalSymlinks returns a clean path.
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs // Abs returns a clean path.
	}
	return filepath.Clean(path)
}

// realCaseWithin rewrites the part of path below root to the directory-entry
// names on disk. On a case- or normalization-insensitive file system (the
// macOS default), an operand can have a different case or Unicode
// normalization than Git stored. The Git scope (gitScopeSpec.query) compares
// names byte for byte, so without this rewrite the search would miss a
// tracked file and give no error. os.SameFile (device and inode) finds the
// entry, so case and NFC/NFD both match. It corrects the components from the
// top down, so a parent has its real name before Seek reads its children. A
// stat or read error leaves that component as typed. listings holds the
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
