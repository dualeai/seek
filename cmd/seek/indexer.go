package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"golang.org/x/sys/unix"
)

const (
	// stateFile stores the hash of the last indexed corpus state.
	stateFile = ".state"
	// headFile stores the HEAD SHA of the last successful committed index.
	// It is a fallback only: the shards record the commit they hold, and
	// committedShardHead reads it. This file answers when that record cannot
	// be read, so a shard with unreadable metadata does not cause a rebuild
	// on every search.
	headFile = ".head"
	// emptyFile stores the state hash of a Git node that is known to have
	// no indexable shards. It tells an empty node apart from a missing or
	// damaged shard directory.
	emptyFile = ".empty"
	// lockFile is the publish/read lock. Readers hold a shared lock while they
	// list, open, and search shards. Publishers hold an exclusive lock while
	// they replace a shard family.
	lockFile = ".lock"
	// buildLockFile lets only one process build a corpus at a time. Readers do
	// not take this lock.
	buildLockFile = ".build.lock"
	// swappingMarkerFile names the shard family that publishShardFamilyLocked
	// swaps. The publish removes it when the swap ends or moves the live
	// family back. A crash during the swap, or a failed move back, leaves it.
	// Then the family can be partial, so the next build removes the family
	// and builds it in full (recoverIncompleteSwap).
	swappingMarkerFile = ".swapping"
	// buildDirPrefix names per-build temp index dirs (cacheDir/index/.build-*).
	// Maintenance scans ignore these dot-prefixed directories.
	// sweepOrphanBuildDirs removes old entries.
	buildDirPrefix = ".build-"
	// repoUncommitted is the zoekt repository name for uncommitted file shards.
	repoUncommitted = "uncommitted"
	// stateVersion is the prefix used in state hashing to invalidate previous
	// state formats when the hash algorithm or input format changes.
	stateVersion = "v6\x00"
)

// shardMax is the maximum corpus size (in bytes) per zoekt shard.
// Smaller shards let Zoekt build more shards concurrently. It is a variable so
// tests can swap it; production callers treat it as read-only.
var shardMax = 10 * 1024 * 1024 // 10 MiB

// largeDocumentParallelism bounds the shard and ctags jobs of one build for
// documents that fill a shard of their own (isolatesShard). It is one per CPU,
// because Seek's objective is the shortest time. The floor is 3, the fixed
// count that Seek used before it sized this from the host. So a host with one
// or two CPUs runs no fewer jobs than before. Large windows also rotate on
// their real content bytes, which bounds memory for documents above the
// read-weight clamp. Seek does not reduce index quality for these files: it
// still indexes full content and runs symbol analysis.
var largeDocumentParallelism = max(3, runtime.GOMAXPROCS(0))

// computeStateHash computes the xxHash64 of the given state string.
// In production, the input is a repoStateFingerprint (raw git status output
// enriched with file stats).
func computeStateHash(rawOutput string) string {
	h := xxhash.New()
	_, _ = h.WriteString(stateVersion)
	_, _ = h.WriteString(rawOutput)
	return formatHex16(h.Sum64())
}

