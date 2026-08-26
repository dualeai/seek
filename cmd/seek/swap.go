package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Temp-swap publish core. A builder constructs a new shard generation in a temp
// dir (newBuildDir), validates HEAD/state, then atomically publishes it into the
// live index dir under the brief publish lock — via publishGeneration, or via
// publishShardFamilyLocked directly when a builder must hold the lock across
// multiple operations (see runIndexingWithCache: committed swap + in-place
// uncommitted build). Readers hold LOCK_SH across their whole glob+open, so they
// are excluded from the swap and never observe a torn shard set. A .swapping
// marker bridges a crash mid-swap: recoverIncompleteSwap, run at the start of
// every build, cleans the possibly torn family and forces a clean rebuild.

// shardFamily selects which shards a swap (or a family glob) operates on. A
// committed full rebuild swaps only the committed family so the live uncommitted
// shards survive; a wholesale build (over-cap fallback, folder) swaps all.
type shardFamily int

const (
	familyAll       shardFamily = iota // every *.zoekt(.meta): wholesale builds
	familyCommitted                    // all EXCEPT uncommitted_*: committed-only swap
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
// "uncommitted" — the single source of truth for this predicate.
func isUncommittedShard(base string) bool {
	return strings.HasPrefix(base, repoUncommitted+"_v")
}

// familyShardFiles returns the *.zoekt and paired *.zoekt.meta files in dir that
// belong to the family. zoekt pairs a shard with its .meta sidecar (tombstones);
// they must always move/delete together — an orphan .zoekt resurrects deleted
// docs. The committed repo name is not always predictable (origin-derived), so
// "committed" is defined as everything that is not the uncommitted family.
func familyShardFiles(dir string, fam shardFamily) []string {
	shards, _ := filepath.Glob(filepath.Join(dir, "*.zoekt"))
	metas, _ := filepath.Glob(filepath.Join(dir, "*.zoekt.meta"))
	all := append(shards, metas...)
	if fam == familyAll {
		return all
	}
	out := all[:0:0] // fresh backing array; don't alias/clobber all while filtering
	for _, path := range all {
		if isUncommittedShard(filepath.Base(path)) {
			continue
		}
		out = append(out, path)
	}
	return out
}

// seedFamily hardlinks the family's live shards (.zoekt + paired .meta) from
// indexDir into buildDir so a zoekt delta build has its prior-generation
// baseline present. Same filesystem → links are metadata-only and share inodes;
// zoekt never mutates an existing shard file in place (it adds new shards and
// rewrites .meta via temp+rename to a fresh inode), so the live shards are never
// disturbed. A cold corpus (no prior shards) seeds nothing → zoekt does a full
// build, which is correct.
func seedFamily(indexDir, buildDir string, fam shardFamily) error {
	for _, src := range familyShardFiles(indexDir, fam) {
		dst := filepath.Join(buildDir, filepath.Base(src))
		if err := os.Link(src, dst); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("seed shard %s: %w", filepath.Base(src), err)
		}
	}
	return nil
}

var buildDirCounter atomic.Uint64

// afterDeleteBeforeRenameHook is a test-only seam (nil in production) invoked
// inside publishShardFamilyLocked between deleting the live family and renaming
// the new shards in, to deterministically widen the torn window.
var afterDeleteBeforeRenameHook func()

// newBuildDir creates a unique temp build dir under indexDir (same filesystem →
// the publish rename is atomic; dot-prefixed → invisible to gc enumeration,
// shardsExist, and size walks).
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

// publishGeneration atomically publishes the family's freshly built shards from
// buildDir into the live indexDir AND writes the corpus state, both under one
// brief publish-lock hold. Readers (SH) are excluded for the whole critical
// section, so they observe the new shards and the new state together — never new
// shards with stale state (which an unlocked state write after the swap could
// expose: an aborted generation's shards under a stale-matching state label).
// writeState runs only after a successful swap. It is skipped when GC removes
// the corpus during a build outside the publish lock. The build is discarded.
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
	if err := publishShardFamilyLocked(cacheDir, indexDir, buildDir, fam); err != nil {
		return err
	}
	if writeState != nil {
		return writeState()
	}
	return nil
}

// publishShardFamilyLocked performs the delete-live-then-rename-new swap for one
// family. The CALLER must already hold the publish lock and have confirmed the
// corpus dir exists. It is exposed separately so a builder can hold the publish
// lock across multiple operations (e.g. committed swap + in-place uncommitted
// build) as one reader-excluded critical section.
func publishShardFamilyLocked(cacheDir, indexDir, buildDir string, fam shardFamily) error {
	// Mark the swap in progress so a crash mid-rename is repaired next build.
	if err := writeCacheFile(cacheDir, swappingMarkerFile, fam.label()); err != nil {
		return fmt.Errorf("write swapping marker: %w", err)
	}
	for _, f := range familyShardFiles(indexDir, fam) {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove live shard %s: %w", f, err)
		}
	}
	// Test seam: widen the torn window (live family deleted, replacements not yet
	// renamed in) so a test can prove a concurrent reader is excluded by the
	// publish EX lock and never observes it. nil in production.
	if afterDeleteBeforeRenameHook != nil {
		afterDeleteBeforeRenameHook()
	}
	for _, f := range familyShardFiles(buildDir, fam) {
		dst := filepath.Join(indexDir, filepath.Base(f))
		if err := os.Rename(f, dst); err != nil {
			return fmt.Errorf("publish shard %s: %w", filepath.Base(f), err)
		}
	}
	removeCacheFile(cacheDir, swappingMarkerFile)
	return nil
}

// recoverIncompleteSwap repairs a crash that left a .swapping marker: the named
// family may be torn (some shards deleted, some not renamed in), so clean that
// family and clear state to force a full rebuild. Cheap no-op when no marker.
func recoverIncompleteSwap(cacheDir, indexDir string) {
	marker := readCacheFile(cacheDir, swappingMarkerFile)
	if marker == "" {
		return
	}
	for _, f := range familyShardFiles(indexDir, shardFamilyFromLabel(marker)) {
		_ = os.Remove(f)
	}
	deleteStateFiles(cacheDir)
	removeCacheFile(cacheDir, swappingMarkerFile)
}

// removeCacheFile deletes a metadata file from cacheDir (best-effort).
func removeCacheFile(cacheDir, name string) {
	_ = os.Remove(filepath.Join(cacheDir, name))
}
