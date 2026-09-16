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
// a successful swap. A read-lock timeout can permit an unlocked read of the
// shards that remain.
// recoverIncompleteSwap uses .swapping to repair an interrupted or failed swap
// on the next build.

// shardFamily selects the shards for a swap or scan. A committed rebuild keeps
// the uncommitted family. A folder or scoped fallback build replaces all shards.
type shardFamily int

const (
	familyAll       shardFamily = iota // all shards and sidecars
	familyCommitted                    // all entries except uncommitted shards
)

const (
	// shardSuffix and metaSuffix name the two file kinds a shard family owns.
	// zoekt pairs a shard with its .meta sidecar (tombstones); they must always
	// move and be deleted together. An orphan shard can make deleted documents
	// searchable again.
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

// isUncommittedShard reports whether base is a zoekt uncommitted shard
// (<name>_v<format>.<n>.zoekt). The "_v" guards against a repo literally named
// "uncommitted".
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

// familyHasShard checks for a shard without loading the size data needed for
// manifest validation.
func familyHasShard(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read shard dir %s: %w", dir, err)
	}
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), shardSuffix) {
			return true, nil
		}
	}
	return false, nil
}

// familyShardNames lists the .zoekt entries that a search can try to open. It
// does not load size metadata.
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
// family, so a caller deciding whether a corpus is usable does not have to
// special-case a corpus that was never built or has just been evicted.
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

// hasShard reports whether any *.zoekt survives, in any family.
func (s familyScan) hasShard() bool {
	for _, m := range s.members {
		if m.shard {
			return true
		}
	}
	return false
}