// formatHex16 formats a uint64 as a zero-padded 16-character hex string
// without the fmt.Sprintf allocation.
func formatHex16(v uint64) string {
	const digits = "0123456789abcdef"
	var buf [16]byte
	for i := 15; i >= 0; i-- {
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

// repoStateFingerprint returns raw Git status output and the stat fields of
// each dirty file: path, mtime, size, inode, mode, and ctime. The folder walk
// keeps the same fields. Git status porcelain does not include working-tree
// content hashes, so these fields detect common changes to files that are
// already dirty. This is a metadata fingerprint, not a content hash; an edit
// that keeps every observed field can keep the same fingerprint.
//
// A node capture calls it once, and the build publishes that state. A dirty
// file that changes after the capture changes the fingerprint of the next
// capture, so the next search builds again.
func repoStateFingerprint(repoDir string, state repoState) string {
	if len(state.Files) == 0 {
		return state.RawOutput
	}
	var b strings.Builder
	b.Grow(len(state.RawOutput) + len(state.Files)*80)
	b.WriteString(state.RawOutput)

	// Pre-build path prefix to avoid per-file filepath.Join allocation.
	// Git status paths are clean relative paths (no double slashes or dots),
	// so simple concatenation is safe.
	pathPrefix := repoDir + "/"

	// Scratch buffer for numeric formatting (avoids strconv.Format* allocs).
	var numBuf [20]byte

	for _, f := range state.Files {
		var stat syscall.Stat_t
		if err := syscall.Lstat(pathPrefix+f, &stat); err != nil {
			// File may have been deleted between git status and stat;
			// include a sentinel so deletions also change the hash.
			b.WriteByte(0)
			b.WriteString(f)
			b.WriteString("\x00deleted\x00")
			continue
		}
		mtime := statMtimeNano(stat)
		b.WriteByte(0)
		b.WriteString(f)
		b.WriteByte(0)
		b.Write(strconv.AppendInt(numBuf[:0], mtime, 10))
		b.WriteByte(0)
		b.Write(strconv.AppendInt(numBuf[:0], stat.Size, 10))
		b.WriteByte(0)
		b.Write(strconv.AppendUint(numBuf[:0], stat.Ino, 10))
		b.WriteByte(0)
		// A chmod changes the mode and the ctime only: a file read by name
		// only for its permissions is read again once they allow it.
		b.Write(strconv.AppendUint(numBuf[:0], uint64(stat.Mode), 10))
		b.WriteByte(0)
		b.Write(strconv.AppendInt(numBuf[:0], statCtimeNano(stat), 10))
		b.WriteByte(0)
	}
	return b.String()
}

// readCacheFile reads small cached text and removes outer white space.
func readCacheFile(cacheDir, name string) string {
	data, err := os.ReadFile(filepath.Join(cacheDir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeCacheFile atomically writes value to cacheDir/name via tmp+rename.
// Two writers of one name share the tmp file, so a record with content is
// written only under the corpus's build or publish lock. The empty markers
// .used and .last-gc also use it with no corpus lock. A lost race fails one
// rename, and the marker records only its time.
func writeCacheFile(cacheDir, name, value string) error {
	tmpPath := filepath.Join(cacheDir, name+".tmp")
	if err := os.WriteFile(tmpPath, []byte(value), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(cacheDir, name))
}

// sharedTmpInfix marks the temp files of writeSharedCacheFile; the gc
// orphan sweep removes old ones.
const sharedTmpInfix = ".tmp-"

// writeSharedCacheFile writes value to cacheDir/name through a temp file of
// its own and a rename. Writers with no exclusive lock use it (.bm25-v2,
// .size): each concurrent writer renames a complete file. The file has the
// mode of os.CreateTemp (0600): only the cache owner reads it.
func writeSharedCacheFile(cacheDir, name, value string) error {
	tmp, err := os.CreateTemp(cacheDir, name+sharedTmpInfix+"*")
	if err != nil {
		return err
	}
	_, writeErr := tmp.WriteString(value)
	closeErr := tmp.Close()
	err = errors.Join(writeErr, closeErr)
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(cacheDir, name))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// readStateFile reads the cached state hash.
func readStateFile(cacheDir string) string { return readCacheFile(cacheDir, stateFile) }

// writeStateFile atomically writes the state hash.
func writeStateFile(cacheDir, state string) error { return writeCacheFile(cacheDir, stateFile, state) }

// readHeadFile reads the last indexed HEAD SHA.
func readHeadFile(cacheDir string) string { return readCacheFile(cacheDir, headFile) }

// writeHeadFile atomically writes the HEAD SHA.
func writeHeadFile(cacheDir, sha string) error { return writeCacheFile(cacheDir, headFile, sha) }

func readEmptyStateFile(cacheDir string) string { return readCacheFile(cacheDir, emptyFile) }

func writeEmptyStateFile(cacheDir, state string) error {
	return writeCacheFile(cacheDir, emptyFile, state)
}

func deleteEmptyStateFiles(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, emptyFile))
	_ = os.Remove(filepath.Join(cacheDir, emptyFile+".tmp"))
}

// deleteStateFiles removes the cached working-tree state and its tmp files, so
// a failed indexing cycle re-checks the corpus on the next search.
//
// It keeps .git-committed-v1. That file describes the committed family still on
// disk: a failed build did not change that family, or recorded it right after
// its publish. Without the file, the next commit rebuilds in full. It clears
// .head, which is only a fallback.
func deleteStateFiles(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, stateFile))
	_ = os.Remove(filepath.Join(cacheDir, stateFile+".tmp"))
	_ = os.Remove(filepath.Join(cacheDir, headFile))
	_ = os.Remove(filepath.Join(cacheDir, headFile+".tmp"))
	deleteEmptyStateFiles(cacheDir)
	// A stale .git-committed-v1 cannot give a wrong answer.
	// prepareNativeGitDelta compares it with the commit that the shard
	// records and rejects a delta when the two differ.
}

// indexParallelism returns the number of parallel indexing workers.
func indexParallelism() int {
	return runtime.GOMAXPROCS(0)
}

// indexBuildOptions gives the Zoekt build options of every shard build.
func indexBuildOptions(indexDir string, parallelism int) index.Options {
	// Keep the same content and ctags contract for every accepted document.
	// Large-file controls can change only Builder scheduling and parallelism.
	//
	// Zoekt replaces the content of two kinds of documents with a NOT-INDEXED
	// marker. They are content with a NUL byte (index.SkipReasonBinary) and
	// content shorter than three bytes (index.SkipReasonTooSmall).
	// index.Options has no switch for either. Seek accepts both limits; only a
	// Zoekt change can remove them. The readers find a NUL in the first
	// binaryProbeBytes themselves and do not read the rest (readContent).
	//
	// TrigramMax at the content limit turns off Zoekt's distinct-trigram skip
	// (default 20000). Zoekt counts only when len(content)-2 is above
	// TrigramMax, so the count never decides whether content is searchable. The
	// cost is index size and build memory for large files with many distinct
	// non-ASCII trigrams.
	//
	// TODO(https://github.com/sourcegraph/zoekt/issues/1204): Zoekt skips a
	// file whose ctags parse takes more than 1 minute, also with
	// CTagsMustSucceed, so that file has no symbols. Set a longer timeout once
	// Zoekt lets callers set it.
	return index.Options{
		IndexDir:         indexDir,
		SizeMax:          int(documentContentLimit),
		TrigramMax:       int(documentContentLimit),
		Parallelism:      parallelism,
		CTagsMustSucceed: true,
		ShardMax:         shardMax,
	}
}

// ctagsOnce caches executable discovery for one process.
var (
	ctagsOnce sync.Once
	ctagsErr  error
)

// ctagsUnavailableError identifies a failure to locate an executable Ctags
// command. An explicit command records its CTAGS_COMMAND value. The wrapped
// cause keeps lookup details for verbose logs.
type ctagsUnavailableError struct {
	command string
	cause   error
}

func (e *ctagsUnavailableError) Error() string {
	if e.command != "" {
		return fmt.Sprintf("cannot use CTAGS_COMMAND=%q: %v", e.command, e.cause)
	}
	return fmt.Sprintf("universal-ctags executable not found: %v", e.cause)
}

func (e *ctagsUnavailableError) Unwrap() error {
	return e.cause
}

// checkCtagsCached returns the cached result of checkCtags, running the
// check at most once per process.
func checkCtagsCached() error {
	ctagsOnce.Do(func() { ctagsErr = checkCtags() })
	return ctagsErr
}

// checkCtags locates an executable command before Zoekt creates a builder. It
// verifies the generic "ctags" fallback by version because that name can refer
// to other implementations. Zoekt validates required Universal Ctags features
// when CTagsMustSucceed is set.
//
// Detection order:
//  1. CTAGS_COMMAND env var (explicit user override)
//  2. "universal-ctags" binary on PATH (zoekt default)
//  3. "ctags" binary on PATH, verified via --version (Homebrew on macOS
//     installs universal-ctags as "ctags")
func checkCtags() error {
	// 1. An explicit environment variable: trust the user.
	if cmd := os.Getenv("CTAGS_COMMAND"); cmd != "" {
		if _, err := exec.LookPath(cmd); err != nil {
			return &ctagsUnavailableError{command: cmd, cause: err}
		}
		return nil
	}

	// 2. Zoekt default: looks for "universal-ctags" on PATH.
	if _, err := exec.LookPath("universal-ctags"); err == nil {
		return nil
	} else {
		universalErr := err

		// 3. Fallback: Homebrew installs universal-ctags as "ctags".
		// Verify via --version to distinguish it from Exuberant Ctags.
		ctags, lookupErr := exec.LookPath("ctags")
		if lookupErr != nil {
			return &ctagsUnavailableError{cause: errors.Join(universalErr, lookupErr)}
		}
		out, versionErr := exec.Command(ctags, "--version").Output()
		if versionErr == nil && strings.Contains(string(out), "Universal Ctags") {
			_ = os.Setenv("CTAGS_COMMAND", ctags)
			return nil
		}
		if errors.Is(versionErr, syscall.EMFILE) || errors.Is(versionErr, syscall.ENFILE) {
			// No free descriptor says nothing about ctags.
			return fmt.Errorf("check %q --version: %w", ctags, versionErr)
		}
		if versionErr != nil {
			versionErr = fmt.Errorf("check %q --version: %w", ctags, versionErr)
		} else {
			versionErr = fmt.Errorf("%q is not Universal Ctags", ctags)
		}
		return &ctagsUnavailableError{cause: errors.Join(universalErr, versionErr)}
	}
}

// build makes the joined Git index of store current with
// separate build and publish locks. Zoekt covers committed and uncommitted
// files. The semantic base generation covers the captured commit, and the
// overlay generation covers the dirty files:
//
//   - .build.lock serializes builders for the complete build.
//   - The build makes the committed family outside the publish lock. Native
//     full uses an empty temporary directory; eligible native delta seeds
//     current shards. These are the only committed-data routes. The build
//     checks HEAD before the publish.
//   - A required semantic generation builds beside the committed Zoekt branch.
//     Each branch reads its own bounded stream from the same captured commit.
//   - A full uncommitted rebuild builds in its own directory before the publish
//     lock, and the lock covers only its swap. An uncommitted delta writes in
//     place under the lock, only on a family that matches its manifest. The
//     shards, manifest, and state describe the same generation.
//   - The build writes the joined descriptor last, under the publish lock. A
//     semantic failure keeps the Zoekt generation searchable without joined
//     retrieval.
//
// Readers hold the shared publish lock while they list, open, and search
// shards, so the text and vector indexes name the same generation. A reader
// waits for a publish with no time limit. A failed swap moves the live family
// back; only an interrupted swap, or a failed move back, leaves shards for the
// next build to repair.
//
// peer reports a build that a peer runs: the node serves its current shards,
// and the peer publishes the fresh ones.
func (g *gitNode) build(
	ctx context.Context,
	store corpusStore,
	state repoState,
	preState string,
	execution searchExecution,
) (peer bool, err error) {
	unlock, acquired, err := store.lockBuild(ctx)
	if err != nil {
		return false, err
	}
	if !acquired {
		return true, nil
	}
	defer unlock()
	return false, g.buildLocked(ctx, store, state, preState, execution)
}

// buildLocked is the build under the build lock.
func (g *gitNode) buildLocked(
	ctx context.Context,
	store corpusStore,
	state repoState,
	preState string,
	execution searchExecution,
) error {
	paths := g.paths
	repoDir := paths.RepoDir
	cacheDir, indexDir := store.cacheDir, store.indexDir

	// Check state and shard integrity again after acquiring the build lock. A
	// peer can publish between the first check and this point.
	cachedState := readStateFile(cacheDir)
	// One scan supplies the post-lock check, the seed decision, and the
	// committed trigger.
	sc, scErr := scanFamilyOptional(indexDir)
	if scErr != nil {
		return scErr
	}
	semanticSource := state.HeadSHA
	hasDirty := len(state.Files) > 0
	in := gitBuildInputs{
		head:         state.HeadSHA,
		preState:     preState,
		cachedState:  cachedState,
		dirty:        hasDirty,
		familyIntact: sc.matchesManifest(indexDir),
		// The committed family is checked separately: a dirty edit can
		// change only the uncommitted family.
		committedIntact: sc.matchesManifestFamily(indexDir, familyCommitted),
		committedMember: sc.hasMember(familyCommitted),
		committedReady:  store.committedSnapshotReady(state, sc),
		// Prepare vectors only when a route can read them. The model itself
		// stays available for re-ranking lexical candidates. buildsVectors
		// documents which searches this covers.
		vectors: execution.buildsVectors(state.HeadSHA),
		// A dirty node also binds the overlay of its dirty files.
		overlay:    execution.buildsOverlay(hasDirty),
		semanticOn: execution.policy == semanticSearch,
	}
	baseSource, overlaySource := "", ""
	if in.vectors {
		baseSource = semanticSource
	}
	if in.overlay {
		overlaySource = semanticOverlaySource(preState)
	}
	in.joinedReady = (!in.vectors && !in.overlay) || store.joinedGenerationsMatch(
		preState,
		baseSource,
		overlaySource,
	)
	// A blob that a build read as missing can arrive with no change to the
	// state.
	storeFingerprint := buildObjectStoreFingerprint(ctx, repoDir, cacheDir)
	arrival, err := recheckMissingBlobs(ctx, repoDir, cacheDir, state.HeadSHA, in.vectors, storeFingerprint)
	if err != nil {
		return gitCorpusError(repoDir, indexDir, err)
	}
	in.arrival = arrival
	if gitBuildCurrent(in) {
		return nil
	}

	parallelism := execution.resources.cpuLimit()

	// publishedCommittedHead asks the shards rather than the sidecar;
	// committedShardHead documents why.
	if in.head != "no-head" {
		in.publishedHead = publishedCommittedHead(cacheDir, sc)
	}
	if in.vectors {
		in.basePresent = semanticGenerationPresent(indexDir, semanticSource)
	}
	if in.overlay {
		in.overlayPresent = semanticGenerationPresent(indexDir, overlaySource)
	}
	build := decideGitBuild(in)

	// Build Zoekt and semantic data together outside the publish lock. Each
	// branch reads the same captured commit through its own bounded Git stream.
	var buildDir string
	var nextCommittedState committedGitState
	if build.buildDir() {
		buildDir, err = newBuildDir(indexDir)
		if err != nil {
			return err
		}
		defer discardBuildDir(buildDir)
	}
	var snapshot gitObjectID
	var lexicalMissing, semanticMissing []missingBlob
	if build.snapshot() {
		snapshot, err = captureGitSnapshot(state.HeadSHA)
		if err != nil {
			deleteStateFiles(cacheDir)
			return gitCorpusError(repoDir, indexDir, err)
		}
	}
	var lexicalErr error
	var semanticErr error
	semanticBuilt := false
	semanticStaging := filepath.Join(buildDir, "semantic")
	overlayStaging := filepath.Join(buildDir, "overlay")
	overlayBuilt := false
	var lexicalBuild func()
	if build.committed {
		lexicalBuild = func() {
			nextCommittedState, lexicalMissing, lexicalErr = buildCommittedGitStage(
				ctx,
				repoDir,
				store,
				buildDir,
				snapshot,
				sc,
				parallelism,
				arrival,
			)
		}
	}
	var semanticBuild func()
	if build.semantic || build.overlay {
		semanticBuild = func() {
			// Linked worktrees of this node find its generations by lineage.
			lineage := semanticGitLineage(paths)
			_ = writeCacheFile(cacheDir, semanticLineageFile, lineage)
			semanticErr = runSemanticBuild(ctx, execution.model, func(embedder semanticModel) error {
				if build.semantic {
					if err := buildSemanticGitGeneration(
						ctx,
						repoDir,
						snapshot,
						indexDir,
						lineage,
						semanticStaging,
						semanticSource,
						embedder,
						execution.resources,
						&semanticMissing,
					); err != nil {
						return err
					}
					semanticBuilt = true
				}
				if build.overlay {
					// The staged base generation, when this build made one.
					base := ""
					if semanticBuilt {
						base = semanticStaging
					}
					if err := buildSemanticOverlayGeneration(
						ctx,
						repoDir,
						indexDir,
						lineage,
						overlayStaging,
						overlaySource,
						state.Files,
						embedder,
						execution.resources,
						base,
					); err != nil {
						return err
					}
					overlayBuilt = true
				}
				return nil
			})
		}
	}
	runJoinedIndexBuilds(lexicalBuild, semanticBuild)
	if buildErr := cmp.Or(lexicalErr, semanticErr); buildErr != nil && ctx.Err() == nil &&
		capturedCommitMissing(ctx, repoDir, build, snapshot) {
		// The commit disappeared after git status captured it: no build can
		// read it, so the next search must build again.
		deleteStateFiles(cacheDir)
		return gitCorpusError(repoDir, indexDir, fmt.Errorf("captured Git commit %s is missing: %w", snapshot, buildErr))
	}
	if lexicalErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !shardsExist(indexDir) {
			deleteStateFiles(cacheDir)
		}
		// The caller decides whether existing shards make a stale search safe.
		return gitCorpusError(repoDir, indexDir, lexicalErr)
	}
	if semanticErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		semanticBuildFailed(execution.model, "Semantic index build failed; keeping lexical search", semanticErr)
	}
	// HEAD must still equal the captured value before publication. This check
	// also covers an unborn snapshot that had no committed Builder work.
	publish, err := validateCommittedBuildHead(ctx, paths, state.HeadSHA)
	if err != nil {
		return err
	}
	if !publish {
		return nil // discard build; live shards untouched
	}

	// prepareUncommitted gives a delta or a prebuilt stage, or prebuildErr.
	// prebuildErr is an EMFILE or ENFILE of the prebuild. Then the committed
	// family and the vectors still publish, the live uncommitted family
	// stays, and no state is written.
	var prebuilt *uncommittedStage
	var prebuildErr error
	var delta *uncommittedDelta
	if hasDirty {
		delta, prebuilt, prebuildErr, err = prepareUncommitted(ctx, repoDir, store, cachedState, in.familyIntact, state, parallelism)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return gitCorpusError(repoDir, indexDir, err)
		}
		if prebuilt != nil {
			defer prebuilt.discard()
		}
	}

	// Keep the committed publish, the uncommitted swap or delta, and state
	// files under one exclusive publish lock. Normal locked readers cannot
	// interleave with these writes. The state and HEAD writes below must
	// remain inside this lock.
	var publishedNames []string
	publishedCommitted := false
	// afterLock runs after the publish lock is released: the removal of the
	// files that a swap moved aside, so readers do not wait for it.
	var afterLock func()
	defer func() {
		if afterLock != nil {
			afterLock()
		}
	}()

	pub, err := acquirePublishLock(ctx, cacheDir)
	if err != nil {
		if errors.Is(err, errCorpusEvicted) {
			return nil // corpus gc-evicted during build; next search rebuilds
		}
		return err
	}
	defer releaseLock(pub)
	if _, sErr := os.Stat(indexDir); sErr != nil {
		return nil // evicted; discard
	}
	if build.dropJoined {
		// The old descriptor must not name a family while that family changes.
		removeJoinedGeneration(cacheDir)
	}

	if build.committed {
		if err := writePendingMissingBlobRecord(cacheDir, lexicalMissingBlobsFile, state.HeadSHA, lexicalMissing); err != nil {
			return fmt.Errorf("write missing-blob record: %w", err)
		}
	}
	// recordFamily writes the family manifest and, once, the committed
	// records. The manifest names the committed shards of the swap and, with
	// uncommitted, the uncommitted files that the directory holds. It runs
	// right after the committed publish, and again at the end. So a failed
	// uncommitted step keeps records that name the published committed
	// family. The manifest does not name an uncommitted family that this
	// build did not write and that did not match the manifest at the start.
	// So the next build sees the damage.
	committedRecorded := false
	recordFamily := func(uncommitted bool) (bool, error) {
		names := append([]string(nil), publishedNames...)
		// This process changed the directory, so scan it again.
		post, postErr := scanFamily(indexDir)
		if postErr != nil {
			return false, postErr
		}
		for _, m := range post.members {
			if m.uncommitted && uncommitted || !m.uncommitted && !publishedCommitted {
				names = append(names, m.name)
			}
		}
		if err := writeFamilyManifest(indexDir, names); err != nil {
			return false, fmt.Errorf("write family manifest: %w", err)
		}
		if committedRecorded {
			return post.hasShard(familyAll), nil
		}
		committedRecorded = true
		if build.committed {
			if err := writeCommittedGitState(cacheDir, nextCommittedState); err != nil {
				return false, fmt.Errorf("write committed Git state: %w", err)
			}
			if err := writeMissingBlobRecord(cacheDir, lexicalMissingBlobsFile, missingBlobRecord{
				head:        state.HeadSHA,
				fingerprint: storeFingerprint,
				blobs:       lexicalMissing,
			}); err != nil {
				return false, fmt.Errorf("write missing-blob record: %w", err)
			}
		} else if build.clearCommitted {
			deleteCommittedGitState(cacheDir)
			removeCacheFile(cacheDir, lexicalMissingBlobsFile)
		}
		return post.hasShard(familyAll), nil
	}
	if build.committed || build.clearCommitted {
		committedNames, pErr := store.publishShardFamilyLocked(buildDir, familyCommitted)
		if pErr != nil {
			return fmt.Errorf("publish committed shards: %w", pErr)
		}
		publishedNames = committedNames
		publishedCommitted = true
		if _, err := recordFamily(in.familyIntact); err != nil {
			deleteStateFiles(cacheDir)
			return err
		}
	}

	// partial is the error of a build that kept dirty files by name, or
	// whose prebuild hit EMFILE or ENFILE. The publish goes on, and only the
	// state and the joined descriptor stay unwritten.
	var partial error
	if hasDirty && prebuildErr != nil {
		partial = prebuildErr
	} else if hasDirty {
		uncommittedErr := gitCorpusError(
			repoDir,
			indexDir,
			indexUncommitted(ctx, repoDir, store, state, preState, parallelism, prebuilt, delta),
		)
		if found, ok := errors.AsType[*dirtyFilesKept](uncommittedErr); ok {
			partial, uncommittedErr = found, nil
		}
		if uncommittedErr != nil {
			deleteStateFiles(cacheDir)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return uncommittedErr
		}
	} else {
		discard, err := cleanUncommittedShards(indexDir)
		afterLock = discard
		if err != nil {
			deleteStateFiles(cacheDir)
			return err
		}
		deleteUncommittedManifest(cacheDir)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		deleteStateFiles(cacheDir)
		return ctxErr
	}

	// The build publishes the captured state: a command runs one git status
	// per node. A file that changes during the build changes the state that
	// the next search captures, because the hash stats the dirty files when
	// it runs. So that search builds again, and a change during the build
	// never stays hidden behind a recorded state.
	// Write the manifest under the same publish lock as the state, with the
	// uncommitted files that this process just wrote. Record dirty-to-clean
	// transitions too.
	// A prebuild that hit EMFILE or ENFILE leaves the live uncommitted family
	// as it was. So the record names that family only when it matched at the
	// start.
	finalHasShard, err := recordFamily(prebuildErr == nil || in.familyIntact)
	if err != nil {
		deleteStateFiles(cacheDir)
		return err
	}
	if partial != nil {
		// No state: the next search builds again and reads the kept files.
		_ = os.Remove(filepath.Join(cacheDir, stateFile))
		deleteEmptyStateFiles(cacheDir)
	} else if err := writeStateFile(cacheDir, preState); err != nil {
		deleteStateFiles(cacheDir)
		return fmt.Errorf("write state file: %w", err)
	} else if finalHasShard {
		deleteEmptyStateFiles(cacheDir)
	} else if err := writeEmptyStateFile(cacheDir, preState); err != nil {
		deleteStateFiles(cacheDir)
		return fmt.Errorf("write empty state file: %w", err)
	}
	// Persist HEAD so working-tree-only changes skip the committed indexer.
	if err := writeHeadFile(cacheDir, state.HeadSHA); err != nil {
		deleteStateFiles(cacheDir)
		slog.Warn("Failed to write head file", "error", err)
	}
	if in.vectors || in.overlay {
		var activationErr error
		baseReady := build.baseBound
		if semanticBuilt {
			// The pending record comes first, so a generation never exists
			// without a record that names its missing blobs.
			activationErr = writePendingMissingBlobRecord(cacheDir, semanticMissingBlobsFile, semanticSource, semanticMissing)
			if activationErr == nil {
				activationErr = publishSemanticGeneration(indexDir, semanticStaging, semanticSource, arrival.semantic)
			}
			if activationErr == nil {
				activationErr = writeMissingBlobRecord(cacheDir, semanticMissingBlobsFile, missingBlobRecord{
					head:        semanticSource,
					fingerprint: storeFingerprint,
					blobs:       semanticMissing,
				})
			}
			baseReady = activationErr == nil
		}
		overlayReady := build.overlayBound
		if activationErr == nil && overlayBuilt {
			activationErr = publishSemanticGeneration(indexDir, overlayStaging, overlaySource, false)
			overlayReady = activationErr == nil
		}
		// Both parts or neither: a node never reads its base without the
		// overlay that its dirty files need. With kept files no state binds.
		if activationErr == nil && baseReady && overlayReady && partial == nil {
			activationErr = store.bindSemanticGenerations(preState, baseSource, overlaySource)
		}
		if activationErr != nil {
			removeJoinedGeneration(cacheDir)
			slog.Debug(
				"Semantic index activation failed; keeping lexical search",
				"provider", semanticProviderName(),
				"error", activationErr,
			)
		}
	}
	if partial != nil {
		return gitCorpusError(repoDir, indexDir, partial)
	}
	return nil
}

