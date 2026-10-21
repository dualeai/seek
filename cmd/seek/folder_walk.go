package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/sys/unix"
)

// gitEntryName is the name of the Git entry that no folder walk indexes: a Git
// directory holds Git internals, and a `.git` file only points to one. Names
// compare without case: on a case-insensitive file system Git finds ".GIT".
const gitEntryName = ".git"

// folderCapture is what the graph expansion's walk of a folder node found. The
// state and the files serve every preparation of the command until a route
// sees the tree change; the skip set serves every walk of the node.
type folderCapture struct {
	stateHash string
	selected  []folderCandidate
	stale     atomic.Bool
	// skipDirs are the directories below the root that hold no content of
	// this node: Git nodes, failed nodes, and bare repositories.
	skipDirs map[string]struct{}
	// dataless counts the cloud-only files, which are searchable by name only.
	dataless int
	// unreadable counts the folders that the walk could not read; the graph
	// expansion warns for a kept node only.
	unreadable      int
	firstUnreadable string
}

// current returns the captured state and files while no route has seen a
// change.
func (c *folderCapture) current() (string, []folderCandidate, bool) {
	if c == nil || c.stale.Load() {
		return "", nil, false
	}
	return c.stateHash, c.selected, true
}

// invalidate marks the capture stale once a route sees the tree change.
func (c *folderCapture) invalidate() {
	if c != nil {
		c.stale.Store(true)
	}
}

// unreadableFolderWarned holds the first unreadable folder of each warning
// this command wrote, so a retry that walks the node again warns no more.
var unreadableFolderWarned sync.Map

// warnUnreadableFolders writes the one unreadable-folder warning of a kept
// folder node.
func warnUnreadableFolders(plan corpusPlan) {
	if plan.folder == nil || plan.folder.unreadable == 0 {
		return
	}
	if _, warned := unreadableFolderWarned.LoadOrStore(plan.folder.firstUnreadable, struct{}{}); !warned {
		slog.Warn("Some folders could not be read; their files are not searched",
			"root", plan.root, "folders", plan.folder.unreadable, "first", plan.folder.firstUnreadable)
	}
}

// captureFolderNode walks a folder node once for the graph expansion. It
// returns the capture and the Git roots below the node.
func captureFolderNode(ctx context.Context, plan corpusPlan) (*folderCapture, []gitPaths, error) {
	found := &folderBoundaries{skipDirs: make(map[string]struct{})}
	walk, err := walkFolderNode(ctx, plan, true, found)
	if err != nil {
		return nil, nil, err
	}
	capture := &folderCapture{
		stateHash:       walk.stateHash,
		selected:        walk.selected,
		skipDirs:        found.skipDirs,
		dataless:        walk.dataless,
		unreadable:      walk.unreadable,
		firstUnreadable: walk.firstUnreadable,
	}
	return capture, found.gitRoots, nil
}

// folderSkipDirs returns the directories that the walk of plan does not enter.
func folderSkipDirs(plan corpusPlan) map[string]struct{} {
	if plan.folder == nil {
		return nil
	}
	return plan.folder.skipDirs
}

// folderCorpusState walks the node and returns its state and its files,
// sorted by name.
func folderCorpusState(ctx context.Context, plan corpusPlan) (string, []folderCandidate, error) {
	walk, err := walkFolderNode(ctx, plan, true, nil)
	return walk.stateHash, walk.selected, err
}

// folderCorpusFingerprint walks the node and returns its state and its file
// count, with no file list.
func folderCorpusFingerprint(ctx context.Context, plan corpusPlan) (string, int, error) {
	walk, err := walkFolderNode(ctx, plan, false, nil)
	return walk.stateHash, walk.selectedCount, err
}

// folderWalk is the result of one walk of a folder node.
type folderWalk struct {
	stateHash     string
	selectedCount int
	// selected are the files, sorted by name; set when the walk collects them.
	selected        []folderCandidate
	unreadable      int
	firstUnreadable string
	dataless        int
}

// dirIdentity is the file identity of a directory.
type dirIdentity struct {
	dev, ino uint64
}

// folderDirIdentity reads the identity of an open directory. Tests replace it.
var folderDirIdentity = func(f *os.File) (dirIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return dirIdentity{}, err
	}
	return dirIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

// fileIsDataless reports a cloud-only file. Tests replace it.
var fileIsDataless = func(_ string, st *unix.Stat_t) bool {
	return statDataless(st)
}

