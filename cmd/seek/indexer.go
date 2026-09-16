package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/cespare/xxhash/v2"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
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
	// shardMax is the maximum corpus size (in bytes) per zoekt shard.
	// Smaller shards let Zoekt build more shards concurrently.
	shardMax = 10 * 1024 * 1024 // 10 MB
	// largeDocumentParallelism bounds concurrent shard and ctags jobs when one
	// document is larger than a normal shard. Seek does not reduce index quality
	// for these files: it still indexes full content and runs symbol analysis.
	largeDocumentParallelism = 3
)

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
// size, and inode of each dirty file. Git status porcelain does not include
// working-tree content hashes, so these fields detect common changes to files
// that are already dirty. This is a metadata fingerprint, not a content hash;
// an edit that preserves every observed field can keep the same fingerprint.
//
// Called twice per indexing cycle: once before indexing (to compute the
// pre-state hash) and once after (to detect drift). The second call
// reads the same metadata again so changes to an observed field cause the
// build to be discarded.
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
// a failed or drifted indexing cycle re-checks the corpus on the next search.
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
	// compares it with the commit read from the shard itself
	// (git_native_delta.go:124-127) and rejects a delta when the two disagree.
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
	return index.Options{
		IndexDir:         indexDir,
		SizeMax:          maxIndexedDocumentBytes,
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
		if versionErr != nil {
			versionErr = fmt.Errorf("check %q --version: %w", ctags, versionErr)
		} else {
			versionErr = fmt.Errorf("%q is not Universal Ctags", ctags)
		}
		return &ctagsUnavailableError{cause: errors.Join(universalErr, versionErr)}
	}
}