// buildCommittedGitStage builds the committed family of snapshot in buildDir:
// a native delta onto the published family when one is eligible, else a full
// build. It returns the next committed state and the blobs that the build read
// as missing.
func buildCommittedGitStage(
	ctx context.Context,
	repoDir string,
	store corpusStore,
	buildDir string,
	snapshot gitObjectID,
	scan familyScan,
	parallelism int,
	arrival missingBlobArrival,
) (committedGitState, []missingBlob, error) {
	// A delta reads only the changed paths, so a blob that arrived for an
	// unchanged path needs the full build.
	if !arrival.lexical {
		delta, eligible, err := prepareNativeGitDelta(
			ctx,
			repoDir,
			store,
			snapshot,
			scan,
		)
		if err != nil {
			return committedGitState{}, nil, err
		}
		if eligible {
			err := indexNativeGitDelta(
				ctx,
				repoDir,
				buildDir,
				scan.paths(familyCommitted),
				delta,
				parallelism,
			)
			switch {
			case err == nil:
				// The seed keeps the name-only files of the base paths that
				// the delta did not change, so the record keeps their blobs.
				return delta.nextState, deltaMissingBlobs(arrival.lexicalCarry, delta.changedPaths, delta.targetInfos), nil
			case !errors.Is(err, errNativeDeltaFallback):
				// A cancel, or a Git read error that a full build reads again.
				return committedGitState{}, nil, err
			}
			// The delta's seed links or its Builder failed: build the family
			// in full, from an empty build directory.
			slog.Debug("Native Git delta build failed; full rebuild", "error", err)
			cleanRepositoryShards(buildDir, delta.repository.Name)
		}
	}
	var missing []missingBlob
	liveBytes, err := indexNativeGitFull(ctx, repoDir, buildDir, snapshot, parallelism, &missing)
	if err != nil {
		return committedGitState{}, nil, err
	}
	buildScan, err := scanFamily(buildDir)
	if err != nil {
		return committedGitState{}, nil, err
	}
	return committedGitState{
		head:       snapshot,
		baseHead:   snapshot,
		baseShards: nativeCommittedShardCount(buildScan),
		liveBytes:  liveBytes,
	}, missing, nil
}