// walkFolderNode walks one folder node. Directories go through one shared
// queue that every worker takes from (GOMAXPROCS workers). Each directory gets
// its own digest, and the state hash combines the digests in the order of
// their relative paths, so worker timing never changes it. found is set only
// for the graph expansion's walk, which records the boundaries; every later
// walk skips the recorded directories.
func walkFolderNode(ctx context.Context, plan corpusPlan, collect bool, found *folderBoundaries) (folderWalk, error) {
	switch plan.rootType {
	case rootTypeFile:
		return walkFolderFileRoot(plan, collect)
	case rootTypeDirectory:
	default:
		return folderWalk{}, fmt.Errorf("unsupported folder root type: %s", plan.rootType)
	}
	w := &folderWalker{
		ctx:      ctx,
		collect:  collect,
		found:    found,
		skipDirs: folderSkipDirs(plan),
		visited:  make(map[dirIdentity]string),
	}
	w.cond = sync.NewCond(&w.mu)
	w.push(plan.root, "")
	var workers sync.WaitGroup
	for range indexParallelism() {
		workers.Go(w.work)
	}
	workers.Wait()
	if w.err != nil {
		return folderWalk{}, w.err
	}
	if err := ctx.Err(); err != nil {
		return folderWalk{}, err
	}
	return w.result(plan), nil
}

// walkFolderFileRoot captures a single-file node.
func walkFolderFileRoot(plan corpusPlan, collect bool) (folderWalk, error) {
	info, err := os.Lstat(plan.root)
	if err != nil {
		return folderWalk{}, fmt.Errorf("read file root: %w", err)
	}
	if !info.Mode().IsRegular() {
		return folderWalk{}, fmt.Errorf("unsupported file root: %s", plan.root)
	}
	candidate := newFolderCandidate(filepath.Base(plan.root), plan.root, info)
	h := newFolderStateHasher(plan)
	appendFolderCandidateHash(h, candidate)
	walk := folderWalk{stateHash: finishFolderStateHash(h), selectedCount: 1}
	if collect {
		walk.selected = []folderCandidate{candidate}
	}
	return walk, nil
}

// folderWalker is the shared state of one directory walk.
type folderWalker struct {
	ctx      context.Context
	collect  bool
	found    *folderBoundaries
	skipDirs map[string]struct{}

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []folderDirJob
	pending int // queued and running jobs
	err     error
	// visited maps each directory identity to the smallest relative path
	// that reached it. A loop is cut at its inner path, because an
	// ancestor is always walked before its descendants; two other paths to one
	// directory keep the smallest, so the result does not depend on timing.
	visited map[dirIdentity]string
	dirs    []folderDirResult

	unreadable      atomic.Int64
	firstUnreadable string
}

// folderDirJob is one directory to visit: its path and its path below the
// root.
type folderDirJob struct {
	path, rel string
}

// folderDirResult is one directory's share of the state.
type folderDirResult struct {
	rel        string
	identity   dirIdentity
	digest     uint64
	files      int
	dataless   int
	candidates []folderCandidate
}

// push queues one directory and wakes one waiting worker.
func (w *folderWalker) push(path, rel string) {
	w.mu.Lock()
	w.queue = append(w.queue, folderDirJob{path: path, rel: rel})
	w.pending++
	w.mu.Unlock()
	w.cond.Signal()
}

// next returns the next job. It returns false when the walk is over: every
// job ran, or one failed.
func (w *folderWalker) next() (folderDirJob, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.queue) == 0 && w.pending > 0 && w.err == nil {
		w.cond.Wait()
	}
	if len(w.queue) == 0 || w.err != nil {
		return folderDirJob{}, false
	}
	job := w.queue[len(w.queue)-1]
	w.queue = w.queue[:len(w.queue)-1]
	return job, true
}

// finish ends one job and keeps the first error. The last job, or an error,
// wakes every waiting worker.
func (w *folderWalker) finish(err error) {
	w.mu.Lock()
	w.pending--
	if err != nil && w.err == nil {
		w.err = err
	}
	done := w.pending == 0 || w.err != nil
	w.mu.Unlock()
	if done {
		w.cond.Broadcast()
	}
}

// work runs jobs until the walk is over. A canceled context fails the next
// job.
func (w *folderWalker) work() {
	for {
		job, ok := w.next()
		if !ok {
			return
		}
		err := w.ctx.Err()
		if err == nil {
			err = w.visit(job)
		}
		w.finish(err)
	}
}

// claim records the identity of a directory and reports whether the walk
// enters it.
func (w *folderWalker) claim(id dirIdentity, rel string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	previous, seen := w.visited[id]
	switch {
	case !seen:
	case previous == "" || strings.HasPrefix(rel, previous+"/") || comparePaths(rel, previous) >= 0:
		// A loop back to an ancestor, or a larger path to a directory
		// already reached.
		return false
	}
	w.visited[id] = rel
	return true
}

// comparePaths orders relative paths by their components: "a/s" sorts before
// "a-b/s" because "a" sorts before "a-b". The order is the same for every
// pair of twin paths below two directories, so the smallest-path rule picks
// the same directory every run.
func comparePaths(a, b string) int {
	return strings.Compare(strings.ReplaceAll(a, "/", "\x00"), strings.ReplaceAll(b, "/", "\x00"))
}

