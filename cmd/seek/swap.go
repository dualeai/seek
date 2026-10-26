package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// A builder creates a shard generation in a temporary directory, validates its
// state, and publishes it under the publish lock. Normal readers hold a shared
// lock while they list, open, and search shards, so they do not interleave with
// a successful swap. A reader waits for a publish until it ends or the user
// cancels; it never reads the shards unlocked.
// A failed swap moves the live family back; recoverIncompleteSwap uses
// .swapping to repair an interrupted swap, or a failed move back, on the next
// build.

// shardFamily selects the shards for a swap or scan. A committed rebuild keeps
// the uncommitted family. A folder build replaces all shards.
type shardFamily int

const (
	// familyAll is every shard and sidecar.
	familyAll shardFamily = iota
	// familyCommitted is every entry except the uncommitted family.
	familyCommitted
)

const (
	// shardSuffix and metaSuffix name the two file kinds a shard family owns.
	// Zoekt pairs a shard with its .meta sidecar (tombstones), so Seek always
	// moves and removes the two together. A shard without its sidecar can make
	// deleted documents searchable again.
	shardSuffix = ".zoekt"
	metaSuffix  = ".zoekt.meta"
)

// label is the family's persisted token inside the .swapping marker file (read
// back by shardFamilyFromLabel during crash recovery).
func (fam shardFamily) label() string {
	if fam == familyCommitted {
		return "committed"
	}
	return "all"
}

// shardFamilyFromLabel is the inverse of shardFamily.label.
func shardFamilyFromLabel(s string) shardFamily {
	if s == "committed" {
		return familyCommitted
	}
	return familyAll
}

// isUncommittedShard reports whether base is a file of the uncommitted family
// (uncommitted_v<format>.<n>.zoekt, or its .meta sidecar). The "_v" keeps out
// a repository name that only starts with "uncommitted".
func isUncommittedShard(base string) bool {
	return strings.HasPrefix(base, repoUncommitted+"_v")
}

// familyScan contains one consistent scan of a shard directory. It includes
// non-directory .zoekt files and their .zoekt.meta sidecars. It also records
// the data needed for presence, numbering, family, path, and manifest checks.
//
// This scan uses os.ReadDir because it reports directory read errors. A partial
// list could cause publication of an incomplete family.
type familyScan struct {
	dir     string
	members []familyMember // sorted by name
}

type familyMember struct {
	name        string // basename
	size        int64  // Lstat size; a symlink reports the link length
	uncommitted bool
	shard       bool // .zoekt; false for the .zoekt.meta sidecar
	prefix      string
	num         int
	numbered    bool
}

// scanFamily reads dir once and reports a read failure to the caller.
func scanFamily(dir string) (familyScan, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return familyScan{}, fmt.Errorf("read shard dir %s: %w", dir, err)
	}
	sc := familyScan{dir: dir, members: make([]familyMember, 0, len(entries))}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		isShard := strings.HasSuffix(name, shardSuffix)
		if !isShard && !strings.HasSuffix(name, metaSuffix) {
			continue
		}
		m := familyMember{name: name, uncommitted: isUncommittedShard(name), shard: isShard}
		if isShard {
			m.prefix, m.num, m.numbered = parseShardName(name)
		}
		// DirEntry.Info has Lstat semantics, so a symlink reports its link
		// length instead of the size of the target file.
		if fi, ierr := ent.Info(); ierr == nil {
			m.size = fi.Size()
		} else {
			m.size = -1
		}
		sc.members = append(sc.members, m)
	}
	slices.SortFunc(sc.members, func(a, b familyMember) int { return strings.Compare(a.name, b.name) })
	return sc, nil
}

// familyShardNames lists the .zoekt entries that a search can try to open. It
// does not load size metadata. A missing directory gives no entry and no error.
//
// The freshness scan runs before the build and outside the publish lock. A
// search must list shards again while it holds the shared publish lock.
//
// It uses the same membership rule and name order as scanFamily.
func familyShardNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read shard dir %s: %w", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), shardSuffix) {
			out = append(out, filepath.Join(dir, ent.Name()))
		}
	}
	return out, nil
}

// scanFamilyOptional reads dir once and treats "no such directory" as an empty
// family. So a caller that checks a corpus needs no special case for a corpus
// with no build yet, or one that gc just evicted.
func scanFamilyOptional(dir string) (familyScan, error) {
	sc, err := scanFamily(dir)
	if err != nil {
		// scanFamily wraps the ReadDir error, so use errors.Is here.
		if errors.Is(err, fs.ErrNotExist) {
			return familyScan{dir: dir}, nil
		}
		return familyScan{dir: dir}, err
	}
	return sc, nil
}