// publishedCommittedHead returns the commit the published committed family
// holds, as the shards themselves record it. See committedShardHead for why the
// shards and not the sidecar answer this.
//
// It falls back to the .head sidecar when no shard can answer. That covers a
// corpus that has not published yet and a shard whose metadata cannot be read.
func publishedCommittedHead(cacheDir string, scan familyScan) string {
	if head, known := committedShardHead(scan); known {
		return head
	}
	return readHeadFile(cacheDir)
}

// committedSnapshotReady reports whether scan has enough committed state for
// the caller's separate cache-state and full-manifest checks. A valid manifest
// can describe an empty committed family beside live dirty shards, so a
// committed shard is not always required.
//
// When a committed shard is present, the answer comes from the commit that
// shard records, not from the .head sidecar. See committedShardHead.
func (store corpusStore) committedSnapshotReady(state repoState, scan familyScan) bool {
	if state.HeadSHA == "no-head" {
		return !scan.hasMember(familyCommitted)
	}
	if scan.hasShard(familyCommitted) {
		// Presence alone says nothing about which commit the shards hold.
		if shardHead, known := committedShardHead(scan); known {
			return shardHead == state.HeadSHA
		}
		// Unknown, so keep the answer this corpus already gives. Reporting
		// "stale" here would rebuild on every read of an unreadable shard.
		return true
	}
	return readHeadFile(store.cacheDir) == state.HeadSHA && scan.matchesManifestFamily(store.indexDir, familyCommitted)
}

// validateCommittedBuildHead distinguishes a real HEAD change from a failed
// validation. A changed HEAD discards the new build and keeps the live index.
// It returns a cancellation and other Git failures to the caller.
func validateCommittedBuildHead(
	ctx context.Context,
	paths gitPaths,
	expectedTreeish string,
) (bool, error) {
	err := validateCommittedHead(ctx, paths, expectedTreeish)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errGitIndexStateChanged):
		slog.Warn("HEAD moved during committed index; will re-index on next search")
		return false, nil
	default:
		return false, err
	}
}

// shardsExist checks if any *.zoekt shard files exist in the index directory.
// It asks only for presence, so it uses the scan that skips the per-entry
// lstat: neither a size nor a sidecar pairing decides this answer.
func shardsExist(indexDir string) bool {
	names, err := familyShardNames(indexDir)
	return err == nil && len(names) > 0
}

// maxCommittedDeltaShards is the pre-admission threshold for shards added after
// the last committed full build. This shard check permits a delta when the
// existing added count equals this value, so that delta can take the family
// above the threshold. The next update selects a native full build. This value
// does not limit the full base shard count. It matches maxFolderDeltaShards.
const maxCommittedDeltaShards = 64

// fallbackGitRepositoryName returns the opaque name of the committed Git
// repository and the prefix of its shards. It is not based on basename:
// "uncommitted" is reserved for dirty-file shards.
func fallbackGitRepositoryName(repoDir string) string {
	return "git-" + hashParts("git_repository_name", canonicalCorpusPath(repoDir))
}

// fileContent carries one file's content from reader to consumer.
//
// weight is the readSemaphore weight reserved for the document: readWeight of
// its size, or zero for a name-only document. A successful producer call
// transfers ownership to the consumer. If a Builder accepts the content, Zoekt
// can keep it until Finish returns, and guardWrappedShards can add it again.
// The consumer releases the weight on an earlier error, or:
//
//   - indexDocumentsWithRepository releases a normal document when
//     Builder.Add returns, because Zoekt bounds the content that it keeps. It
//     releases a window of large documents after Finish and the shard guard.
//   - indexDeltaDocumentsWithRepository releases the set before return and,
//     after Builder creation, only after Finish and the shard guard.
//
// Zero weight means that a synchronous folder-delta read did not use the
// semaphore, or that the document is name-only. Releasing zero has no effect.
type fileContent struct {
	name       string
	content    []byte
	weight     int64
	branches   []string
	skipReason index.SkipReason
}

// readFilesToChannel reads files from the working tree using a bounded
// worker pool and sends them to out. It sends some files by name only. These
// are cloud-only files, files whose content does not fit the index, and files
// that start with binary content (readContent). It skips files that are not
// regular, such as symlinks and directories, and a file removed after git
// status. It also sends by name only a file that it cannot read for a lasting
// reason. Another read error goes to failure (dirtyFailure.passing), as in the
// folder reader. It closes out after all workers exit.
//
// Each worker reserves readSemaphore weight before it opens a file. A
// successful send transfers release ownership to the consumer. The worker
// releases the weight on all other paths.
func readFilesToChannel(ctx context.Context, repoDir string, files []string, parallelism int, out chan<- fileContent, failure *dirtyFailure) {
	workers := fileReadWorkerCount(parallelism, len(files))
	ch := make(chan string, workers)
	var wg sync.WaitGroup

	for range workers {
		wg.Go(func() {
			for f := range ch {
				readOneDirtyFile(ctx, repoDir, f, out, failure)
			}
		})
	}

feed:
	for _, f := range files {
		select {
		case ch <- f:
		case <-ctx.Done():
			// As streamFolderFilesWithMarkers does, stop feeding workers on
			// cancellation, so the producer does not outlive the consumers.
			break feed
		}
	}
	close(ch)
	wg.Wait()
	close(out)
}