// runIndexingWithCacheExecution makes the joined Git index current with
// separate build and publish locks. Zoekt covers committed and uncommitted
// files. The semantic generation covers the captured commit:
//
//   - .build.lock serializes builders for the complete build.
//   - The committed family is built outside the publish lock. Native full uses
//     an empty temporary directory; eligible native delta seeds current shards.
//     These are the only committed-data routes. HEAD is checked before publish.
//   - A required semantic generation builds beside the committed Zoekt branch.
//     Each branch reads its own bounded stream from the same captured commit.
//   - The uncommitted family is rebuilt under the publish lock so its shards,
//     manifest, and state describe the same generation.
//   - The joined descriptor is written last under the publish lock. A semantic
//     failure keeps the Zoekt generation searchable without joined retrieval.
//
// Lexical readers normally hold a shared publish lock while they list, open,
// and search shards. A timed-out lexical read can continue without the lock.
// Joined readers require the strict shared lock because both indexes must name
// the same generation. A failed swap can leave shards until the next build
// repairs .swapping.
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
	semanticRequired := execution.vectorsWanted(state.HeadSHA)
	joinedReady := !semanticRequired || joinedGenerationMatches(
		cacheDir,
		indexDir,
		preState,
		semanticSource,
	)
	if cachedState == preState && !noHeadArtifacts && committedPresent && familyIntact && joinedReady {
		return nil
	}

	parallelism := execution.resources.cpuLimit()

	if err := checkGitDirtyFileBudget(repoDir, indexDir, state.Files); err != nil {
		deleteStateFiles(cacheDir)
		return err
	}

	hasDirty := len(state.Files) > 0
	// Rebuild when the published shards hold another commit, or the committed
	// family no longer matches its manifest. publishedCommittedHead asks the
	// shards rather than the sidecar; committedShardHead documents why.
	needCommitted := state.HeadSHA != "no-head" &&
		(publishedCommittedHead(cacheDir, sc) != state.HeadSHA || !committedIntact)
	clearCommitted := noHeadArtifacts
	semanticPresent := semanticRequired && semanticGenerationPresent(indexDir, semanticSource)
	needSemantic := semanticRequired && !semanticPresent

	// Build Zoekt and semantic data together outside the publish lock. Each
	// branch reads the same captured commit through its own bounded Git stream.
	var buildDir string
	var nextCommittedState *committedGitState
	if needCommitted || clearCommitted || needSemantic {
		buildDir, err = newBuildDir(indexDir)
		if err != nil {
			return err
		}
		defer discardBuildDir(buildDir)
	}
	var snapshot gitSnapshot
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
	var lexicalBuild func()
	if needCommitted {
		lexicalBuild = func() {
			nextCommittedState, lexicalErr = buildCommittedGitStage(
				ctx,
				repoDir,
				cacheDir,
				indexDir,
				buildDir,
				snapshot,
				sc,
				parallelism,
			)
		}
	}
	var semanticBuild func()
	if needSemantic {
		semanticBuild = func() {
			semanticErr = runSemanticBuild(ctx, execution.model, func(embedder semanticModel) error {
				_, err := buildSemanticGitGeneration(
					ctx,
					repoDir,
					snapshot,
					nil,
					semanticStaging,
					semanticSource,
					embedder,
					execution.resources,
				)
				return err
			})
			semanticBuilt = semanticErr == nil
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

	// Keep the committed publish, uncommitted build, and state files under one
	// exclusive publish lock. Normal locked readers cannot interleave with these
	// writes. The state and HEAD writes below must remain inside this lock.
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
		(needCommitted || clearCommitted || hasDirty) {
		// The old descriptor must not name a family while that family changes.
		removeJoinedGeneration(cacheDir)
	}

	if needCommitted || clearCommitted {
		committedNames, pErr := publishShardFamilyLocked(cacheDir, indexDir, buildDir, familyCommitted)
		if pErr != nil {
			return fmt.Errorf("publish committed shards: %w", pErr)
		}
		publishedNames = committedNames
		publishedCommitted = true
	}

	if hasDirty {
		uncommittedErr := gitCorpusError(
			repoDir,
			indexDir,
			indexUncommitted(ctx, repoDir, indexDir, cacheDir, state, cachedState, preState, parallelism),
		)
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

	// Read Git state again. Reusing the pre-build clean state cannot detect a
	// file that became dirty while both branches ran.
	postRepoState, postStateErr := gitRepoStateIn(ctx, repoDir)
	if postStateErr != nil {
		deleteStateFiles(cacheDir)
		return postStateErr
	}
	postState := gitCorpusStateHash(paths, postRepoState)

	if postState == preState {
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
		} else if clearCommitted {
			deleteCommittedGitState(cacheDir)
		}
		if err := writeStateFile(cacheDir, preState); err != nil {
			deleteStateFiles(cacheDir)
			return fmt.Errorf("write state file: %w", err)
		}
		if finalHasShard {
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
		if semanticRequired {
			var activationErr error
			if semanticBuilt {
				activationErr = publishAndBindSemanticGeneration(
					cacheDir, indexDir, preState, semanticSource, semanticStaging,
				)
			} else if semanticPresent {
				activationErr = bindSemanticGeneration(cacheDir, indexDir, preState, semanticSource)
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
	} else {
		// The working tree moved during the build, so the state and the
		// uncommitted shards cannot be trusted. The committed vectors can:
		// validateCommittedBuildHead already proved HEAD held, the generation is
		// keyed by that commit, and it never reads the working tree. Publish it
		// unbound so the next clean search binds it instead of embedding the
		// same commit again. It stays unreachable until something binds it.
		if semanticRequired && semanticBuilt {
			keepDriftedSemanticGeneration(cacheDir, indexDir, semanticSource, semanticStaging)
		}
		deleteStateFiles(cacheDir)
		slog.Warn("Index may be stale, will re-index on next search")
	}

	return nil
}

func buildCommittedGitStage(
	ctx context.Context,
	repoDir string,
	cacheDir string,
	indexDir string,
	buildDir string,
	snapshot gitSnapshot,
	scan familyScan,
	parallelism int,
) (*committedGitState, error) {
	delta, eligible, err := prepareNativeGitDelta(
		ctx,
		repoDir,
		cacheDir,
		indexDir,
		snapshot,
		scan,
	)
	if err != nil {
		return nil, err
	}
	if eligible {
		if err := indexNativeGitDelta(
			ctx,
			repoDir,
			buildDir,
			scan.paths(familyCommitted),
			delta,
		); err != nil {
			return nil, err
		}
		state := delta.nextState
		return &state, nil
	}
	budget, err := indexNativeGitFullWithBudget(
		ctx,
		repoDir,
		buildDir,
		snapshot,
		nil,
		parallelism,
	)
	if err != nil {
		return nil, err
	}
	buildScan, err := scanFamily(buildDir)
	if err != nil {
		return nil, err
	}
	return &committedGitState{
		head:       snapshot.commitOID,
		baseHead:   snapshot.commitOID,
		budget:     budget,
		baseShards: nativeCommittedShardCount(buildScan),
	}, nil
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

// keepDriftedSemanticGeneration publishes a finished committed generation that
// a drifted build would otherwise discard. A failure here costs only the
// rebuild it was trying to save, so it never fails the search.
func keepDriftedSemanticGeneration(cacheDir, indexDir, source, stagingDir string) {
	if err := publishUnboundSemanticGeneration(cacheDir, indexDir, source, stagingDir); err != nil {
		slog.Debug("Could not keep the semantic generation after drift", "error", err)
	}
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
		// This used to return true on presence alone, which said nothing about
		// which commit the shards hold.
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
// Cancellation and other Git failures propagate to the corpus worker.
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
// weight is the byte count reserved from readSemaphore. A successful producer
// call transfers ownership to the consumer. If a Builder accepts the content,
// Zoekt can keep it until Finish returns. The consumer releases the weight on
// an earlier error or after Finish:
//
//   - indexDocumentsWithRepository releases each window after Finish.
//   - indexDeltaDocumentsWithRepository releases the set before return and,
//     after Builder creation, only after Finish.
//
// Zero weight means that a synchronous folder-delta read did not use the
// semaphore. Releasing zero has no effect.
type fileContent struct {
	name       string
	content    []byte
	weight     int64
	branches   []string
	skipReason index.SkipReason
}

// readFilesToChannel reads files from the working tree using a bounded
// worker pool and sends them to out. Skips files larger than
// maxGitDirtyFileSize, symlinks, and directories. Individual file
// failures are non-fatal (files may be deleted or modified between
// git status and read). The channel is closed after all workers exit.
//
// Each worker reserves readSemaphore weight before it opens a file. A
// successful send transfers release ownership to the consumer. The worker
// releases the weight on all other paths.
func readFilesToChannel(ctx context.Context, repoDir string, files []string, parallelism int, out chan<- fileContent) {
	workers := fileReadWorkerCount(parallelism, len(files))
	ch := make(chan string, workers)
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				readOneDirtyFile(ctx, repoDir, f, out)
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
// io.ReadFull). The `released` sentinel transfers ownership to the
// consumer once the channel send succeeds.
func readOneDirtyFile(ctx context.Context, repoDir, f string, out chan<- fileContent) {
	srcPath := filepath.Join(repoDir, f)

	// Use Lstat to avoid following symlinks
	fi, err := os.Lstat(srcPath)
	if err != nil {
		return
	}

	// Only process regular files — skip directories (dirty
	// submodules), symlinks, FIFOs, sockets, and devices to
	// avoid blocking or reading unexpected data.
	if !fi.Mode().IsRegular() {
		return
	}

	size := fi.Size()
	if size > maxGitDirtyFileSize {
		slog.Warn("Skipping large uncommitted file", "path", f, "size_mb", size/(1024*1024))
		return
	}
	weight := size
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
	// Fstat that os.ReadFile performs internally. A single
	// Read is not guaranteed to fill the buffer, even for
	// regular files, so use ReadFull and keep partial content
	// only when the file shrank during the read.
	fh, err := os.Open(srcPath)
	if err != nil {
		return
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(fh, buf)
	_ = fh.Close()
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return
	}
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
//   - readSemaphore's maxInFlightBytes byte budget (see caps.go).
//
// Each yielded fileContent carries its reserved readSemaphore weight. The
// consumer releases it after Builder.Finish returns. A caller must drain the
// channel or cancel ctx.
func streamFiles(ctx context.Context, repoDir string, files []string, parallelism int) <-chan fileContent {
	out := make(chan fileContent)
	go readFilesToChannel(ctx, repoDir, files, parallelism, out)
	return out
}

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
// computed state hash for this cycle). On the next cycle, runIndexingWithCache
// reads .state — which now contains preState — and passes it back as
// cachedState. tryUncommittedDelta then loads the manifest by matching
// expectedState == cachedState. The folder indexer uses the same state binding.
//
// When a prior manifest is present and the existing shard count is below
// maxUncommittedDeltaShards, only files whose (size, mtime, ino) changed are
// re-read and written into a delta shard; vanished files become tombstones in
// the .meta sidecars of older shards. Falls back to a full rebuild when the
// manifest is missing/stale, the shard count exceeds the cap, or the delta
// path returns an error.
func indexUncommitted(
	ctx context.Context,
	repoDir, indexDir, cacheDir string,
	state repoState,
	cachedState, preState string,
	parallelism int,
) error {
	if len(state.Files) == 0 {
		cleanUncommittedShards(indexDir)
		deleteUncommittedManifest(cacheDir)
		return nil
	}

	candidates := statUncommittedCandidates(repoDir, state.Files)
	entries := make([]uncommittedManifestEntry, 0, len(candidates))
	for _, c := range candidates {
		entries = append(entries, c.manifestEntry())
	}

	if cachedState != "" {
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
		if shardCount > 0 && shardCount <= maxUncommittedDeltaShards {
			if err := tryUncommittedDelta(ctx, repoDir, indexDir, cacheDir, candidates, cachedState, preState, entries); err == nil {
				return nil
			} else if errors.Is(err, errDeltaPayloadExceedsWindow) {
				slog.Debug("Uncommitted delta payload exceeds window threshold; full rebuild", "error", err)
			} else {
				slog.Debug("Uncommitted delta indexing failed, falling back to full rebuild", "error", err)
			}
		} else if shardCount > maxUncommittedDeltaShards {
			slog.Debug("Uncommitted delta shard limit reached, falling back to full rebuild", "shards", shardCount)
		}
	}

	// Full rebuild: clear the old uncommitted family first. indexDocuments
	// builds in place and its first window is non-delta, so zoekt's cleanup
	// only reaches the contiguous prefix — a gapped family would leave stale
	// shards live beside the freshly written ones.
	cleanUncommittedShards(indexDir)
	fileCh := streamFiles(ctx, repoDir, state.Files, parallelism)
	_, err := indexDocuments(ctx, indexDir, repoUncommitted, repoDir, fileCh, parallelism)
	if err != nil {
		deleteUncommittedManifest(cacheDir)
		return err
	}
	if err := writeUncommittedManifest(cacheDir, preState, entries); err != nil {
		slog.Debug("Failed to write uncommitted manifest", "error", err)
	}
	return nil
}

// tryUncommittedDelta attempts a delta-only rebuild of the uncommitted shard
// set. The manifest is looked up by cachedState (the .state value persisted
// at the end of the previous cycle) and re-written tagged with preState (the
// new .state value about to be persisted by runIndexingWithCache).
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

	docs, err := readUncommittedCandidates(ctx, repoDir, toRead)
	if err != nil {
		// readUncommittedCandidates returns (nil, errDeltaPayloadExceedsWindow)
		// after releasing all weights internally — no further work here.
		return err
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
	if err := writeUncommittedManifest(cacheDir, preState, entries); err != nil {
		slog.Debug("Failed to write uncommitted manifest", "error", err)
	}
	return nil
}

// readUncommittedCandidates loads file contents for the given
// candidates from the working tree. Missing or non-regular files are
// skipped silently (same policy as readFilesToChannel).
//
// Two guards prevent a synchronous drain >= indexWindowBytes from
// wedging the global readSemaphore:
//   - Pre-flight: sum candidate.size (lstat already populated). If
//     it already exceeds the window threshold, return immediately
//     without spawning any reader — saves up to indexWindowBytes of
//     wasted read I/O.
//   - Bounded drain: if cumulative weight reaches the threshold
//     mid-drain, cancel the inner context so workers unwind via
//     Acquire's ctx-aware abort, then drain-and-release the remainder.
//
// Both paths return errDeltaPayloadExceedsWindow. Caller
// indexUncommitted catches via errors.Is and falls back to the
// windowed full rebuild (streamFiles → indexDocuments).
func readUncommittedCandidates(ctx context.Context, repoDir string, candidates []uncommittedCandidate) ([]fileContent, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	var preSum int64
	for _, c := range candidates {
		preSum += c.size
		if preSum >= indexWindowBytes {
			return nil, errDeltaPayloadExceedsWindow
		}
	}
	files := make([]string, len(candidates))
	for i, c := range candidates {
		files[i] = c.name
	}
	innerCtx, cancelInner := context.WithCancel(ctx)
	defer cancelInner()
	ch := streamFiles(innerCtx, repoDir, files, fileReadWorkerCount(indexParallelism(), len(files)))
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
		if cumulative >= indexWindowBytes {
			exceeded = true
			cancelInner()
		}
	}
	if exceeded {
		releaseFileContentWeights(docs)
		return nil, errDeltaPayloadExceedsWindow
	}
	return docs, nil
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
// windows. It calls Finish when a normal window reaches indexWindowBytes.
// Documents larger than shardMax use separate windows and a global concurrency
// limit. This scheduling rule does not change content or symbol indexing. It
// releases Builder-owned document weight after Finish and releases documents
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
	largeParallelism := min(max(parallelism, 1), largeDocumentParallelism)
	var current *index.Builder
	var pendingWeight int64
	var addErr error
	indexedAny := false
	openedWindows := 0
	currentLarge := false
	largeDocuments := 0
	var largeReservations int64

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
		b, err := index.NewBuilder(opts)
		if err != nil {
			return fmt.Errorf("create builder: %w", err)
		}
		current = b
		currentLarge = large
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
		if pendingWeight > 0 {
			readSemaphore.Release(pendingWeight)
			pendingWeight = 0
		}
		if largeReservations > 0 {
			largeDocumentSemaphore.Release(largeReservations)
			largeReservations = 0
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

		large := len(doc.content) > shardMax
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
		if large {
			if current != nil && !largeDocumentSemaphore.TryAcquire(1) {
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
				if err := largeDocumentSemaphore.Acquire(ctx, 1); err != nil {
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
			largeReservations++
		}
		if current == nil {
			if err := openWindow(openedWindows > 0, large); err != nil {
				if largeReservations > 0 {
					largeDocumentSemaphore.Release(largeReservations)
					largeReservations = 0
				}
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
		if err := current.Add(index.Document{
			Name:       doc.name,
			Content:    doc.content,
			Branches:   doc.branches,
			SkipReason: doc.skipReason,
		}); err != nil {
			addErr = fmt.Errorf("add document %s: %w", doc.name, err)
			if cancelProducer != nil {
				cancelProducer()
			}
			continue
		}
		if large {
			largeDocuments++
		}

		if pendingWeight >= indexWindowBytes || (currentLarge && largeDocuments >= largeParallelism) {
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