// hasShard reports whether the family has a shard. Sidecars alone do not make
// a family present.
func (s familyScan) hasShard(fam shardFamily) bool {
	return slices.ContainsFunc(s.members, func(m familyMember) bool { return m.shard && inFamily(fam, m) })
}

// hasMember reports whether the family has a shard or sidecar.
func (s familyScan) hasMember(fam shardFamily) bool {
	return slices.ContainsFunc(s.members, func(m familyMember) bool { return inFamily(fam, m) })
}

// parseShardName splits <repo>_v<format>.<NNNNN>.zoekt into its family prefix
// and shard number.
func parseShardName(base string) (prefix string, num int, ok bool) {
	stem, found := strings.CutSuffix(base, shardSuffix)
	if !found || len(stem) < 6 { // needs at least ".NNNNN"
		return "", 0, false
	}
	if stem[len(stem)-6] != '.' {
		return "", 0, false
	}
	n := 0
	for _, c := range []byte(stem[len(stem)-5:]) {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		n = n*10 + int(c-'0')
	}
	return stem[:len(stem)-6], n, true
}

func inFamily(fam shardFamily, m familyMember) bool {
	return fam != familyCommitted || !m.uncommitted
}

// paths returns the family's shards and sidecars. The committed family is all
// entries outside the fixed uncommitted family, so the caller needs no
// committed repository name.
func (s familyScan) paths(fam shardFamily) []string {
	out := make([]string, 0, len(s.members))
	for _, m := range s.members {
		if inFamily(fam, m) {
			out = append(out, filepath.Join(s.dir, m.name))
		}
	}
	return out
}

// contiguous reports whether each selected shard prefix is numbered 0..n-1
// with no gap.
//
// Zoekt derives the next shard number from the contiguous run that it finds. A
// delta based on a gapped family can replace a later shard. This check controls
// whether the writer can seed a delta build; it does not find missing shards
// at the end of a run.
//
// Basename order does not group a prefix's shards ("a.00001.00000.zoekt" sorts
// between "a"'s shards 0 and 1), so sort the parsed pairs before the walk.
func (s familyScan) contiguous(fam shardFamily) bool {
	idx := make([]int, 0, len(s.members))
	for i, m := range s.members {
		if m.shard && m.numbered && inFamily(fam, m) {
			idx = append(idx, i)
		}
	}
	slices.SortFunc(idx, func(a, b int) int {
		x, y := s.members[a], s.members[b]
		if c := strings.Compare(x.prefix, y.prefix); c != 0 {
			return c
		}
		return x.num - y.num
	})
	want := 0
	for k, i := range idx {
		m := s.members[i]
		if k == 0 || m.prefix != s.members[idx[k-1]].prefix {
			want = 0
		}
		if m.num != want {
			return false
		}
		want++
	}
	return true
}

// pathsAreContiguous checks an explicit shard list. The uncommitted indexer
// already has such a list, grouped by repository name.
func pathsAreContiguous(files []string) bool {
	sc := familyScan{}
	for _, f := range files {
		base := filepath.Base(f)
		m := familyMember{name: base, shard: strings.HasSuffix(base, shardSuffix)}
		if m.shard {
			m.prefix, m.num, m.numbered = parseShardName(base)
		}
		sc.members = append(sc.members, m)
	}
	return sc.contiguous(familyAll)
}

// familyShardFiles reads dir once and returns the family's shards and sidecars.
func familyShardFiles(dir string, fam shardFamily) ([]string, error) {
	sc, err := scanFamily(dir)
	if err != nil {
		return nil, err
	}
	return sc.paths(fam), nil
}

// familyComplete reports whether the scan holds a published family that a
// search can use. The family needs at least one shard, and exactly the set
// that the last publish recorded. Presence alone accepts one survivor of a
// torn family, so it always checks both.
func (s familyScan) familyComplete(indexDir string) bool {
	return s.hasShard(familyAll) && s.matchesManifest(indexDir)
}

// familyManifestFile records the name and size of each published shard and
// sidecar. It stays in indexDir so removing the index also removes its
// manifest.
const familyManifestFile = ".family-v1"

// writeFamilyManifest records the intended published set. The caller supplies
// this set because a scan after publication could include an unrelated file
// that appeared during the swap.
func writeFamilyManifest(indexDir string, names []string) error {
	sorted := append([]string(nil), names...)
	slices.Sort(sorted)
	var b strings.Builder
	b.WriteString("v1\n")
	for _, name := range sorted {
		st, err := os.Lstat(filepath.Join(indexDir, name))
		if err != nil {
			return fmt.Errorf("stat published shard %s: %w", name, err)
		}
		fmt.Fprintf(&b, "%s %d\n", name, st.Size())
	}
	return writeCacheFile(indexDir, familyManifestFile, b.String())
}