// readOneDirtyFile reads one dirty file in its own function. So the deferred
// Release of readSemaphore weight runs on every exit path, also on a panic in
// readContent. The `released` flag transfers ownership to the consumer once
// the channel send succeeds.
func readOneDirtyFile(ctx context.Context, repoDir, f string, out chan<- fileContent, failure *dirtyFailure) {
	srcPath := filepath.Join(repoDir, f)
	failed := func(err error) {
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
			// Gone after git status, or under a directory that is now a
			// file: the next search's state drops it.
		case folderReadIsLasting(err):
			// An unreadable dirty file stays searchable by name.
			slog.Debug("Indexing an unreadable uncommitted file by name only", "path", f, "error", err)
			sendNameOnly(ctx, out, f, seekNoBytesReason)
		default:
			failure.passing(ctx, out, f, &folderReadError{path: srcPath, err: err})
		}
	}

	// Use Lstat to avoid following symlinks.
	var st unix.Stat_t
	if err := dirtyLstat(srcPath, &st); err != nil {
		failed(err)
		return
	}

	// Only process regular files: skip directories, symlinks, FIFOs,
	// sockets, and devices, which can block a read or hold unexpected
	// data.
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return
	}

	// A cloud-only file is never opened: the open would download it.
	if fileIsDataless(f, &st) {
		sendNameOnly(ctx, out, f, seekNoBytesReason)
		return
	}

	size := st.Size
	if !documentContentFits(size) {
		// Send the name without content or weight: Zoekt stores its
		// NOT-INDEXED marker, and the name stays searchable.
		slog.Debug("Indexing large uncommitted file by name only", "path", f, "size_mb", size/(1024*1024))
		sendNameOnly(ctx, out, f, index.SkipReasonTooLarge)
		return
	}
	weight := readWeight(size)
	if err := readSemaphore.Acquire(ctx, weight); err != nil {
		return // ctx cancelled or done; semaphore not Acquired.
	}
	released := false
	defer func() {
		if !released {
			readSemaphore.Release(weight)
		}
	}()

	// Read using the known size from Lstat to avoid the extra
	// Fstat that os.ReadFile performs internally. readContent
	// stops after its binary probe for a binary file, and keeps
	// partial content only when the file shrank during the read.
	fh, err := dirtyOpenFile(srcPath)
	if err != nil {
		failed(err)
		return
	}
	buf, binary, _, err := readContent(fh, size)
	_ = fh.Close()
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		failed(err)
		return
	}
	if binary {
		// No content to hold: the deferred release returns the weight.
		sendNameOnly(ctx, out, f, index.SkipReasonBinary)
		return
	}
	n := len(buf)
	if n == 0 && size > 0 {
		return
	}

	// Transfer Release ownership to the consumer. The select also ends on
	// a cancel, so a consumer that stopped reading cannot keep the worker
	// and its semaphore weight forever. released stays false on the
	// ctx.Done path so the deferred sentinel returns the weight.
	select {
	case out <- fileContent{name: f, content: buf[:n], weight: weight}:
		released = true
	case <-ctx.Done():
		return
	}
}

// recordNameOnlyMarkers passes the documents of in to the channel that it
// returns. It sets markers[name] to the marker bytes of each document that
// the shard keeps by name only (nameOnlyMarkerBytes). The consumer must drain
// the channel to its end, as indexDocuments does: markers is complete when
// the channel closes.
func recordNameOnlyMarkers(in <-chan fileContent, markers map[string]int64) <-chan fileContent {
	out := make(chan fileContent)
	go func() {
		defer close(out)
		for doc := range in {
			if marker := nameOnlyMarkerBytes(doc); marker > 0 {
				markers[doc.name] = marker
			}
			out <- doc
		}
	}()
	return out
}

// streamDirtyFiles returns a channel that yields file contents read from
// the working tree. The output channel is unbuffered because Zoekt's
// public Builder API accepts []byte documents, not io.Reader streams. Two
// limits bound the memory of the documents that are read and not yet
// released, and the smaller one applies:
//   - the worker count (each worker holds at most one fileContent);
//   - readSemaphore's readBudgetBytes weight budget (see caps.go).
//
// Each yielded fileContent carries its reserved readSemaphore weight. The
// consumer releases it: a Zoekt build as fileContent states, and a vector
// build after the last model batch of the file (semanticBuildOwner). A caller
// must drain the channel or cancel ctx. failure decides what a temporary read
// error does (dirtyFailure).
func streamDirtyFiles(ctx context.Context, repoDir string, files []string, parallelism int, failure *dirtyFailure) <-chan fileContent {
	out := make(chan fileContent)
	go readFilesToChannel(ctx, repoDir, files, parallelism, out, failure)
	return out
}

// dirtyFailure decides what a temporary read error (EIO, EMFILE, ESTALE:
// folderReadError) does to a dirty file. The text index keeps the file by name
// and notes the error. The build applies every other change, and the caller
// returns the error after the publish. So no state is written, and the next
// search reads the file again. The vector build stops instead (stop): a
// generation bound to the state must hold every file.
type dirtyFailure struct {
	stop context.CancelCauseFunc
	// keep keeps a file by name after a read error; without it the build
	// stops. EMFILE and ENFILE stop it anyway: they are no property of the
	// file, and the live family stays whole.
	keep bool
	mu   sync.Mutex
	err  error
	kept []string
}

// passing applies the failure rule to err, a read error of the dirty file
// name. It stops the build, or sends the file by name only and notes err.
func (failure *dirtyFailure) passing(ctx context.Context, out chan<- fileContent, name string, err error) {
	if !failure.keep || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
		failure.stop(err)
		return
	}
	failure.mu.Lock()
	if failure.err == nil {
		failure.err = err
	}
	failure.kept = append(failure.kept, name)
	failure.mu.Unlock()
	slog.Debug("Indexing an uncommitted file by name only after a read error", "path", name, "error", err)
	sendNameOnly(ctx, out, name, seekNoBytesReason)
}

// noted returns the files that the text index kept by name after a read
// error, as one dirtyFilesKept error, or nil.
func (failure *dirtyFailure) noted() error {
	failure.mu.Lock()
	defer failure.mu.Unlock()
	if failure.err == nil {
		return nil
	}
	files := slices.Clone(failure.kept)
	slices.Sort(files)
	return &dirtyFilesKept{files: files, cause: failure.err}
}

// dirtyFilesKept reports the dirty files that a build kept by name after a
// temporary read error. The build published everything else, the state
// stays unwritten, and a pending manifest marks these files kept, so the next
// search reads only them again.
type dirtyFilesKept struct {
	files []string
	cause error
}

func (e *dirtyFilesKept) Error() string {
	return fmt.Sprintf("%d uncommitted files kept by name after a read error: %v", len(e.files), e.cause)
}

func (e *dirtyFilesKept) Unwrap() error { return e.cause }

// keptEntrySize marks a manifest entry of a file kept by name. No stat has
// this size, so the next diff reads the file again. Or it tombstones the file
// when the file left the dirty set.
const keptEntrySize = -1

// markKept returns the manifest entries with each kept file marked.
func markKept(entries []uncommittedManifestEntry, names []string) []uncommittedManifestEntry {
	kept := make(map[string]struct{}, len(names))
	for _, name := range names {
		kept[name] = struct{}{}
	}
	out := slices.Clone(entries)
	for index := range out {
		if _, ok := kept[out[index].Name]; ok {
			out[index].Size = keptEntrySize
		}
	}
	return out
}

// onlyKept reports changed paths that are all kept files of the manifest:
// such a delta reads only them, whatever the family's shard count.
func onlyKept(changed []string, manifest uncommittedManifest) bool {
	if len(changed) == 0 {
		return false
	}
	kept := map[string]struct{}{}
	for _, entry := range manifest.Files {
		if entry.Size == keptEntrySize {
			kept[entry.Name] = struct{}{}
		}
	}
	for _, name := range changed {
		if _, ok := kept[name]; !ok {
			return false
		}
	}
	return true
}

// dirtyOpenFile opens a dirty file for reading, and dirtyLstat stats it. Tests
// replace them to fail one chosen file with EIO or EMFILE. A test cannot make
// these faults on disk. EIO comes from a failing disk, and a lower file limit
// fails every open of the process. Tests also replace dirtyOpenFile to see
// which files a build reads, or to write a file just after its read. No
// file-system state shows these reads.
var (
	dirtyOpenFile = openWorkTreeFile
	dirtyLstat    = lstatWorkTreeFile
)

