package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
)

// maxNativeGitDeltaPaths is the most changed paths of a commit range that a
// delta takes (readNativeGitDiff). A larger range builds the family in full.
const maxNativeGitDeltaPaths = gitObjectBatchSize

// nativeGitDelta is an admitted delta of the committed family, from the
// published commit to the target commit.
type nativeGitDelta struct {
	repository   zoekt.Repository
	changedPaths []string
	targetInfos  []gitBlobInfo
	nextState    committedGitState
	// keptLiveBytes is the live bytes of the base less the bytes of the old
	// blobs that the delta tombstones. indexNativeGitDelta adds the new blobs
	// that it reads to set the live bytes of nextState.
	keptLiveBytes int64
}

// nativeGitBlobMode reports a tree-entry mode of a blob that the index reads:
// a regular file, an executable file, or a symlink.
func nativeGitBlobMode(mode string) bool {
	switch mode {
	case "100644", "100755", "120000":
		return true
	default:
		return false
	}
}

func nativeGitCommitExists(ctx context.Context, repoDir string, oid gitObjectID) (bool, error) {
	cmd := newGitCommand(ctx, repoDir, "cat-file", "-e", oid.String()+"^{commit}")
	_, err := gitOutput(ctx, cmd, "git cat-file -e")
	if err == nil {
		return true, nil
	}
	// A nonzero exit means the commit is absent; other errors are failures.
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return false, nil
	}
	return false, err
}

func nativeCommittedShardCount(scan familyScan) int {
	count := 0
	for _, member := range scan.members {
		if member.shard && !member.uncommitted {
			count++
		}
	}
	return count
}

// nativeGitDeltaShardLimitExceeded reports a family that a delta must not
// extend. That family has no recorded full base, fewer shards than that base,
// or more than maxCommittedDeltaShards shards above it.
func nativeGitDeltaShardLimitExceeded(current, base int) bool {
	return base < 1 || current < base || current-base > maxCommittedDeltaShards
}

// nativeGitDeltaStaticMetadataEqual reports whether the base keeps the static
// metadata of target. A base with other metadata, for example zoekt.* Git
// config values in its shards, needs one native full build.
func nativeGitDeltaStaticMetadataEqual(existing, target *zoekt.Repository) bool {
	return existing.TenantID == target.TenantID &&
		existing.ID == target.ID &&
		existing.Name == target.Name &&
		existing.URL == target.URL &&
		existing.Source == target.Source &&
		existing.CommitURLTemplate == target.CommitURLTemplate &&
		existing.FileURLTemplate == target.FileURLTemplate &&
		existing.LineFragmentTemplate == target.LineFragmentTemplate &&
		reflect.DeepEqual(existing.RawConfig, target.RawConfig) &&
		existing.Tombstone == target.Tombstone
}