// removeFamilyManifest removes the family manifest of indexDir and its
// temporary file. It keeps the shards.
func removeFamilyManifest(indexDir string) {
	_ = os.Remove(filepath.Join(indexDir, familyManifestFile))
	_ = os.Remove(filepath.Join(indexDir, familyManifestFile+".tmp"))
}

// matchesManifest reports whether the scanned directory still holds exactly the
// files the last publish recorded, at exactly the sizes it recorded.
//
// A presence or numbering check cannot find missing shards at the end of a
// run. The manifest can find them, because it records every published member.
//
// Set equality, not a subset: an extra file is damage too. A publish lists the
// live family before it moves the family aside, so it can leave an unrecorded
// file. A missing entry is damage even when it is a .meta sidecar. A shard
// without its sidecar, or with a stale sidecar of the same number, can make
// deleted content searchable again.
//
// Both lists are sorted by name, so a positional comparison checks equality.
func (s familyScan) matchesManifest(indexDir string) bool {
	return s.matchesManifestFamily(indexDir, familyAll)
}

// matchesManifestFamily restricts the comparison to one family.
//
// The committed and uncommitted families use different rebuild paths. A
// mismatch in one family must not mark the other family as damaged.
//
// On the manifest side, the recorded name decides membership. scanFamily
// applies the same rule on the directory side, so the two walks stay in step.
func (s familyScan) matchesManifestFamily(indexDir string, fam shardFamily) bool {
	raw := readCacheFile(indexDir, familyManifestFile)
	if raw == "" {
		return false
	}
	header, rest, _ := strings.Cut(raw, "\n")
	if strings.TrimSpace(header) != "v1" {
		return false
	}
	i := 0
	// advance walks the scan to the next member of fam.
	advance := func() {
		for i < len(s.members) && !inFamily(fam, s.members[i]) {
			i++
		}
	}
	advance()
	for rest != "" {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.LastIndexByte(line, ' ')
		if idx <= 0 {
			return false
		}
		name := line[:idx]
		if !inFamily(fam, familyMember{name: name, uncommitted: isUncommittedShard(name)}) {
			continue
		}
		size, err := strconv.ParseInt(line[idx+1:], 10, 64)
		if err != nil {
			return false
		}
		if i >= len(s.members) {
			return false
		}
		if m := s.members[i]; m.name != name || m.size != size {
			return false
		}
		i++
		advance()
	}
	// Fewer recorded entries than members means that the directory contains an
	// unrecorded file.
	return i == len(s.members)
}

// seedFamilyFiles links all specified members. It fails if a validated source
// disappears instead of publishing a shorter family.
func seedFamilyFiles(buildDir string, srcs []string) error {
	for _, src := range srcs {
		dst := filepath.Join(buildDir, filepath.Base(src))
		if err := os.Link(src, dst); err != nil {
			return fmt.Errorf("seed shard %s: %w", filepath.Base(src), err)
		}
	}
	return nil
}

var buildDirCounter atomic.Uint64

// newBuildDir creates a unique temporary directory under indexDir. This keeps
// publication renames on one filesystem. Maintenance scans skip the dot prefix.
func newBuildDir(indexDir string) (string, error) {
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return "", fmt.Errorf("create index directory: %w", err)
	}
	name := fmt.Sprintf("%s%d-%d-%d", buildDirPrefix, os.Getpid(), time.Now().UnixNano(), buildDirCounter.Add(1))
	dir := filepath.Join(indexDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create build directory: %w", err)
	}
	return dir, nil
}

// discardBuildDir removes a build directory after a failure, or after a
// publish, when it holds the old live files. Best effort; the gc orphan sweep
// (sweepOrphanBuildDirs) removes what is left.
func discardBuildDir(buildDir string) {
	_ = os.RemoveAll(buildDir)
}

// publishGeneration replaces a shard family, then writes its manifest and its
// state, while it holds the exclusive publish lock. Normal locked readers
// cannot interleave with these writes. This multi-file swap is not one
// filesystem transaction. A failed step moves the live family back. .swapping
// records an interrupted swap, or a failed move back, for repair on the next
// build. If GC removed the corpus during the build, it returns
// errCorpusEvicted, and the caller discards the build.
func (store corpusStore) publishGeneration(ctx context.Context, buildDir string, fam shardFamily, writeState func() error) error {
	pub, err := acquirePublishLock(ctx, store.cacheDir)
	if err != nil {
		return err // includes errCorpusEvicted
	}
	defer releaseLock(pub)

	// GC can evict the corpus (rename it to trash) while the build runs outside
	// the publish lock, also while the build holds its build lock.
	if _, err := os.Stat(store.indexDir); err != nil {
		if os.IsNotExist(err) {
			return errCorpusEvicted
		}
		return fmt.Errorf("stat index dir before swap: %w", err)
	}
	published, err := store.publishShardFamilyLocked(buildDir, fam)
	if err != nil {
		return err
	}
	// A wholesale publish replaces the complete family.
	if err := writeFamilyManifest(store.indexDir, published); err != nil {
		return err
	}
	return writeState()
}