// lstatWorkTreeFile stats path into st with no symlink follow. One stat gives
// the file type, the size and the cloud-only flag. As os.Lstat does, it tries
// again after EINTR and puts the path in the error.
func lstatWorkTreeFile(path string, st *unix.Stat_t) error {
	err := unix.Lstat(path, st)
	for errors.Is(err, unix.EINTR) {
		err = unix.Lstat(path, st)
	}
	if err != nil {
		return &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	return nil
}

// errNotRegularFile reports a work-tree path that is no longer a regular file
// when it is opened. A FIFO, socket, device, directory or symlink replaced the
// file after its stat. It wraps fs.ErrNotExist, so the dirty-file reader
// treats the file as removed.
var errNotRegularFile = fmt.Errorf("not a regular file: %w", fs.ErrNotExist)

// openWorkTreeFile opens a work-tree file that a stat found regular. The path
// can change between the stat and the open, and the open of a FIFO with no
// writer blocks, with the build lock held. So the open does not block and does
// not follow a symlink, and the open file must still be regular.
func openWorkTreeFile(path string) (*os.File, error) {
	const flags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
	file, err := os.OpenFile(path, flags, 0)
	// Linux: another process holds a write lease (a Samba oplock, an NFS
	// delegation). The failed open starts the lease break, so the same open
	// works after it, within the kernel's lease-break time (45 s by default).
	deadline := time.Now().Add(time.Minute)
	for wait := time.Millisecond; errors.Is(err, syscall.EWOULDBLOCK) && time.Now().Before(deadline); wait = min(2*wait, time.Second) {
		time.Sleep(wait)
		file, err = os.OpenFile(path, flags, 0)
	}
	switch {
	case errors.Is(err, syscall.ELOOP):
		// O_NOFOLLOW: the last component is now a symlink.
		return nil, errNotRegularFile
	case errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENXIO):
		// A socket (EOPNOTSUPP on macOS, ENXIO on Linux) or a device with no
		// driver.
		return nil, errNotRegularFile
	case errors.Is(err, syscall.ENOTDIR):
		// A directory of the path is now a file.
		return nil, errNotRegularFile
	case err != nil:
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errNotRegularFile
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// fileReadWorkerCount returns the reader count for items files: parallelism,
// at least one, and no more than the files.
func fileReadWorkerCount(parallelism, items int) int {
	return min(max(parallelism, 1), items)
}

// indexUncommitted publishes the dirty files of state into the "uncommitted"
// Zoekt repo, under the publish lock. A stage built before the lock
// (prebuilt) replaces the whole family. Otherwise uncommittedDeltaPossible
// found delta. The files whose size, mtime, inode, mode, or ctime changed are
// read again and written into a delta shard. Vanished files become tombstones
// in the .meta sidecars of older shards. The manifest carries the tag
// preState. The next build reads .state, which holds preState, and finds the
// manifest by it. A delta that fails for a reason other than a read error
// (folderReadError) rebuilds the family in full.
func indexUncommitted(
	ctx context.Context,
	repoDir string,
	store corpusStore,
	state repoState,
	preState string,
	parallelism int,
	prebuilt *uncommittedStage,
	delta *uncommittedDelta,
) error {
	if prebuilt != nil {
		return prebuilt.publish(store, preState)
	}
	if len(delta.changed) == 0 {
		// No dirty file changed (a HEAD change, a vector build): the family
		// stays, whatever its shard count.
		if err := writeUncommittedManifest(store.cacheDir, preState, delta.entries, delta.tombstoned); err != nil {
			slog.Debug("Failed to refresh uncommitted manifest", "error", err)
		}
		return nil
	}
	err := tryUncommittedDelta(ctx, repoDir, store, delta, preState, parallelism)
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*folderReadError](err); ok {
		// The delta kept some files by name and published the rest
		// (dirtyFilesKept), or EMFILE or ENFILE stopped it before any
		// write. A full rebuild would read the same files.
		return err
	}
	switch {
	case errors.Is(err, errDeltaPayloadExceedsWindow):
		slog.Debug("Uncommitted delta payload exceeds window threshold; full rebuild", "error", err)
	case errors.Is(err, errDeltaCompaction):
		slog.Debug("Uncommitted deltas tombstoned as many bytes as the family holds; full rebuild")
	default:
		slog.Debug("Uncommitted delta indexing failed, falling back to full rebuild", "error", err)
	}
	// The delta fell back (a large payload, a Builder error): build in full
	// under the lock, as the prebuild does outside it.
	stage, err := buildUncommittedStage(ctx, repoDir, store.indexDir, state, parallelism)
	if err != nil {
		return err
	}
	defer stage.discard()
	return stage.publish(store, preState)
}

// uncommittedDelta is a change that the live uncommitted family can take. It
// holds the manifest of the cached state, the files to read, and the changed
// paths. entries are the manifest entries of the dirty files, each with the
// marker of copyUncommittedMarkers. tombstoned is the content that deltas
// tombstoned since the last full build, with this delta.
type uncommittedDelta struct {
	tombstoned int64
	manifest   uncommittedManifest
	entries    []uncommittedManifestEntry
	toRead     []uncommittedCandidate
	changed    []string
}

// uncommittedDeltaPossible returns the delta that the live uncommitted family
// can take, or nil for a full rebuild. A delta needs:
//   - a manifest of the cached state;
//   - a family that matches .family-v1 (familyIntact);
//   - a contiguous family of at most maxUncommittedDeltaShards shards (any
//     count when only kept files changed);
//   - tombstoned content that does not need a compaction
//     (deltaNeedsCompaction);
//   - files to read that fit one index window.
//
// A delta of removals only reads no file. The function writes nothing, so a
// build can ask before the publish lock. The build lock keeps other builds
// out, so the delta stays valid under the publish lock. A read repair can
// still remove .family-v1 under the publish lock alone. Then familyIntact is
// stale, the build writes the manifest again, and the next damaged read
// repairs once more.
func uncommittedDeltaPossible(ctx context.Context, repoDir string, store corpusStore, cachedState string, familyIntact bool, files []string) *uncommittedDelta {
	cacheDir, indexDir := store.cacheDir, store.indexDir
	manifest, ok := readUncommittedManifest(cacheDir, cachedState)
	if !ok {
		return nil
	}
	if !familyIntact {
		// The family lost a shard, has a damaged shard, or a move stopped part
		// way. A keep or a delta would record the rest as complete.
		slog.Debug("Uncommitted family does not match its manifest; full rebuild", "index_dir", indexDir)
		return nil
	}
	existing := repositoryShardFiles(indexDir, repoUncommitted)
	if len(existing) == 0 {
		return nil
	}
	// The uncommitted family renumbers as the committed one does. A delta
	// onto a gapped family writes over the shards above the gap, and their
	// dirty content can then never be found.
	if !pathsAreContiguous(existing) {
		slog.Warn("Uncommitted shard family has a numbering gap; rebuilding it in full",
			"index_dir", indexDir)
		return nil
	}
	candidates := statUncommittedCandidates(repoDir, files)
	toRead, changed, tombstoned := diffUncommittedAgainstManifest(candidates, manifest)
	entries := uncommittedManifestEntries(candidates)
	copyUncommittedMarkers(entries, manifest)
	delta := &uncommittedDelta{
		manifest:   manifest,
		entries:    entries,
		toRead:     toRead,
		changed:    changed,
		tombstoned: manifest.DeadBytes + tombstoned,
	}
	if len(changed) == 0 {
		return delta // the locked part only refreshes the manifest
	}
	// The delta falls back to a full rebuild for these: build it before the
	// lock instead. A delta of removals only reads no file, and Zoekt writes
	// only its tombstones.
	if len(existing) > maxUncommittedDeltaShards && !onlyKept(changed, manifest) {
		slog.Debug("Uncommitted delta shard limit reached, falling back to full rebuild", "shards", len(existing))
		return nil
	}
	// Before the reads, each changed file counts at an upper bound of its
	// shard bytes (unreadFileMarker); tryUncommittedDelta checks again with
	// the read bytes.
	if deltaNeedsCompaction(delta.tombstoned, uncommittedLiveBytes(entries)) {
		slog.Debug("Uncommitted deltas tombstoned as many bytes as the family holds; full rebuild")
		return nil
	}
	var payload int64
	window := indexWindow(ctx)
	for _, candidate := range toRead {
		if payload += candidate.size; payload >= window {
			slog.Debug("Uncommitted delta payload exceeds window threshold; full rebuild", "payload", payload, "window", window)
			return nil
		}
	}
	return delta
}

// uncommittedStage is a full uncommitted family built in a build directory
// (the gc sweep removes one that a crash leaves).
type uncommittedStage struct {
	dir     string
	failure *dirtyFailure
	entries []uncommittedManifestEntry
}

// prepareUncommitted decides how the uncommitted family changes. delta is the
// change that the live family takes under the publish lock, or nil for a full
// rebuild. A full rebuild reads every dirty file, so it builds its stage here,
// in its own directory before the publish lock. Readers then wait only for the
// swap. prebuildErr is an EMFILE or ENFILE of that build, which the caller can
// survive; err is any other failure.
func prepareUncommitted(
	ctx context.Context,
	repoDir string,
	store corpusStore,
	cachedState string,
	familyIntact bool,
	state repoState,
	parallelism int,
) (delta *uncommittedDelta, stage *uncommittedStage, prebuildErr, err error) {
	if delta = uncommittedDeltaPossible(ctx, repoDir, store, cachedState, familyIntact, state.Files); delta != nil {
		return delta, nil, nil, nil
	}
	stage, err = buildUncommittedStage(ctx, repoDir, store.indexDir, state, parallelism)
	switch {
	case err == nil:
		return nil, stage, nil, nil
	case ctx.Err() != nil:
		return nil, nil, nil, ctx.Err()
	case errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE):
		return nil, nil, err, nil
	}
	return nil, nil, nil, err
}