// hasMember reports whether the family has a shard or sidecar.
func (s familyScan) hasMember(fam shardFamily) bool {
	for _, m := range s.members {
		if inFamily(fam, m) {
			return true
		}
	}
	return false
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

// paths returns the family's shards and sidecars. Committed repository names
// can vary through local [zoekt] config, so the committed family is all entries
// outside the fixed uncommitted family.
func (s familyScan) paths(fam shardFamily) []string {
	out := make([]string, 0, len(s.members))
	for _, m := range s.members {
		if inFamily(fam, m) {
			out = append(out, filepath.Join(s.dir, m.name))
		}
	}
	return out
}

// hasCommittedShard reports whether the committed family has a shard. Sidecars
// alone do not make a family present.
func (s familyScan) hasCommittedShard() bool {
	for _, m := range s.members {
		if m.shard && !m.uncommitted {
			return true
		}
	}
	return false
}

// contiguous reports whether each selected shard prefix is numbered 0..n-1
// with no gap.
//
// Zoekt derives the next shard number from the contiguous run that it finds. A
// delta based on a gapped family can replace a later shard. This check controls
// whether the writer can seed a delta build; it does not detect a missing
// trailing suffix.
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
// search can use: at least one shard, and exactly the set the last publish
// recorded. Presence alone accepts one survivor of a torn family, so the two
// checks always travel together.
func (s familyScan) familyComplete(indexDir string) bool {
	return s.hasShard() && s.matchesManifest(indexDir)
}

// familyManifestFile records the name and size of each published shard and
// sidecar. It stays in indexDir so removing the index also removes its manifest.
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

// removeFamilyManifest drops the manifest alongside the shards it describes.
func removeFamilyManifest(indexDir string) {
	_ = os.Remove(filepath.Join(indexDir, familyManifestFile))
	_ = os.Remove(filepath.Join(indexDir, familyManifestFile+".tmp"))
}

// matchesManifest reports whether the scanned directory still holds exactly the
// files the last publish recorded, at exactly the sizes it recorded.
//
// A presence or numbering check cannot find a missing suffix. The manifest can
// detect this because it records every published member.
//
// Set equality, not a subset: an extra file is damage too, because a publish
// lists the live family before deleting it and can therefore leave an
// unrecorded file. A manifest entry that is now missing is damage even when it
// is a .meta sidecar. A stale sidecar can attach to a new shard with the same
// number and make deleted content searchable again.
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
// Membership is decided by the recorded name on the manifest side, which is the
// same rule scanFamily applies to the directory side, so the two walks stay in
// step.
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

// afterDeleteBeforeRenameHook lets tests pause publication after deletion and
// before rename. It is nil in production.
var afterDeleteBeforeRenameHook func()

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

// discardBuildDir removes an abandoned build dir (validate-before-publish bailout
// or error). Best-effort; the gc orphan sweep collects any leftover.
func discardBuildDir(buildDir string) {
	if buildDir == "" {
		return
	}
	_ = os.RemoveAll(buildDir)
}

// publishGeneration replaces a shard family and writes its state while holding
// the exclusive publish lock. On success, normal locked readers cannot
// interleave with the shard and state writes. This multi-file swap is not one
// filesystem transaction; .swapping records an interrupted or failed swap for
// repair on the next build. If GC removed the corpus during the build,
// publication returns errCorpusEvicted and discards the build.
func publishGeneration(ctx context.Context, cacheDir, indexDir, buildDir string, fam shardFamily, writeState func() error) error {
	pub, err := acquirePublishLock(ctx, cacheDir)
	if err != nil {
		return err // includes errCorpusEvicted
	}
	defer releaseLock(pub)

	// Corpus may have been evicted (renamed to trash) while the build ran
	// outside the publish lock, even though its build-lock fd survived.
	if _, err := os.Stat(indexDir); err != nil {
		if os.IsNotExist(err) {
			return errCorpusEvicted
		}
		return fmt.Errorf("stat index dir before swap: %w", err)
	}
	published, err := publishShardFamilyLocked(cacheDir, indexDir, buildDir, fam)
	if err != nil {
		return err
	}
	// A wholesale publish replaces the complete family.
	if err := writeFamilyManifest(indexDir, published); err != nil {
		return err
	}
	if writeState != nil {
		return writeState()
	}
	return nil
}

// publishShardFamilyLocked performs the delete-live-then-rename-new swap for one
// family. The caller must hold the publish lock and confirm that the corpus
// directory exists. The operations can fail after a partial replacement;
// .swapping then makes the next build repair the family.
func publishShardFamilyLocked(cacheDir, indexDir, buildDir string, fam shardFamily) ([]string, error) {
	// Mark the swap in progress so a crash mid-rename is repaired next build.
	if err := writeCacheFile(cacheDir, swappingMarkerFile, fam.label()); err != nil {
		return nil, fmt.Errorf("write swapping marker: %w", err)
	}
	// Enumerate both sides before touching anything: a read failure must abort
	// the swap, never delete the live family and rename a partial set in.
	live, err := familyShardFiles(indexDir, fam)
	if err != nil {
		return nil, err
	}
	incoming, err := familyShardFiles(buildDir, fam)
	if err != nil {
		return nil, err
	}
	for _, f := range live {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove live shard %s: %w", f, err)
		}
	}
	// Let tests pause while the exclusive publish lock protects a partial swap.
	if afterDeleteBeforeRenameHook != nil {
		afterDeleteBeforeRenameHook()
	}
	published := make([]string, 0, len(incoming))
	for _, f := range incoming {
		base := filepath.Base(f)
		dst := filepath.Join(indexDir, base)
		// A directory at this path is not in live, so the removal loop did not
		// clear it. Remove any non-regular replacement before Rename.
		if st, statErr := os.Lstat(dst); statErr == nil && !st.Mode().IsRegular() {
			if rmErr := os.RemoveAll(dst); rmErr != nil {
				return nil, fmt.Errorf("clear non-file at shard path %s: %w", base, rmErr)
			}
		}
		if err := os.Rename(f, dst); err != nil {
			return nil, fmt.Errorf("publish shard %s: %w", base, err)
		}
		published = append(published, base)
	}
	removeCacheFile(cacheDir, swappingMarkerFile)
	return published, nil
}

// recoverIncompleteSwap repairs a crash that left a .swapping marker: the named
// family may be torn (some shards deleted, some not renamed in), so clean that
// family and clear state to force a full rebuild. It returns when no marker
// exists.
func recoverIncompleteSwap(cacheDir, indexDir string) {
	marker := readCacheFile(cacheDir, swappingMarkerFile)
	if marker == "" {
		return
	}
	torn, err := familyShardFiles(indexDir, shardFamilyFromLabel(marker))
	if err != nil {
		// Best-effort recovery: clearing state below still forces a rebuild.
		slog.Warn("Cannot enumerate torn shard family", "index_dir", indexDir, "error", err)
	}
	for _, f := range torn {
		_ = os.Remove(f)
	}
	removeFamilyManifest(indexDir)
	deleteStateFiles(cacheDir)
	removeCacheFile(cacheDir, swappingMarkerFile)
}

// removeCacheFile deletes a metadata file from cacheDir (best-effort).
func removeCacheFile(cacheDir, name string) {
	_ = os.Remove(filepath.Join(cacheDir, name))
}
