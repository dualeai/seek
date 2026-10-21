package main

import (
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

	"github.com/cespare/xxhash/v2"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"golang.org/x/sys/unix"
)

const (
	// stateFile stores the hash of the last indexed corpus state.
	stateFile = ".state"
	// stateTmpFile is used for atomic writes of the state file.
	stateTmpFile = ".state.tmp"
	// headFile stores the HEAD SHA of the last successful committed index.
	// It is a fallback only: the shards record the commit they hold, and
	// committedShardHead reads it. This file answers when that record cannot
	// be read, so a damaged shard keeps the previous behaviour instead of
	// rebuilding on every search.
	headFile = ".head"
	// emptyFile stores the state hash for a layer that is known to have
	// no indexable shards. This prevents a missing/corrupt shard directory
	// from being confused with an intentionally empty scoped layer.
	emptyFile = ".empty"
	// lockFile is the publish/read lock. Readers hold a shared lock while they
	// list, open, and search shards. Publishers hold an exclusive lock while
	// they replace a shard family.
	lockFile = ".lock"
	// buildLockFile lets only one process build a corpus at a time. Readers do
	// not take this lock.
	buildLockFile = ".build.lock"
	// swappingMarkerFile records, while present, that a temp-swap publish was
	// interrupted mid-rename; the named shard family may be torn and must be
	// cleaned + fully rebuilt on the next build (see recoverIncompleteSwap).
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
var shardMax = 10 * 1024 * 1024 // 10 MB

// largeDocumentParallelism bounds the shard and ctags jobs of one build for
// documents that fill a shard of their own (isolatesShard): one per CPU, and at
// least 3, because seek's objective is the shortest time. Large windows also
// rotate on their real content bytes, which bounds memory for documents above
// the read-weight clamp. Seek does not reduce index quality for these files: it
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

// repoStateFingerprint returns raw Git status output plus the path, mtime,
// size, inode, mode, and ctime of each dirty file, the fields that the folder
// walk keeps. Git status porcelain does not include working-tree content
// hashes, so these fields detect common changes to files that are already
// dirty. This is a metadata fingerprint, not a content hash; an edit that
// preserves every observed field can keep the same fingerprint.
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

// stagedBase is the staged base generation when this build made one.
func stagedBase(built bool, staging string) string {
	if built {
		return staging
	}
	return ""
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
func writeCacheFile(cacheDir, name, value string) error {
	tmpPath := filepath.Join(cacheDir, name+".tmp")
	if err := os.WriteFile(tmpPath, []byte(value), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(cacheDir, name))
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
// disk, which a failed build did not change, and dropping it forced the next
// commit into a full rebuild. It still clears .head, which costs nothing now
// that .head is only a fallback.
func deleteStateFiles(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, stateFile))
	_ = os.Remove(filepath.Join(cacheDir, stateFile+".tmp"))
	_ = os.Remove(filepath.Join(cacheDir, headFile))
	_ = os.Remove(filepath.Join(cacheDir, headFile+".tmp"))
	deleteEmptyStateFiles(cacheDir)
	// .git-committed-v1 is kept. It records the delta base of the committed
	// family that is still on disk, and a failed build does not change that
	// family. Deleting it forced the next commit to rebuild in full.
	//
	// A stale record cannot produce a wrong answer: prepareNativeGitDelta
	// compares it with the commit read from the shard itself and rejects a
	// delta when the two disagree.
}

// indexParallelism returns the number of parallel indexing workers.
func indexParallelism() int {
	p := runtime.GOMAXPROCS(0)
	if p < 1 {
		p = 1
	}
	return p
}

func indexBuildOptions(indexDir string, parallelism int) index.Options {
	// Keep the same content and ctags contract for every accepted document.
	// Large-file controls can change only Builder scheduling and parallelism.
	//
	// TODO: Zoekt still replaces the content of two kinds of documents with a
	// NOT-INDEXED marker, and index.Options has no switch for either: content
	// with a NUL byte (index.SkipReasonBinary) and content shorter than three
	// bytes (index.SkipReasonTooSmall). Seek accepts both limits for now.
	// Removing them needs a Zoekt change. The readers find a NUL in the first
	// binaryProbeBytes themselves and do not read the rest (readContent).
	//
	// TrigramMax at the content limit turns off Zoekt's distinct-trigram skip
	// (default 20000). Zoekt counts only when len(content)-2 is above
	// TrigramMax, so the count never decides whether content is searchable. The
	// cost is index size and build memory for large files with many distinct
	// non-ASCII trigrams.
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
	if e.cause != nil {
		return fmt.Sprintf("universal-ctags executable not found: %v", e.cause)
	}
	return "universal-ctags executable not found"
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
	// 1. Explicit env var — trust the user.
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

// missingBlobRecheckTestHook runs after the missing-blob recheck and before
// the builds read blobs. Only tests set it.
var missingBlobRecheckTestHook func()

// runIndexingWithCacheExecution makes the joined Git index current with
// separate build and publish locks. Zoekt covers committed and uncommitted
// files. The semantic base generation covers the captured commit, and the
// overlay generation covers the dirty files:
//
//   - .build.lock serializes builders for the complete build.
//   - The committed family is built outside the publish lock. Native full uses
//     an empty temporary directory; eligible native delta seeds current shards.
//     These are the only committed-data routes. HEAD is checked before publish.
//   - A required semantic generation builds beside the committed Zoekt branch.
//     Each branch reads its own bounded stream from the same captured commit.
//   - A full uncommitted rebuild builds in its own directory before the publish
//     lock, and the lock covers only its swap. An uncommitted delta writes in
//     place under the lock. The shards, manifest, and state describe the same
//     generation.
//   - The joined descriptor is written last under the publish lock. A semantic
//     failure keeps the Zoekt generation searchable without joined retrieval.
//
// Readers hold the shared publish lock while they list, open, and search
// shards, so the text and vector indexes name the same generation; a reader
// waits for a publish with no time limit. A failed swap can leave shards until
// the next build repairs .swapping.
func runIndexingWithCacheExecution(
	ctx context.Context,
	paths gitPaths,
	cacheDir string,
	indexDir string,
	state repoState,
	preState string,
	execution searchExecution,
) error {
	repoDir := paths.RepoDir
	// Ensure partial temp files are cleaned up on all exit paths.
	defer func() {
		_ = os.Remove(filepath.Join(cacheDir, stateTmpFile))
		_ = os.Remove(filepath.Join(cacheDir, headFile+".tmp"))
		_ = os.Remove(filepath.Join(cacheDir, committedGitStateFile+".tmp"))
		_ = os.Remove(filepath.Join(cacheDir, lexicalMissingBlobsFile+".tmp"))
		_ = os.Remove(filepath.Join(cacheDir, semanticMissingBlobsFile+".tmp"))
	}()

	buildFd, acquired, err := acquireBuildLock(ctx, cacheDir, indexDir)
	if err != nil {
		return err
	}
	if !acquired {
		// Another builder is active and a usable index exists — serve current
		// shards; the active builder publishes the fresh ones.
		slog.Debug("Another process is indexing; serving current index")
		return nil
	}
	defer releaseLock(buildFd)

	// Repair a crash that left a half-published swap before trusting state.
	recoverIncompleteSwap(cacheDir, indexDir)

	// Check state and shard integrity again after acquiring the build lock. A
	// peer can publish between the first check and this point.
	cachedState := readStateFile(cacheDir)
	// One scan supplies the post-lock check, the seed decision,
	// and the needCommitted trigger below.
	sc, scErr := scanFamilyOptional(indexDir)
	if scErr != nil {
		return scErr
	}
	familyIntact := sc.matchesManifest(indexDir)
	// Check the committed family separately. A dirty edit can change only the
	// uncommitted family.
	committedIntact := sc.matchesManifestFamily(indexDir, familyCommitted)
	noHeadArtifacts := state.HeadSHA == "no-head" && sc.hasMember(familyCommitted)
	committedPresent := committedSnapshotReady(cacheDir, indexDir, state, sc)
	semanticSource := state.HeadSHA
	// Prepare vectors only when a route can read them. The model itself stays
	// available for re-ranking lexical candidates. runSearchCommand documents
	// which searches this covers.
	semanticRequired := execution.buildsVectors(state.HeadSHA)
	// A dirty node also binds the overlay of its dirty files.
	hasDirty := len(state.Files) > 0
	overlayRequired := execution.buildsOverlay(hasDirty)
	baseSource, overlaySource := "", ""
	if semanticRequired {
		baseSource = semanticSource
	}
	if overlayRequired {
		overlaySource = semanticOverlaySource(preState)
	}
	joinedReady := (!semanticRequired && !overlayRequired) || joinedGenerationsMatch(
		cacheDir,
		indexDir,
		preState,
		baseSource,
		overlaySource,
	)
	// A blob that a build read as missing can arrive with no change to the
	// state.
	storeFingerprint := buildObjectStoreFingerprint(ctx, repoDir, cacheDir)
	arrival, err := recheckMissingBlobs(ctx, repoDir, cacheDir, state.HeadSHA, semanticRequired, storeFingerprint)
	if err != nil {
		return gitCorpusError(repoDir, indexDir, err)
	}
	if missingBlobRecheckTestHook != nil {
		missingBlobRecheckTestHook()
	}
	if cachedState == preState && !noHeadArtifacts && committedPresent && familyIntact && joinedReady &&
		!arrival.lexical && !arrival.semantic {
		return nil
	}

	parallelism := execution.resources.cpuLimit()

	// Rebuild when the published shards hold another commit, or the committed
	// family no longer matches its manifest. publishedCommittedHead asks the
	// shards rather than the sidecar; committedShardHead documents why.
	needCommitted := state.HeadSHA != "no-head" &&
		(publishedCommittedHead(cacheDir, sc) != state.HeadSHA || !committedIntact || arrival.lexical)
	clearCommitted := noHeadArtifacts
	semanticPresent := semanticRequired && semanticGenerationPresent(indexDir, semanticSource)
	needSemantic := semanticRequired && (!semanticPresent || arrival.semantic)
	overlayPresent := overlayRequired && semanticGenerationPresent(indexDir, overlaySource)
	needOverlay := overlayRequired && !overlayPresent

	// Build Zoekt and semantic data together outside the publish lock. Each
	// branch reads the same captured commit through its own bounded Git stream.
	var buildDir string
	var nextCommittedState *committedGitState
	if needCommitted || clearCommitted || needSemantic || needOverlay {
		buildDir, err = newBuildDir(indexDir)
		if err != nil {
			return err
		}
		defer discardBuildDir(buildDir)
	}
	var snapshot gitSnapshot
	var lexicalMissing, semanticMissing []missingBlob
	if needCommitted || needSemantic {
		var ok bool
		snapshot, ok, err = captureGitSnapshot(ctx, repoDir, state.HeadSHA)
		if err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("captured committed Git state has no commit")
			}
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
	if needCommitted {
		lexicalBuild = func() {
			nextCommittedState, lexicalMissing, lexicalErr = buildCommittedGitStage(
				ctx,
				repoDir,
				cacheDir,
				indexDir,
				buildDir,
				snapshot,
				sc,
				parallelism,
				arrival,
			)
		}
	}
	var semanticBuild func()
	if needSemantic || needOverlay {
		semanticBuild = func() {
			semanticErr = runSemanticBuild(ctx, execution.model, func(embedder semanticModel) error {
				if needSemantic {
					if _, err := buildSemanticGitGeneration(
						ctx,
						repoDir,
						snapshot,
						indexDir,
						semanticGitLineage(paths),
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
				if needOverlay {
					if _, err := buildSemanticOverlayGeneration(
						ctx,
						repoDir,
						indexDir,
						semanticGitLineage(paths),
						overlayStaging,
						overlaySource,
						state.Files,
						embedder,
						execution.resources,
						stagedBase(semanticBuilt, semanticStaging),
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
		slog.Debug(
			"Semantic index build failed; keeping lexical search",
			"provider", semanticProviderName(),
			"error", semanticErr,
		)
		if errors.Is(semanticErr, errDegenerateSemanticVector) {
			rejectLateOnAcceleratedProvider(semanticErr.Error())
		}
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

	// A full uncommitted rebuild reads every dirty file: build it in its own
	// directory before the publish lock, so readers wait only for the swap. A
	// delta stays under the lock; it writes in place.
	var prebuilt *uncommittedStage
	// prebuildErr is an EMFILE or ENFILE of the prebuild: the committed
	// family and the vectors still publish, the live uncommitted family
	// stays, and no state is written.
	var prebuildErr error
	// checked holds the dirty files' stats of the check; the locked part
	// reads them instead of a second Lstat pass.
	var checked []uncommittedCandidate
	deltaPossible := false
	if hasDirty {
		deltaPossible, checked = uncommittedDeltaPossible(ctx, repoDir, indexDir, cacheDir, cachedState, state.Files)
	}
	if hasDirty && !deltaPossible {
		stage, err := buildUncommittedStage(ctx, repoDir, indexDir, state, parallelism)
		switch {
		case err == nil:
			defer stage.discard()
			prebuilt = stage
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE):
			prebuildErr = err
		default:
			return gitCorpusError(repoDir, indexDir, err)
		}
	}

	// Keep the committed publish, the uncommitted swap or delta, and state
	// files under one exclusive publish lock. Normal locked readers cannot
	// interleave with these writes. The state and HEAD writes below must
	// remain inside this lock.
	var publishedNames []string
	publishedCommitted := false

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
	if execution.policy.semanticEnabled() &&
		(needCommitted || clearCommitted || hasDirty || arrival.semantic) {
		// The old descriptor must not name a family while that family changes.
		removeJoinedGeneration(cacheDir)
	}

	if needCommitted {
		if err := writePendingMissingBlobRecord(cacheDir, lexicalMissingBlobsFile, state.HeadSHA, lexicalMissing); err != nil {
			return fmt.Errorf("write missing-blob record: %w", err)
		}
	}
	if needCommitted || clearCommitted {
		committedNames, pErr := publishShardFamilyLocked(cacheDir, indexDir, buildDir, familyCommitted)
		if pErr != nil {
			return fmt.Errorf("publish committed shards: %w", pErr)
		}
		publishedNames = committedNames
		publishedCommitted = true
	}

	// partial is the error of a build that kept dirty files by name, or
	// whose prebuild hit EMFILE: the publish goes on, and only the state and
	// the joined descriptor stay unwritten.
	var partial error
	if hasDirty && prebuildErr != nil {
		partial = prebuildErr
	} else if hasDirty {
		uncommittedErr := gitCorpusError(
			repoDir,
			indexDir,
			indexUncommitted(ctx, repoDir, indexDir, cacheDir, state, cachedState, preState, parallelism, prebuilt, checked),
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
		cleanUncommittedShards(indexDir)
		deleteUncommittedManifest(cacheDir)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		deleteStateFiles(cacheDir)
		return ctxErr
	}

	// The build publishes the captured state: a command runs one git status
	// per node. A file that changes during the build changes the state that
	// the next search captures (the hash stats the dirty files when it is
	// computed), so that search builds again: a change during the build never
	// stays hidden behind a recorded state.
	// Write the manifest under the same publish lock as the state. Use the
	// committed names returned by the swap and scan the uncommitted files
	// that this process just wrote. Record dirty-to-clean transitions too.
	names := append([]string(nil), publishedNames...)
	// This process changed the directory, so scan it again.
	post, postErr := scanFamily(indexDir)
	if postErr != nil {
		deleteStateFiles(cacheDir)
		return postErr
	}
	finalHasShard := post.hasShard()
	for _, m := range post.members {
		if m.uncommitted || !publishedCommitted {
			names = append(names, m.name)
		}
	}
	if err := writeFamilyManifest(indexDir, names); err != nil {
		deleteStateFiles(cacheDir)
		return fmt.Errorf("write family manifest: %w", err)
	}
	if needCommitted {
		if nextCommittedState == nil {
			deleteStateFiles(cacheDir)
			return fmt.Errorf("committed Git build did not return publication state")
		}
		if err := writeCommittedGitState(cacheDir, *nextCommittedState); err != nil {
			deleteStateFiles(cacheDir)
			return fmt.Errorf("write committed Git state: %w", err)
		}
		if err := writeMissingBlobRecord(cacheDir, lexicalMissingBlobsFile, missingBlobRecord{
			head:        state.HeadSHA,
			fingerprint: storeFingerprint,
			blobs:       lexicalMissing,
		}); err != nil {
			deleteStateFiles(cacheDir)
			return fmt.Errorf("write missing-blob record: %w", err)
		}
	} else if clearCommitted {
		deleteCommittedGitState(cacheDir)
		removeCacheFile(cacheDir, lexicalMissingBlobsFile)
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
	if semanticRequired || overlayRequired {
		var activationErr error
		baseReady := !semanticRequired || (semanticPresent && !arrival.semantic)
		if semanticBuilt {
			// The pending record comes first, so a generation never exists
			// without a record that names its missing blobs.
			activationErr = writePendingMissingBlobRecord(cacheDir, semanticMissingBlobsFile, semanticSource, semanticMissing)
			if activationErr == nil && semanticPublishTestHook != nil {
				activationErr = semanticPublishTestHook()
			}
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
		overlayReady := !overlayRequired || overlayPresent
		if activationErr == nil && overlayBuilt {
			activationErr = publishSemanticGeneration(indexDir, overlayStaging, overlaySource, false)
			overlayReady = activationErr == nil
		}
		// Both parts or neither: a node never reads its base without the
		// overlay that its dirty files need. With kept files no state binds.
		if activationErr == nil && baseReady && overlayReady && partial == nil {
			activationErr = bindSemanticGenerations(cacheDir, indexDir, preState, baseSource, overlaySource)
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
	cacheDir string,
	indexDir string,
	buildDir string,
	snapshot gitSnapshot,
	scan familyScan,
	parallelism int,
	arrival missingBlobArrival,
) (*committedGitState, []missingBlob, error) {
	// A delta reads only the changed paths, so a blob that arrived for an
	// unchanged path needs the full build.
	if !arrival.lexical {
		delta, eligible, err := prepareNativeGitDelta(
			ctx,
			repoDir,
			cacheDir,
			indexDir,
			snapshot,
			scan,
		)
		if err != nil {
			return nil, nil, err
		}
		if eligible {
			if err := indexNativeGitDelta(
				ctx,
				repoDir,
				buildDir,
				scan.paths(familyCommitted),
				delta,
			); err != nil {
				return nil, nil, err
			}
			// The seed keeps the name-only files of the base paths that the
			// delta did not change, so the record keeps their blobs.
			state := delta.nextState
			return &state, deltaMissingBlobs(arrival.lexicalCarry, delta.changedPaths, delta.targetInfos), nil
		}
	}
	var missing []missingBlob
	if err := indexNativeGitFull(ctx, repoDir, buildDir, snapshot, parallelism, &missing); err != nil {
		return nil, nil, err
	}
	buildScan, err := scanFamily(buildDir)
	if err != nil {
		return nil, nil, err
	}
	return &committedGitState{
		head:       snapshot.commitOID,
		baseHead:   snapshot.commitOID,
		baseShards: nativeCommittedShardCount(buildScan),
	}, missing, nil
}

// publishedCommittedHead returns the commit the published committed family
// holds, as the shards themselves record it. See committedShardHead for why the
// shards and not the sidecar answer this.
//
// It falls back to the .head sidecar when no shard can answer, which covers a
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
func committedSnapshotReady(cacheDir, indexDir string, state repoState, scan familyScan) bool {
	if state.HeadSHA == "no-head" {
		return !scan.hasMember(familyCommitted)
	}
	if scan.hasCommittedShard() {
		// Presence alone says nothing about which commit the shards hold.
		if shardHead, known := committedShardHead(scan); known {
			return shardHead == state.HeadSHA
		}
		// Unknown, so keep the answer this corpus already gives. Reporting
		// "stale" here would rebuild on every read of an unreadable shard.
		return true
	}
	return readHeadFile(cacheDir) == state.HeadSHA && scan.matchesManifestFamily(indexDir, familyCommitted)
}

// validateCommittedBuildHead distinguishes a real HEAD change from a failed
// validation. A changed HEAD discards the new build and keeps the live index.
// Cancellation and other Git failures propagate to the caller.
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
	present, err := familyHasShard(indexDir)
	return err == nil && present
}

// maxCommittedDeltaShards is the pre-admission threshold for shards added after
// the last committed full build. This shard check permits a delta when the
// existing added count equals this value, so that delta can take the family
// above the threshold. The next update selects a native full build. This value
// does not limit the full base shard count. It matches maxFolderDeltaShards.
const maxCommittedDeltaShards = 64

// fallbackGitRepositoryName returns the default opaque name for committed Git
// shards. Local zoekt.name config can replace it. It is not based on basename:
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
// The consumer releases the weight on an earlier error or after Finish and the
// shard guard:
//
//   - indexDocumentsWithRepository releases each window after Finish and the
//     shard guard.
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
// worker pool and sends them to out. Cloud-only files, files whose content
// does not fit the index, and files that start with binary content
// (readContent) are sent by name only. Skips symlinks and directories. A file
// removed after git status is skipped. A file that cannot be read for a
// lasting reason is sent by name only; another read error goes to failure
// (dirtyFailure.passing), as the folder reader does. The channel is closed
// after all workers exit.
//
// Each worker reserves readSemaphore weight before it opens a file. A
// successful send transfers release ownership to the consumer. The worker
// releases the weight on all other paths.
func readFilesToChannel(ctx context.Context, repoDir string, files []string, parallelism int, out chan<- fileContent, failure *dirtyFailure) {
	workers := fileReadWorkerCount(parallelism, len(files))
	ch := make(chan string, workers)
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				readOneDirtyFile(ctx, repoDir, f, out, failure)
			}
		}()
	}

	for _, f := range files {
		select {
		case ch <- f:
		case <-ctx.Done():
			// Match streamFolderFiles: stop feeding workers on cancellation so
			// the producer does not outlive the consumers.
			close(ch)
			wg.Wait()
			close(out)
			return
		}
	}
	close(ch)
	wg.Wait()
	close(out)
}

// readOneDirtyFile factors the per-file read so deferred Release of
// readSemaphore weight runs on every exit path (including panics inside
// readContent). The `released` sentinel transfers ownership to the
// consumer once the channel send succeeds.
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
	fi, err := dirtyLstat(srcPath)
	if err != nil {
		failed(err)
		return
	}

	// Only process regular files — skip directories (dirty
	// submodules), symlinks, FIFOs, sockets, and devices to
	// avoid blocking or reading unexpected data.
	if !fi.Mode().IsRegular() {
		return
	}

	// A cloud-only file is never opened: the open would download it.
	var st unix.Stat_t
	if unix.Lstat(srcPath, &st) == nil && fileIsDataless(f, &st) {
		sendNameOnly(ctx, out, f, seekNoBytesReason)
		return
	}

	size := fi.Size()
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

	// Transfer Release ownership to the consumer. The select unblocks
	// on ctx cancel so an abandoned consumer cannot wedge the worker
	// holding semaphore weight indefinitely. released stays false on
	// the ctx.Done path so the deferred sentinel returns the weight.
	select {
	case out <- fileContent{name: f, content: buf[:n], weight: weight}:
		released = true
	case <-ctx.Done():
		return
	}
}

// streamFiles returns a channel that yields file contents read from
// the working tree. The output channel is unbuffered because Zoekt's
// public Builder API accepts []byte documents, not io.Reader streams.
// In-flight memory is bounded by two limits, whichever is tighter:
//   - the worker count (each worker holds at most one fileContent),
//   - readSemaphore's readBudgetBytes weight budget (see caps.go).
//
// Each yielded fileContent carries its reserved readSemaphore weight. The
// consumer releases it after Builder.Finish returns. A caller must drain the
// channel or cancel ctx.
func streamFiles(ctx context.Context, repoDir string, files []string, parallelism int) <-chan fileContent {
	return streamDirtyFiles(ctx, repoDir, files, parallelism, nil)
}

// streamDirtyFiles is streamFiles with failure, which decides what a read
// error that may pass does (dirtyFailure).
func streamDirtyFiles(ctx context.Context, repoDir string, files []string, parallelism int, failure *dirtyFailure) <-chan fileContent {
	out := make(chan fileContent)
	go readFilesToChannel(ctx, repoDir, files, parallelism, out, failure)
	return out
}

// dirtyFailure decides what a read error that may pass (EIO, EMFILE, ESTALE:
// folderReadError) does to a dirty file. The text index keeps the file by name
// and notes the error: the build applies every other change, and the caller
// returns the error after the publish, so no state is written and the next
// search reads the file again. The vector build stops instead (stop): a
// generation bound to the state must hold every file. A nil failure skips the
// file.
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
// name: it stops the build, or sends the file by name only and notes err.
func (failure *dirtyFailure) passing(ctx context.Context, out chan<- fileContent, name string, err error) {
	switch {
	case failure == nil:
	case !failure.keep || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE):
		if failure.stop != nil {
			failure.stop(err)
		}
	default:
		failure.mu.Lock()
		if failure.err == nil {
			failure.err = err
		}
		failure.kept = append(failure.kept, name)
		failure.mu.Unlock()
		slog.Debug("Indexing an uncommitted file by name only after a read error", "path", name, "error", err)
		sendNameOnly(ctx, out, name, seekNoBytesReason)
	}
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
// read error that may pass. The build published everything else, the state
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

// keptEntrySize marks a manifest entry of a file kept by name: no stat has
// it, so the next diff reads the file again, or tombstones it when it left
// the dirty set.
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

// dirtyOpenFile opens a dirty file for reading, and dirtyLstat stats it;
// tests replace them to inject errors.
var (
	dirtyOpenFile = os.Open
	dirtyLstat    = os.Lstat
)

func fileReadWorkerCount(parallelism, items int) int {
	if items <= 0 {
		return 0
	}
	if parallelism < 1 {
		return 1
	}
	if parallelism > items {
		return items
	}
	return parallelism
}

// indexUncommitted indexes the working-tree dirty files for repoDir into the
// "uncommitted" Zoekt repo.
//
// State binding: the manifest is written tagged with preState (the freshly
// computed state hash for this cycle). On the next cycle,
// runIndexingWithCacheExecution reads .state, which holds preState, and passes
// it back as cachedState. tryUncommittedDelta then loads the manifest by
// matching expectedState == cachedState. The folder indexer uses the same
// state binding.
//
// A stage built before the publish lock (prebuilt) replaces the whole family.
// Otherwise, when a prior manifest is present and the family has at most
// maxUncommittedDeltaShards shards (any count when only kept files changed),
// only files whose size, mtime, inode, mode, or ctime changed are read again
// and written into a delta shard; vanished files become tombstones in the
// .meta sidecars of older shards. It rebuilds in full when the manifest is
// missing or stale, the shard count is above the limit, or the delta fails
// for a reason other than a read error (folderReadError).
func indexUncommitted(
	ctx context.Context,
	repoDir, indexDir, cacheDir string,
	state repoState,
	cachedState, preState string,
	parallelism int,
	prebuilt *uncommittedStage,
	checked []uncommittedCandidate,
) error {
	if len(state.Files) == 0 {
		cleanUncommittedShards(indexDir)
		deleteUncommittedManifest(cacheDir)
		return nil
	}
	if prebuilt != nil {
		return prebuilt.publish(indexDir, cacheDir, preState)
	}

	candidates := checked
	if candidates == nil {
		candidates = statUncommittedCandidates(repoDir, state.Files)
	}
	entries := uncommittedManifestEntries(candidates)

	// A manifest of the cached state, or a pending one when no state was
	// written.
	if manifest, ok := readUncommittedManifest(cacheDir, cachedState); ok {
		cleanEmptyShards(ctx, indexDir, repoUncommitted)
		existing := repositoryShardFiles(indexDir, repoUncommitted)
		shardCount := len(existing)
		// The uncommitted family renumbers exactly like the committed one: a
		// delta onto a gapped family writes over the survivors above the gap
		// and makes dirty content permanently unsearchable. Fall through to the
		// full rebuild instead, which clears the family first.
		if shardCount > 0 && !pathsAreContiguous(existing) {
			slog.Warn("Uncommitted shard family has a numbering gap; rebuilding it in full",
				"index_dir", indexDir)
			shardCount = 0
		}
		_, changed := diffUncommittedAgainstManifest(candidates, manifest)
		if shardCount > 0 && len(changed) == 0 {
			// No dirty file changed (a HEAD change, a vector build): the
			// family stays, whatever its shard count.
			if err := writeUncommittedManifest(cacheDir, preState, entries); err != nil {
				slog.Debug("Failed to refresh uncommitted manifest", "error", err)
			}
			return nil
		}
		if shardCount > 0 && (shardCount <= maxUncommittedDeltaShards || onlyKept(changed, manifest)) {
			if err := tryUncommittedDelta(ctx, repoDir, indexDir, cacheDir, candidates, cachedState, preState, entries); err == nil {
				return nil
			} else if _, ok := errors.AsType[*folderReadError](err); ok {
				// The delta kept some files by name and published the rest
				// (dirtyFilesKept), or EMFILE stopped it before any write:
				// a full rebuild would read the same files.
				return err
			} else if errors.Is(err, errDeltaPayloadExceedsWindow) {
				slog.Debug("Uncommitted delta payload exceeds window threshold; full rebuild", "error", err)
			} else {
				slog.Debug("Uncommitted delta indexing failed, falling back to full rebuild", "error", err)
			}
		} else if shardCount > maxUncommittedDeltaShards {
			slog.Debug("Uncommitted delta shard limit reached, falling back to full rebuild", "shards", shardCount)
		}
	}

	// The delta fell back (a large payload, removals only): build in full
	// under the lock, as the prebuild does outside it.
	stage, err := buildUncommittedStage(ctx, repoDir, indexDir, state, parallelism)
	if err != nil {
		return err
	}
	defer stage.discard()
	return stage.publish(indexDir, cacheDir, preState)
}

// uncommittedDeltaPossible reports whether the live uncommitted family can
// take a delta: a manifest of the cached state, a contiguous family of at most
// maxUncommittedDeltaShards shards (any count when only kept files changed),
// and changed files to read that fit one index window. It also returns the
// stats of the dirty files. It writes nothing, so a build can ask before the
// publish lock (the build lock keeps other writers out).
func uncommittedDeltaPossible(ctx context.Context, repoDir, indexDir, cacheDir, cachedState string, files []string) (bool, []uncommittedCandidate) {
	manifest, ok := readUncommittedManifest(cacheDir, cachedState)
	if !ok {
		return false, nil
	}
	existing := repositoryShardFiles(indexDir, repoUncommitted)
	if len(existing) == 0 || !pathsAreContiguous(existing) {
		return false, nil
	}
	candidates := statUncommittedCandidates(repoDir, files)
	toRead, changed := diffUncommittedAgainstManifest(candidates, manifest)
	if len(changed) == 0 {
		return true, candidates // the locked part only refreshes the manifest
	}
	// The delta falls back to a full rebuild for these: build it before the
	// lock instead. Zoekt rejects a delta of removals only.
	if (len(existing) > maxUncommittedDeltaShards && !onlyKept(changed, manifest)) || len(toRead) == 0 {
		return false, candidates
	}
	var payload int64
	for _, candidate := range toRead {
		if payload += candidate.size; payload >= indexWindow(ctx) {
			return false, candidates
		}
	}
	return true, candidates
}

// uncommittedStage is a full uncommitted family built in a build directory
// (the gc sweep removes one that a crash leaves).
type uncommittedStage struct {
	dir     string
	failure *dirtyFailure
	entries []uncommittedManifestEntry
}

// buildUncommittedStage builds the full uncommitted family of state in a new
// build directory. A file with a read error that may pass is kept by name
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
	fileCh := streamDirtyFiles(readCtx, repoDir, state.Files, parallelism, stage.failure)
	_, err = indexDocuments(readCtx, dir, repoUncommitted, repoDir, fileCh, parallelism)
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
// lock: a gapped old family leaves no stale shard beside the new one. A move
// that fails part way leaves a partial family and no state, so the next
// search rebuilds it. A file kept by name after a read error leaves the state
// unwritten.
func (stage *uncommittedStage) publish(indexDir, cacheDir, preState string) error {
	cleanUncommittedShards(indexDir)
	if err := moveRepositoryShards(stage.dir, indexDir, repoUncommitted); err != nil {
		deleteUncommittedManifest(cacheDir)
		return err
	}
	if err := stage.failure.noted(); err != nil {
		writePendingUncommittedManifest(cacheDir, stage.entries, err)
		return err
	}
	if err := writeUncommittedManifest(cacheDir, preState, stage.entries); err != nil {
		slog.Debug("Failed to write uncommitted manifest", "error", err)
	}
	return nil
}

func (stage *uncommittedStage) discard() {
	discardBuildDir(stage.dir)
}

// tryUncommittedDelta attempts a delta-only rebuild of the uncommitted shard
// set. The manifest is looked up by cachedState (the .state value persisted
// at the end of the previous cycle) and re-written tagged with preState (the
// new .state value about to be persisted by runIndexingWithCacheExecution).
func tryUncommittedDelta(
	ctx context.Context,
	repoDir, indexDir, cacheDir string,
	candidates []uncommittedCandidate,
	cachedState, preState string,
	entries []uncommittedManifestEntry,
) error {
	manifest, ok := readUncommittedManifest(cacheDir, cachedState)
	if !ok {
		return fmt.Errorf("uncommitted manifest missing or stale")
	}
	toRead, changedPaths := diffUncommittedAgainstManifest(candidates, manifest)
	if len(changedPaths) == 0 {
		// No file content drifted since the manifest was written — the
		// state hash must have changed for some other reason (HEAD,
		// untracked file added then removed, etc.). Refresh the manifest
		// binding without touching shards.
		if err := writeUncommittedManifest(cacheDir, preState, entries); err != nil {
			slog.Debug("Failed to refresh uncommitted manifest", "error", err)
		}
		return nil
	}

	docs, noted, err := readUncommittedCandidates(ctx, repoDir, toRead)
	if err != nil {
		// readUncommittedCandidates returns (nil, errDeltaPayloadExceedsWindow)
		// after releasing all weights internally — no further work here.
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A cancel cut the reads short: write nothing.
		releaseFileContentWeights(docs)
		return ctxErr
	}
	if kept, ok := errors.AsType[*dirtyFilesKept](noted); ok && onlyKept(changedPaths, manifest) && len(kept.files) == len(changedPaths) {
		// Every changed file is a kept file that failed again: the family
		// holds it by name already, so write no shard.
		releaseFileContentWeights(docs)
		writePendingUncommittedManifest(cacheDir, entries, noted)
		return noted
	}
	if len(docs) == 0 && len(changedPaths) > 0 {
		// Pure tombstone cycle (all changes are removals). Zoekt's
		// builder rejects empty delta builds with the same error
		// shape as the folder side; surface that as a fallback signal.
		// docs is empty here; no weights to release.
		return fmt.Errorf("uncommitted delta contains only removals")
	}
	// indexDeltaDocuments releases all per-document readSemaphore weights;
	// do not release them here.
	if _, err := indexDeltaDocuments(indexDir, repoUncommitted, repoDir, docs, uncommittedDeltaShardMax, changedPaths); err != nil {
		return err
	}
	if noted != nil {
		// The delta applied every change and kept some files by name: no
		// state, and a pending manifest that marks them.
		writePendingUncommittedManifest(cacheDir, entries, noted)
		return noted
	}
	if err := writeUncommittedManifest(cacheDir, preState, entries); err != nil {
		slog.Debug("Failed to write uncommitted manifest", "error", err)
	}
	return nil
}

// readUncommittedCandidates loads file contents for the given
// candidates from the working tree. Missing or non-regular files are
// skipped silently, and unreadable ones are sent by name only (same policy as
// readFilesToChannel). The second result names the files kept by name after a
// read error that may pass (dirtyFailure.noted). EMFILE or ENFILE returns a
// folderReadError and no documents.
//
// Two guards prevent a synchronous drain >= the build's window from
// wedging the global readSemaphore:
//   - Pre-flight: sum candidate.size (lstat already populated). If
//     it already exceeds the window threshold, return immediately
//     without spawning any reader — saves up to one window of
//     wasted read I/O.
//   - Bounded drain: if cumulative weight reaches the threshold
//     mid-drain, cancel the inner context so workers unwind via
//     Acquire's ctx-aware abort, then drain-and-release the remainder.
//
// Both paths return errDeltaPayloadExceedsWindow. Caller
// indexUncommitted catches via errors.Is and falls back to the
// windowed full rebuild (buildUncommittedStage).
func readUncommittedCandidates(ctx context.Context, repoDir string, candidates []uncommittedCandidate) ([]fileContent, error, error) {
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	var preSum int64
	for _, c := range candidates {
		preSum += c.size
		if preSum >= indexWindow(ctx) {
			return nil, nil, errDeltaPayloadExceedsWindow
		}
	}
	files := make([]string, len(candidates))
	for i, c := range candidates {
		files[i] = c.name
	}
	innerCtx, cancelInner := context.WithCancelCause(ctx)
	defer cancelInner(nil)
	failure := &dirtyFailure{keep: true, stop: cancelInner}
	ch := streamDirtyFiles(innerCtx, repoDir, files, fileReadWorkerCount(indexParallelism(), len(files)), failure)
	docs := make([]fileContent, 0, len(files))
	var cumulative int64
	exceeded := false
	for doc := range ch {
		if exceeded {
			// Drain the rest releasing inline so workers (still holding
			// weight on Acquired-but-not-yet-cancelled reads) and the
			// producer goroutine can exit cleanly.
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

// indexDocumentsWithRepository consumes fileContent through rotating Builder
// windows. It calls Finish when a normal window reaches indexWindow(ctx).
// Documents that fill a shard of their own (isolatesShard) use separate
// windows and a per-build concurrency limit (largeDocumentParallelism);
// guardWrappedShards checks their shards after Finish. This scheduling rule
// does not change content or symbol indexing. It releases Builder-owned
// document weight after Finish and the shard guard, and releases documents
// that no Builder accepted as soon as their error path ends. See fileContent
// for ownership.
//
// Window 0 uses IsDelta=false, so Finish prunes stale shards. Later windows use
// IsDelta=true with no changed paths, so they skip deletion and tombstone work
// and continue shard numbering. indexed is true after a Builder opens.
func indexDocumentsWithRepository(
	ctx context.Context,
	indexDir string,
	repository zoekt.Repository,
	fileCh <-chan fileContent,
	parallelism int,
	cancelProducer context.CancelFunc,
) (bool, error) {
	// A large window rotates at largeParallelism documents, so one build runs
	// at most that many large shard jobs. No token is shared between builds: a
	// consumer that waits for another build's token holds its document, while
	// that build can wait for readers that the read budget blocks.
	largeParallelism := min(max(parallelism, 1), largeDocumentParallelism)
	window := indexWindow(ctx)
	var current *index.Builder
	var pendingWeight int64
	// pendingContent counts real content bytes; readWeight stops counting at
	// maxReadWeightBytes, so large windows also rotate on it.
	var pendingContent int64
	var addErr error
	indexedAny := false
	openedWindows := 0
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
		b, err := index.NewBuilder(opts)
		if err != nil {
			return fmt.Errorf("create builder: %w", err)
		}
		current = b
		currentLarge = large
		currentOpts = opts
		currentFirstShard = firstShard
		largeDocuments = 0
		openedWindows++
		indexedAny = true
		return nil
	}

	// finishWindow Finishes the current builder and Releases pending
	// weight. Always Releases even on Finish error: Finish's
	// b.building.Wait() has joined the shard goroutines (or aborted
	// them via b.buildError) before returning, so no goroutine retains
	// any doc.Content past this point.
	finishWindow := func() error {
		if current == nil {
			return nil
		}
		err := current.Finish()
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
		current = nil
		currentLarge = false
		largeDocuments = 0
		return err
	}

	drainRemaining := func() {
		for d := range fileCh {
			if d.weight > 0 {
				readSemaphore.Release(d.weight)
			}
		}
	}

	for doc := range fileCh {
		if err := ctx.Err(); err != nil {
			if cancelProducer != nil {
				cancelProducer()
			}
			if doc.weight > 0 {
				readSemaphore.Release(doc.weight)
			}
			drainRemaining()
			_ = finishWindow()
			return indexedAny, err
		}
		if addErr != nil {
			if doc.weight > 0 {
				readSemaphore.Release(doc.weight)
			}
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
				if cancelProducer != nil {
					cancelProducer()
				}
				if doc.weight > 0 {
					readSemaphore.Release(doc.weight)
				}
				drainRemaining()
				return indexedAny, err
			}
		}
		if current == nil {
			if err := openWindow(openedWindows > 0, large); err != nil {
				if cancelProducer != nil {
					cancelProducer()
				}
				if doc.weight > 0 {
					readSemaphore.Release(doc.weight)
				}
				drainRemaining()
				return indexedAny, err
			}
		}

		// Quality invariant: file size changes scheduling only. Do not set
		// Document.Symbols to a non-nil empty slice or disable ctags for a large
		// document. Either action would make sym: results depend on file size.
		// Zoekt's Builder.Add buffers doc into b.todo before any error return, so
		// doc.weight stays the window's responsibility either way and is Released
		// by finishWindow.
		pendingWeight += doc.weight
		pendingContent += int64(len(doc.content))
		if currentLarge {
			largeWindowDocs = append(largeWindowDocs, document)
		}
		if err := current.Add(document); err != nil {
			addErr = fmt.Errorf("add document %s: %w", doc.name, err)
			if cancelProducer != nil {
				cancelProducer()
			}
			continue
		}
		if large {
			largeDocuments++
		}

		if pendingWeight >= window ||
			(currentLarge && (largeDocuments >= largeParallelism || pendingContent >= window)) {
			if err := finishWindow(); err != nil {
				if cancelProducer != nil {
					cancelProducer()
				}
				drainRemaining()
				return indexedAny, err
			}
		}
	}

	if current != nil {
		if err := finishWindow(); err != nil {
			if cancelProducer != nil {
				cancelProducer()
			}
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
// that kept some files by name (noted): no state, and a kept mark on those
// files, so the next search's delta reads them again.
func writePendingUncommittedManifest(cacheDir string, entries []uncommittedManifestEntry, noted error) {
	kept, ok := errors.AsType[*dirtyFilesKept](noted)
	if !ok {
		deleteUncommittedManifest(cacheDir)
		return
	}
	if err := writeUncommittedManifest(cacheDir, "", markKept(entries, kept.files)); err != nil {
		deleteUncommittedManifest(cacheDir)
	}
}

// moveRepositoryShards moves the shards of repoName from stage into indexDir.
func moveRepositoryShards(stage, indexDir, repoName string) error {
	for _, shard := range repositoryShardFiles(stage, repoName) {
		if err := os.Rename(shard, filepath.Join(indexDir, filepath.Base(shard))); err != nil {
			return fmt.Errorf("publish uncommitted shard: %w", err)
		}
	}
	return nil
}

// cleanUncommittedShards removes stale uncommitted shard files.
func cleanUncommittedShards(indexDir string) {
	cleanRepositoryShards(indexDir, repoUncommitted)
}

func cleanRepositoryShards(indexDir, repoName string) {
	for _, m := range repositoryShardFiles(indexDir, repoName) {
		_ = os.Remove(m)
	}
	metas, _ := filepath.Glob(filepath.Join(indexDir, repoName+"_v*.zoekt.meta"))
	for _, m := range metas {
		_ = os.Remove(m)
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