// prepareNativeGitDelta checks whether target can extend the published
// committed family. A nil delta with no error selects native full indexing;
// an error stops the build. It selects full indexing for:
//   - an incomplete or incompatible base;
//   - no usable Ctags;
//   - too many shards or paths;
//   - a missing base commit;
//   - a delta payload that reaches the build's index window;
//   - tombstoned content that reaches both the live content and one shard
//     (deltaNeedsCompaction).
//
// No other committed-data backend is a fallback.
func prepareNativeGitDelta(
	ctx context.Context,
	repoDir string,
	store corpusStore,
	target gitObjectID,
	scan familyScan,
) (*nativeGitDelta, error) {
	cacheDir, indexDir := store.cacheDir, store.indexDir
	if !scan.hasShard(familyCommitted) || !scan.contiguous(familyCommitted) || !scan.matchesManifestFamily(familyCommitted) {
		return nil, nil
	}
	if err := requireNativeGitVersion(ctx); err != nil {
		return nil, err
	}
	// This also resolves the verified `ctags` fallback before IndexState
	// compares the index options with the base. If Ctags is unavailable, the
	// native full build decides whether the target needs a Builder at all.
	if err := checkCtagsCached(); err != nil {
		return nil, nil
	}

	repository := nativeGitRepository(repoDir, target)
	opts := indexBuildOptions(indexDir, 1)
	opts.RepositoryDescription = repository
	opts.SetDefaults()
	var existing *zoekt.Repository
	var ok bool
	err := zoektShardCall(func() (err error) {
		existing, _, ok, err = opts.FindRepositoryMetadata()
		return err
	})
	if err != nil || !ok || existing == nil {
		return nil, nil
	}
	existingHead, ok := recordedHeadVersion(existing)
	if !ok {
		return nil, nil
	}
	baseOID, err := parseGitObjectID([]byte(existingHead))
	if err != nil || len(baseOID) != len(target) || baseOID == target {
		return nil, nil
	}
	committedState, ok := readCommittedGitState(cacheDir)
	currentShards := nativeCommittedShardCount(scan)
	if !ok || committedState.head != baseOID || nativeGitDeltaShardLimitExceeded(currentShards, committedState.baseShards) {
		return nil, nil
	}
	if !nativeGitDeltaStaticMetadataEqual(existing, &repository) {
		return nil, nil
	}
	baseOptions := opts
	baseRepository := repository
	baseRepository.Branches = existing.Branches
	baseOptions.RepositoryDescription = baseRepository
	var state index.IndexState
	if zoektShardCall(func() error {
		state, _ = baseOptions.IndexState()
		return nil
	}) != nil || state != index.IndexStateEqual {
		return nil, nil
	}
	baseExists, err := nativeGitCommitExists(ctx, repoDir, baseOID)
	if err != nil {
		return nil, err
	}
	if !baseExists {
		return nil, nil
	}

	diff, err := readNativeGitDiff(ctx, repoDir, baseOID, target, maxNativeGitDeltaPaths)
	if errors.Is(err, errNativeGitDeltaTooLarge) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	targetInfos, oldBytes, err := scanNativeGitDeltaChanges(ctx, repoDir, diff)
	if err != nil {
		return nil, err
	}
	var payloadBytes, newBytes int64
	for _, info := range targetInfos {
		if !info.oversize {
			payloadBytes += info.size
			if payloadBytes >= indexWindow(ctx) {
				return nil, nil
			}
		}
		if !info.nameOnly() {
			newBytes += info.size
		}
	}
	tombstoned := committedState.deadBytes + oldBytes
	liveBytes := max(committedState.liveBytes-oldBytes+newBytes, 0)
	if deltaNeedsCompaction(tombstoned, liveBytes) {
		return nil, nil
	}

	changedPaths := make([]string, 0, len(diff))
	for _, entry := range diff {
		if nativeGitBlobMode(entry.oldMode) {
			changedPaths = append(changedPaths, entry.path)
		}
	}
	return &nativeGitDelta{
		repository:    repository,
		changedPaths:  changedPaths,
		targetInfos:   targetInfos,
		keptLiveBytes: committedState.liveBytes - oldBytes,
		nextState: committedGitState{
			head:       target,
			baseHead:   committedState.baseHead,
			baseShards: committedState.baseShards,
			deadBytes:  tombstoned,
		},
	}, nil
}