// visit reads one directory: its files go into its digest, and its
// subdirectories into the queue.
func (w *folderWalker) visit(job folderDirJob) error {
	dirFile, entries, err := openSortedDirectory(job.path)
	if err != nil {
		if job.rel == "" {
			return fmt.Errorf("read folder root: %w", err)
		}
		if errors.Is(err, unix.EMFILE) || errors.Is(err, unix.ENFILE) {
			// Out of file descriptors is no property of the folder: a walk
			// that went on would publish a partial state.
			return fmt.Errorf("read folder %s: %w", job.path, err)
		}
		w.recordUnreadable(job.path)
		return nil
	}
	defer func() { _ = dirFile.Close() }()
	id, err := folderDirIdentity(dirFile)
	if err != nil {
		w.recordUnreadable(job.path)
		return nil
	}
	if !w.claim(id, job.rel) {
		return nil
	}
	if job.rel != "" && w.found != nil && w.found.claim(w.ctx, job.path, entries) {
		return nil
	}
	result := folderDirResult{rel: job.rel, identity: id}
	h := xxhash.New()
	dirFD := int(dirFile.Fd())
	for _, entry := range entries {
		name := entry.Name()
		if isGitEntry(job.path, name) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		rel := name
		if job.rel != "" {
			rel = job.rel + "/" + name
		}
		path := job.path + string(filepath.Separator) + name
		if entry.IsDir() {
			if _, skip := w.skipDirs[path]; !skip {
				w.push(path, rel)
			}
			continue
		}
		stat, ok := fstatatRegularFile(dirFD, name)
		if !ok {
			continue
		}
		candidate := folderCandidate{
			name:     rel,
			path:     path,
			mode:     stat.mode,
			size:     stat.size,
			mtime:    stat.mtime,
			ctime:    stat.ctime,
			dev:      stat.dev,
			ino:      stat.ino,
			dataless: stat.dataless,
		}
		if candidate.dataless {
			result.dataless++
		}
		appendFolderCandidateHash(h, candidate)
		result.files++
		if w.collect {
			result.candidates = append(result.candidates, candidate)
		}
	}
	result.digest = h.Sum64()
	w.mu.Lock()
	w.dirs = append(w.dirs, result)
	w.mu.Unlock()
	return nil
}

// recordUnreadable counts a directory that the walk cannot read. The graph
// expansion's walk also records it as not entered, so a seed below it stays
// its own node.
func (w *folderWalker) recordUnreadable(path string) {
	w.mu.Lock()
	if w.unreadable.Add(1) == 1 || path < w.firstUnreadable {
		w.firstUnreadable = path
	}
	w.mu.Unlock()
	if w.found != nil {
		w.found.mu.Lock()
		w.found.skipDirs[path] = struct{}{}
		w.found.mu.Unlock()
	}
}

// result combines the directory digests in path order. A directory that lost
// its identity to a smaller path is dropped with its files.
func (w *folderWalker) result(plan corpusPlan) folderWalk {
	dirs := slices.DeleteFunc(w.dirs, func(d folderDirResult) bool {
		return w.visited[d.identity] != d.rel
	})
	slices.SortFunc(dirs, func(a, b folderDirResult) int { return strings.Compare(a.rel, b.rel) })
	h := newFolderStateHasher(plan)
	walk := folderWalk{
		unreadable:      int(w.unreadable.Load()),
		firstUnreadable: w.firstUnreadable,
	}
	var numBuf [20]byte
	for _, dir := range dirs {
		if dir.files == 0 {
			continue
		}
		appendFolderFingerprintPart(h, "dir")
		appendFolderFingerprintPart(h, dir.rel)
		appendFolderFingerprintBytes(h, strconv.AppendUint(numBuf[:0], dir.digest, 10))
		walk.selectedCount += dir.files
		walk.dataless += dir.dataless
		if w.collect {
			walk.selected = append(walk.selected, dir.candidates...)
		}
	}
	slices.SortFunc(walk.selected, func(a, b folderCandidate) int { return strings.Compare(a.name, b.name) })
	walk.stateHash = finishFolderStateHash(h)
	return walk
}

func openSortedDirectory(dir string) (*os.File, []os.DirEntry, error) {
	dirFile, err := os.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		_ = dirFile.Close()
		return nil, nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return dirFile, entries, nil
}

// folderFileStat is the stat of one regular file that the walk keeps.
type folderFileStat struct {
	mode     os.FileMode
	size     int64
	mtime    int64
	ctime    int64
	dev      uint64
	ino      uint64
	dataless bool
}

// fstatatRegularFile stats name in the open directory dirFD, with no symlink
// follow and no open of the file. It reports false for any other file type.
func fstatatRegularFile(dirFD int, name string) (folderFileStat, bool) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return folderFileStat{}, false
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return folderFileStat{}, false
	}
	return folderFileStat{
		mode:     regularFileMode(uint32(st.Mode)),
		size:     st.Size,
		mtime:    st.Mtim.Nano(),
		ctime:    st.Ctim.Nano(),
		dev:      uint64(st.Dev),
		ino:      uint64(st.Ino),
		dataless: fileIsDataless(name, &st),
	}, true
}

func regularFileMode(mode uint32) os.FileMode {
	out := os.FileMode(mode & 0o777)
	if mode&unix.S_ISUID != 0 {
		out |= os.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		out |= os.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		out |= os.ModeSticky
	}
	return out
}