// buildUncommittedStage builds the full uncommitted family of state in a new
// build directory. A file with a temporary read error is kept by name
// (stage.failure). EMFILE, ENFILE, or a cancel discards the stage.
func buildUncommittedStage(ctx context.Context, repoDir, indexDir string, state repoState, parallelism int) (*uncommittedStage, error) {
	entries := uncommittedManifestEntries(statUncommittedCandidates(repoDir, state.Files))
	dir, err := newBuildDir(indexDir)
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stage := &uncommittedStage{dir: dir, failure: &dirtyFailure{keep: true, stop: cancel}, entries: entries}
	markers := make(map[string]int64)
	fileCh := recordNameOnlyMarkers(streamDirtyFiles(readCtx, repoDir, state.Files, parallelism, stage.failure), markers)
	_, err = indexDocuments(readCtx, dir, repoUncommitted, repoDir, fileCh, parallelism)
	for i := range entries {
		entries[i].Marker = markers[entries[i].Name]
	}
	if readErr, ok := errors.AsType[*folderReadError](context.Cause(readCtx)); ok {
		// EMFILE or ENFILE: the live family stays whole.
		err = readErr
	}
	if err == nil {
		// A cancel stops the readers with no error: the stage is partial.
		err = ctx.Err()
	}
	if err != nil {
		stage.discard()
		return nil, err
	}
	return stage, nil
}

// publish replaces the whole live family with the stage, under the publish
// lock, with the swap of the committed family (swapFiles). So a gapped old
// family leaves no stale shard beside the new one, and a failed step moves
// the live family back. A failed move back leaves a partial family and no
// manifest, so the next search rebuilds it. A file kept by name after a read
// error leaves the state unwritten.
func (stage *uncommittedStage) publish(store corpusStore, preState string) error {
	live := repositoryFamilyFiles(store.indexDir, repoUncommitted)
	incoming := repositoryShardFiles(stage.dir, repoUncommitted)
	if _, restored, err := swapFiles(store.indexDir, stage.dir, live, incoming); err != nil {
		if !restored {
			deleteUncommittedManifest(store.cacheDir)
		}
		return fmt.Errorf("publish uncommitted shards: %w", err)
	}
	removeRepositoryShardTemps(store.indexDir, repoUncommitted)
	if err := stage.failure.noted(); err != nil {
		writePendingUncommittedManifest(store.cacheDir, stage.entries, err, 0)
		return err
	}
	if err := writeUncommittedManifest(store.cacheDir, preState, stage.entries, 0); err != nil {
		slog.Debug("Failed to write uncommitted manifest", "error", err)
	}
	return nil
}

func (stage *uncommittedStage) discard() {
	discardBuildDir(stage.dir)
}

// tryUncommittedDelta writes delta into the live uncommitted family, with up
// to parallelism shard jobs at the same time. It writes the manifest with the
// tag preState (the new .state value). A delta of removals only reads no file,
// and Zoekt writes only its tombstones (indexDeltaDocumentsWithRepository).
// For one removed file of 400 dirty files, a full rebuild took 3.0 s, and this
// delta took 0.12 s. The delta runs under the publish lock, so readers wait
// less.
func tryUncommittedDelta(
	ctx context.Context,
	repoDir string,
	store corpusStore,
	delta *uncommittedDelta,
	preState string,
	parallelism int,
) error {
	cacheDir, indexDir, entries := store.cacheDir, store.indexDir, delta.entries
	docs, noted, err := readUncommittedCandidates(ctx, repoDir, delta.toRead)
	if err != nil {
		// readUncommittedCandidates releases every weight before it returns
		// an error (errDeltaPayloadExceedsWindow or a folderReadError).
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A cancel cut the reads short: write nothing.
		releaseFileContentWeights(docs)
		return ctxErr
	}
	if kept, ok := errors.AsType[*dirtyFilesKept](noted); ok && onlyKept(delta.changed, delta.manifest) && len(kept.files) == len(delta.changed) {
		// Every changed file is a kept file that failed again: the family
		// holds it by name already, so write no shard and no tombstone.
		releaseFileContentWeights(docs)
		writePendingUncommittedManifest(cacheDir, entries, noted, delta.manifest.DeadBytes)
		return noted
	}
	// The reads give the bytes that each changed file's shard document holds.
	setUncommittedMarkers(entries, docs)
	if deltaNeedsCompaction(delta.tombstoned, uncommittedLiveBytes(entries)) {
		releaseFileContentWeights(docs)
		return errDeltaCompaction
	}
	// indexDeltaDocuments releases all per-document readSemaphore weights; do
	// not release them here.
	if _, err := indexDeltaDocuments(indexDir, repoUncommitted, repoDir, docs, parallelism, delta.changed); err != nil {
		return err
	}
	if noted != nil {
		// The delta applied every change and kept some files by name: no
		// state, and a pending manifest that marks them.
		writePendingUncommittedManifest(cacheDir, entries, noted, delta.tombstoned)
		return noted
	}
	if err := writeUncommittedManifest(cacheDir, preState, entries, delta.tombstoned); err != nil {
		slog.Debug("Failed to write uncommitted manifest", "error", err)
	}
	return nil
}

// readUncommittedCandidates loads file contents for the given
// candidates from the working tree. It skips missing and non-regular files
// with no notice, and sends unreadable ones by name only (as
// readFilesToChannel does). The second result names the files kept by name
// after a temporary read error (dirtyFailure.noted). EMFILE or ENFILE returns
// a folderReadError and no documents.
//
// The delta holds every document read here until its Finish. So two checks
// keep the held weight below the build's window, and the global readSemaphore
// cannot stay full:
//   - Before the reads: uncommittedDeltaPossible gives no delta when the sum
//     of candidate.size (from the earlier lstat) reaches the window.
//   - During the reads: when the weight of the documents read reaches the
//     window, it cancels the inner context. The workers stop at their next
//     Acquire, and it releases the weight of every document. This check also
//     sees a file that grew after its lstat.
//
// The second check returns errDeltaPayloadExceedsWindow. tryUncommittedDelta
// returns it, and indexUncommitted then builds the family in full
// (buildUncommittedStage).
func readUncommittedCandidates(ctx context.Context, repoDir string, candidates []uncommittedCandidate) ([]fileContent, error, error) {
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	files := make([]string, len(candidates))
	for i, c := range candidates {
		files[i] = c.name
	}
	innerCtx, cancelInner := context.WithCancelCause(ctx)
	defer cancelInner(nil)
	failure := &dirtyFailure{keep: true, stop: cancelInner}
	ch := streamDirtyFiles(innerCtx, repoDir, files, indexParallelism(), failure)
	docs := make([]fileContent, 0, len(files))
	var cumulative int64
	exceeded := false
	for doc := range ch {
		if exceeded {
			// Release each later document at once, so the workers that
			// acquired weight before the cancel, and the producer, can
			// end.
			if doc.weight > 0 {
				readSemaphore.Release(doc.weight)
			}
			continue
		}
		docs = append(docs, doc)
		cumulative += doc.weight
		if cumulative >= indexWindow(ctx) {
			exceeded = true
			cancelInner(nil)
		}
	}
	if readErr, ok := errors.AsType[*folderReadError](context.Cause(innerCtx)); ok {
		// EMFILE or ENFILE: no shard is written, the live family stays.
		releaseFileContentWeights(docs)
		return nil, nil, readErr
	}
	if exceeded {
		releaseFileContentWeights(docs)
		return nil, nil, errDeltaPayloadExceedsWindow
	}
	return docs, failure.noted(), nil
}

// indexDocuments adapts a repository name and source to the shared Builder
// sink.
func indexDocuments(
	ctx context.Context,
	indexDir string,
	repoName string,
	source string,
	fileCh <-chan fileContent,
	parallelism int,
) (bool, error) {
	repository := zoekt.Repository{Name: repoName, Source: source}
	return indexDocumentsWithRepository(ctx, indexDir, repository, fileCh, parallelism, nil)
}