// scanNativeGitDeltaChanges checks the new blobs that diff-tree names, so delta
// admission never lists the unchanged target tree. The same batch session
// also reads the old blobs of the changed paths, with no other Git process. It
// returns the bytes that their shard documents held (heldContentBytes), which
// the delta tombstones (deltaNeedsCompaction). So an old binary blob counts as
// its marker, not its size: a commit that changes only a binary asset stays a
// delta. The read streams each old blob once, as the delta reads each new one.
func scanNativeGitDeltaChanges(
	ctx context.Context,
	repoDir string,
	diff []gitDiffEntry,
) ([]gitBlobInfo, int64, error) {
	entries := make([]gitTreeEntry, 0, 2*len(diff))
	for _, entry := range diff {
		if nativeGitBlobMode(entry.newMode) {
			entries = append(entries, gitTreeEntry{oid: entry.newOID, path: entry.path})
		}
	}
	newEntries := len(entries)
	for _, entry := range diff {
		if nativeGitBlobMode(entry.oldMode) {
			entries = append(entries, gitTreeEntry{oid: entry.oldOID, path: entry.path})
		}
	}
	if len(entries) == 0 {
		return nil, 0, nil
	}
	batch, err := startNativeGitBatch(ctx, repoDir)
	if err != nil {
		return nil, 0, err
	}
	defer batch.abort(nil)
	targetInfos := make([]gitBlobInfo, 0, newEntries)
	oldInfos := make([]gitBlobInfo, 0, len(entries)-newEntries)
	for start := 0; start < len(entries); start += gitObjectBatchSize {
		end := min(start+gitObjectBatchSize, len(entries))
		infos, err := batch.check(entries[start:end])
		if err != nil {
			return nil, 0, err
		}
		for i, info := range infos {
			if start+i < newEntries {
				targetInfos = append(targetInfos, info)
			} else {
				oldInfos = append(oldInfos, info)
			}
		}
	}
	var oldBytes int64
	for start := 0; start < len(oldInfos); start += gitObjectBatchSize {
		end := min(start+gitObjectBatchSize, len(oldInfos))
		err := batch.read(oldInfos[start:end], func(document fileContent) error {
			oldBytes += heldContentBytes(document)
			readSemaphore.Release(document.weight)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	if err := batch.close(); err != nil {
		return nil, 0, err
	}
	return targetInfos, oldBytes, nil
}

// indexNativeGitDelta hard-links the verified committed seed into buildDir and
// writes one delta there, with up to parallelism shard jobs at the same time.
// buildDir is staging owned by the caller; this function does not change or
// publish the live family. A seed link error or a Builder error wraps
// errNativeDeltaFallback; a blob read error does not.
func indexNativeGitDelta(
	ctx context.Context,
	repoDir string,
	buildDir string,
	seedPaths []string,
	delta *nativeGitDelta,
	parallelism int,
) error {
	if err := seedFamilyFiles(buildDir, seedPaths); err != nil {
		return fmt.Errorf("%w: %w", errNativeDeltaFallback, err)
	}
	documents := make([]fileContent, 0, len(delta.targetInfos))
	var heldBytes int64
	err := readNativeGitBlobs(ctx, repoDir, delta.targetInfos, func(document fileContent) error {
		document.branches = []string{"HEAD"}
		heldBytes += heldContentBytes(document)
		documents = append(documents, document)
		return nil
	})
	if err != nil {
		releaseFileContentWeights(documents)
		return err
	}
	// The admission counted the new blobs by size: the reads give the bytes
	// that their shard documents hold, a marker for a binary blob.
	delta.nextState.liveBytes = max(delta.keptLiveBytes+heldBytes, 0)
	if _, err := indexDeltaDocumentsWithRepository(buildDir, delta.repository, documents, parallelism, delta.changedPaths); err != nil {
		return fmt.Errorf("%w: %w", errNativeDeltaFallback, err)
	}
	return nil
}

// errNativeDeltaFallback marks a delta error after which buildCommittedGitStage
// builds the family in full, as the folder delta does (seedFolderDelta). Two
// errors take it:
//   - A seed link error: a full build links no seed.
//   - A Zoekt Builder error of the delta, such as a damaged base shard record
//     that only Finish reads: a full build reads no base shard.
//
// The full build can get the same error again, such as a ctags failure or a
// full disk. Until that error ends, each search then pays for the delta and
// the failed full build. On a repository of 2,000 or 20,000 files, that is
// 463-556 ms against 94-137 ms with no fallback.
var errNativeDeltaFallback = errors.New("native Git delta failed")