// publishShardFamilyLocked swaps one family (swapFiles). The caller must hold
// the publish lock, confirm that the corpus directory exists, and discard
// buildDir, which then holds the old files. .swapping marks the swap from its
// first move: a crash, or a failed move back, leaves it, and the next build
// repairs the family.
func (store corpusStore) publishShardFamilyLocked(buildDir string, fam shardFamily) ([]string, error) {
	// Enumerate both sides before touching anything: a read failure must abort
	// the swap, never move the live family and rename a partial set in.
	live, err := familyShardFiles(store.indexDir, fam)
	if err != nil {
		return nil, err
	}
	incoming, err := familyShardFiles(buildDir, fam)
	if err != nil {
		return nil, err
	}
	if err := writeCacheFile(store.cacheDir, swappingMarkerFile, fam.label()); err != nil {
		return nil, fmt.Errorf("write swapping marker: %w", err)
	}
	published, restored, err := swapFiles(store.indexDir, buildDir, live, incoming)
	if err != nil {
		if restored {
			removeCacheFile(store.cacheDir, swappingMarkerFile)
		}
		return nil, err
	}
	removeCacheFile(store.cacheDir, swappingMarkerFile)
	return published, nil
}

// swapFiles moves the live files into a new directory in buildDir
// (liveAsidePrefix), then renames each incoming file into indexDir. When a
// step fails, it removes the new files and moves the live files back, and
// restored reports whether that worked. So the live family stays whole even
// when one of its files can be neither moved nor removed (an immutable file).
// A delete of the live files first would then leave a family that lost
// content.
func swapFiles(indexDir, buildDir string, live, incoming []string) (published []string, restored bool, err error) {
	aside, err := os.MkdirTemp(buildDir, liveAsidePrefix)
	if err != nil {
		return nil, true, fmt.Errorf("create the directory for the live shards: %w", err)
	}
	var moved []string
	rollBack := func(err error) ([]string, bool, error) {
		restored := true
		for _, base := range published {
			if removeErr := os.Remove(filepath.Join(indexDir, base)); removeErr != nil {
				restored = false
			}
		}
		for _, f := range moved {
			if renameErr := os.Rename(filepath.Join(aside, filepath.Base(f)), f); renameErr != nil {
				restored = false
			}
		}
		return nil, restored, err
	}
	for _, f := range live {
		if err := os.Rename(f, filepath.Join(aside, filepath.Base(f))); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return rollBack(fmt.Errorf("move live shard %s aside: %w", f, err))
		}
		moved = append(moved, f)
	}
	published = make([]string, 0, len(incoming))
	for _, f := range incoming {
		base := filepath.Base(f)
		dst := filepath.Join(indexDir, base)
		// A directory at this path is not in live, so the move did not clear
		// it. Remove any non-regular replacement before Rename.
		if st, statErr := os.Lstat(dst); statErr == nil && !st.Mode().IsRegular() {
			if rmErr := os.RemoveAll(dst); rmErr != nil {
				return rollBack(fmt.Errorf("clear non-file at shard path %s: %w", base, rmErr))
			}
		}
		if err := os.Rename(f, dst); err != nil {
			return rollBack(fmt.Errorf("publish shard %s: %w", base, err))
		}
		published = append(published, base)
	}
	return published, true, nil
}

// liveAsidePrefix starts the name of the directory, in a build directory, that
// holds the live shards during a swap.
const liveAsidePrefix = "live-"

// recoverIncompleteSwap repairs a swap that left a .swapping marker (a crash,
// or a failed move back). The named family can be torn: some shards moved
// aside, some not renamed in. So it removes that family and the state files,
// which forces a full rebuild. It does nothing when no marker exists.
func (store corpusStore) recoverIncompleteSwap() {
	marker := readCacheFile(store.cacheDir, swappingMarkerFile)
	if marker == "" {
		return
	}
	torn, err := familyShardFiles(store.indexDir, shardFamilyFromLabel(marker))
	if err != nil {
		// Best-effort recovery: clearing state below still forces a rebuild.
		slog.Warn("Cannot enumerate torn shard family", "index_dir", store.indexDir, "error", err)
	}
	for _, f := range torn {
		_ = os.Remove(f)
	}
	removeFamilyManifest(store.indexDir)
	deleteStateFiles(store.cacheDir)
	removeCacheFile(store.cacheDir, swappingMarkerFile)
}

// removeCacheFile deletes a metadata file from cacheDir (best-effort).
func removeCacheFile(cacheDir, name string) {
	_ = os.Remove(filepath.Join(cacheDir, name))
}