// indexDocumentsWithRepository consumes fileContent with one Builder for the
// normal documents of a build. It releases the read weight of a normal document
// when Builder.Add returns. Zoekt then bounds the content that it keeps: one
// pending shard, and at most parallelism shards that it builds at one time.
// Windows of normal documents at the read window add a Finish barrier and a
// .meta rewrite each. Against such windows, one Builder built the Kubernetes
// graph of 41 nodes 10 % faster. It built the graph of 81 nodes 27 % to 30 %
// faster (the windows took 33 % to 39 % longer).
//
// A document that fills a shard of its own (isolatesShard) goes to a separate
// window. The window finishes at min(parallelism, largeDocumentParallelism)
// documents, or when its content reaches or passes indexWindow(ctx).
// guardWrappedShards checks those shards after Finish and can add the
// documents again, so the window keeps their weight until then. Two Builders
// cannot number shards in one directory at the same time, so each switch
// between normal and large documents finishes the open Builder. This
// scheduling rule does not change content or symbol indexing. A document that
// no Builder accepted is released when its error path ends. See fileContent
// for ownership.
//
// Window 0 uses IsDelta=false, so Finish prunes stale shards. Later windows use
// IsDelta=true with no changed paths, so they continue shard numbering. The
// Finish of such a window rewrites the .meta sidecar of every older shard.
// The first result is true after a Builder opens. When no document arrives,
// it removes the shards of the repository in indexDir (a build directory) and
// returns false.
func indexDocumentsWithRepository(
	ctx context.Context,
	indexDir string,
	repository zoekt.Repository,
	fileCh <-chan fileContent,
	parallelism int,
	cancelProducer context.CancelFunc,
) (bool, error) {
	if cancelProducer == nil {
		cancelProducer = func() {}
	}
	// A large window rotates at largeParallelism documents, so one build runs
	// at most that many large shard jobs. Builds share no token. Else a
	// consumer could wait for another build's token while it holds its
	// document. At the same time, that build could wait for readers that the
	// read budget blocks.
	largeParallelism := min(max(parallelism, 1), largeDocumentParallelism)
	window := indexWindow(ctx)
	var current *index.Builder
	// pendingWeight and pendingContent count the large window only.
	// pendingContent counts real content bytes; readWeight stops counting at
	// maxReadWeightBytes, so a large window rotates on content.
	var pendingWeight int64
	var pendingContent int64
	var addErr error
	indexedAny := false
	currentLarge := false
	largeDocuments := 0
	// A large window gives each document its own shard; the guard checks them
	// after Finish (guardWrappedShards).
	var currentOpts index.Options
	currentFirstShard := 0
	var largeWindowDocs []index.Document
	guardOpts := indexBuildOptions(indexDir, 1)

	openWindow := func(isDelta, large bool) error {
		if err := checkCtagsCached(); err != nil {
			return err
		}
		builderParallelism := parallelism
		if large {
			builderParallelism = largeParallelism
		}
		opts := indexBuildOptions(indexDir, builderParallelism)
		opts.RepositoryDescription = repository
		opts.IsDelta = isDelta
		firstShard := 0
		if large {
			firstShard = firstBuilderShard(opts)
		}
		// A later window is a delta Builder: it reads the records of the
		// shards that the earlier windows wrote.
		var b *index.Builder
		err := zoektShardCall(func() (err error) {
			b, err = index.NewBuilder(opts)
			return err
		})
		if err != nil {
			return fmt.Errorf("create builder: %w", err)
		}
		current = b
		currentLarge = large
		currentOpts = opts
		currentFirstShard = firstShard
		largeDocuments = 0
		indexedAny = true
		return nil
	}

	// finishWindow finishes the current Builder and releases the pending
	// weight, also when Finish fails. Finish waits for every shard goroutine
	// (b.building.Wait) before it returns, also after a shard error. So no
	// goroutine keeps any doc.Content past this point.
	finishWindow := func() error {
		if current == nil {
			return nil
		}
		err := zoektShardCall(current.Finish)
		if err == nil && currentLarge {
			// Run before the weight release: the guard can add the
			// documents again.
			err = guardWrappedShards(currentOpts, currentFirstShard, largeWindowDocs, nil)
		}
		clear(largeWindowDocs)
		largeWindowDocs = largeWindowDocs[:0]
		pendingContent = 0
		if pendingWeight > 0 {
			readSemaphore.Release(pendingWeight)
			pendingWeight = 0
		}
		// openWindow sets currentLarge and largeDocuments again.
		current = nil
		return err
	}

	// abandon stops the producer, releases the weight that the current
	// document holds and no Builder accepted, and drains the rest.
	abandon := func(weight int64) {
		cancelProducer()
		readSemaphore.Release(weight)
		for d := range fileCh {
			readSemaphore.Release(d.weight)
		}
	}

	for doc := range fileCh {
		if err := ctx.Err(); err != nil {
			abandon(doc.weight)
			_ = finishWindow()
			return indexedAny, err
		}
		if addErr != nil {
			readSemaphore.Release(doc.weight)
			continue
		}

		document := index.Document{
			Name:       doc.name,
			Content:    doc.content,
			Branches:   doc.branches,
			SkipReason: doc.skipReason,
		}
		// Only a document that Zoekt keeps and flushes alone opens a large
		// window, so each large window maps one document to one shard.
		large := isolatesShard(document, guardOpts)
		if current != nil && large != currentLarge {
			if err := finishWindow(); err != nil {
				abandon(doc.weight)
				return indexedAny, err
			}
		}
		if current == nil {
			if err := openWindow(indexedAny, large); err != nil {
				abandon(doc.weight)
				return indexedAny, err
			}
		}

		// Quality invariant: file size changes scheduling only. Do not set
		// Document.Symbols to a non-nil empty slice or disable ctags for a
		// large document. Either action would make sym: results depend on file
		// size. Zoekt's Builder.Add buffers doc into b.todo before any error
		// return. A large window keeps doc.weight until finishWindow, after the
		// guard.
		if currentLarge {
			pendingWeight += doc.weight
			pendingContent += int64(len(doc.content))
			largeWindowDocs = append(largeWindowDocs, document)
		}
		err := current.Add(document)
		if !currentLarge {
			// From here, Zoekt's flush throttle bounds this content, and no
			// guard adds a normal document again.
			readSemaphore.Release(doc.weight)
		}
		if err != nil {
			addErr = fmt.Errorf("add document %s: %w", doc.name, err)
			cancelProducer()
			continue
		}
		if large {
			largeDocuments++
		}

		if currentLarge && (largeDocuments >= largeParallelism || pendingContent >= window) {
			if err := finishWindow(); err != nil {
				abandon(0)
				return indexedAny, err
			}
		}
	}

	if current != nil {
		if err := finishWindow(); err != nil {
			cancelProducer()
			if addErr != nil {
				return true, addErr
			}
			return true, err
		}
	} else if !indexedAny {
		cleanRepositoryShards(indexDir, repository.Name)
		return false, nil
	}

	if addErr != nil {
		return true, addErr
	}
	return true, nil
}

// writePendingUncommittedManifest writes the manifest of a published family
// that kept some files by name (noted). The manifest has no state, a kept mark
// on those files, and deadBytes. So the next search's delta reads the kept
// files again.
func writePendingUncommittedManifest(cacheDir string, entries []uncommittedManifestEntry, noted error, deadBytes int64) {
	kept, ok := errors.AsType[*dirtyFilesKept](noted)
	if !ok {
		deleteUncommittedManifest(cacheDir)
		return
	}
	if err := writeUncommittedManifest(cacheDir, "", markKept(entries, kept.files), deadBytes); err != nil {
		deleteUncommittedManifest(cacheDir)
	}
}

// cleanUncommittedShards removes the live uncommitted family with the swap of
// a publish (swapFiles, with no new file). A file that cannot move (an
// immutable file) fails the removal and moves the family back. So no shard
// stays without the others or without its tombstone records; such a shard
// would serve replaced content. The caller runs discard, which deletes the
// moved files, after the publish lock is released.
func cleanUncommittedShards(indexDir string) (discard func(), err error) {
	defer removeRepositoryShardTemps(indexDir, repoUncommitted)
	discard = func() {}
	live := repositoryFamilyFiles(indexDir, repoUncommitted)
	if len(live) == 0 {
		return discard, nil
	}
	dir, err := newBuildDir(indexDir)
	if err != nil {
		return discard, err
	}
	discard = func() { discardBuildDir(dir) }
	if _, _, err := swapFiles(indexDir, dir, live, nil); err != nil {
		return discard, fmt.Errorf("remove uncommitted shards: %w", err)
	}
	return discard, nil
}

// cleanRepositoryShards removes the shards, records and temporary shards of
// repoName in a build directory.
func cleanRepositoryShards(indexDir, repoName string) {
	for _, file := range repositoryFamilyFiles(indexDir, repoName) {
		_ = os.Remove(file)
	}
	removeRepositoryShardTemps(indexDir, repoName)
}

// repositoryFamilyFiles lists the shards and records of repoName.
func repositoryFamilyFiles(indexDir, repoName string) []string {
	metas, _ := filepath.Glob(filepath.Join(indexDir, repoName+"_v*.zoekt.meta"))
	return append(repositoryShardFiles(indexDir, repoName), metas...)
}

// removeRepositoryShardTemps removes the shards that a Zoekt Builder of
// repoName left under their temporary names (<shard>.<random>.tmp): Finish
// renames them only when it ends. The caller holds the node's build lock, so
// no Builder of the node writes them now.
func removeRepositoryShardTemps(indexDir, repoName string) {
	temps, _ := filepath.Glob(filepath.Join(indexDir, repoName+"_v*.zoekt.*.tmp"))
	for _, temp := range temps {
		_ = os.Remove(temp)
	}
}

func repositoryShardCount(indexDir, repoName string) int {
	return len(repositoryShardFiles(indexDir, repoName))
}

func repositoryShardFiles(indexDir, repoName string) []string {
	matches, err := filepath.Glob(filepath.Join(indexDir, repoName+"_v*.zoekt"))
	if err != nil {
		return nil
	}
	return matches
}
