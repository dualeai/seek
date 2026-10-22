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

const maxNativeGitDeltaPaths = gitObjectBatchSize

type nativeGitDelta struct {
	repository   zoekt.Repository
	changedPaths []string
	targetInfos  []gitBlobInfo
	nextState    committedGitState
	// baseLiveBytes and oldBytes are the live bytes of the base and the bytes
	// of the old blobs that the delta tombstones. indexNativeGitDelta sets the
	// live bytes of nextState from them and the new blobs that it reads.
	baseLiveBytes int64
	oldBytes      int64
}

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
// committed family. eligible=false with no error selects native full indexing;
// an error stops the build. It selects full indexing for an incomplete or
// incompatible base, no usable Ctags, too many shards or paths, a missing base
// commit, a delta payload that reaches the build's index window, or tombstoned
// content that reaches both the live content and one shard
// (deltaNeedsCompaction). No other committed-data backend is a fallback.
func prepareNativeGitDelta(
	ctx context.Context,
	repoDir string,
	store corpusStore,
	target gitSnapshot,
	scan familyScan,
) (*nativeGitDelta, bool, error) {
	cacheDir, indexDir := store.cacheDir, store.indexDir
	if !scan.hasShard(familyCommitted) || !scan.contiguous(familyCommitted) || !scan.matchesManifestFamily(indexDir, familyCommitted) {
		return nil, false, nil
	}
	if err := requireNativeGitVersion(ctx); err != nil {
		return nil, false, err
	}
	// This also resolves the verified `ctags` fallback before index options are
	// compared with the base. If Ctags is unavailable, let native full decide
	// whether the target needs a Builder at all.
	if err := checkCtagsCached(); err != nil {
		return nil, false, nil
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
		return nil, false, nil
	}
	existingHead, ok := recordedHeadVersion(existing)
	if !ok {
		return nil, false, nil
	}
	baseOID, err := parseGitObjectID([]byte(existingHead))
	if err != nil || len(baseOID) != len(target.commitOID) || baseOID == target.commitOID {
		return nil, false, nil
	}
	committedState, ok := readCommittedGitState(cacheDir)
	currentShards := nativeCommittedShardCount(scan)
	if !ok || committedState.head != baseOID || nativeGitDeltaShardLimitExceeded(currentShards, committedState.baseShards) {
		return nil, false, nil
	}
	if !nativeGitDeltaStaticMetadataEqual(existing, &repository) {
		return nil, false, nil
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
		return nil, false, nil
	}
	baseExists, err := nativeGitCommitExists(ctx, repoDir, baseOID)
	if err != nil {
		return nil, false, err
	}
	if !baseExists {
		return nil, false, nil
	}

	diff, err := readNativeGitDiff(ctx, repoDir, baseOID, target.commitOID, maxNativeGitDeltaPaths)
	if errors.Is(err, errNativeGitDeltaTooLarge) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	targetInfos, oldBytes, err := scanNativeGitDeltaChanges(ctx, repoDir, diff)
	if err != nil {
		return nil, false, err
	}
	var payloadBytes, newBytes int64
	for _, info := range targetInfos {
		if !info.oversize {
			payloadBytes += info.size
			if payloadBytes >= indexWindow(ctx) {
				return nil, false, nil
			}
		}
		if !info.nameOnly() {
			newBytes += info.size
		}
	}
	tombstoned := committedState.deadBytes + oldBytes
	liveBytes := max(committedState.liveBytes-oldBytes+newBytes, 0)
	if deltaNeedsCompaction(tombstoned, liveBytes) {
		return nil, false, nil
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
		baseLiveBytes: committedState.liveBytes,
		oldBytes:      oldBytes,
		nextState: committedGitState{
			head:       target.commitOID,
			baseHead:   committedState.baseHead,
			baseShards: committedState.baseShards,
			liveBytes:  liveBytes,
			deadBytes:  tombstoned,
		},
	}, true, nil
}

// scanNativeGitDeltaChanges checks the new blobs named by diff-tree, so delta
// admission never enumerates the unchanged target tree. The same batch session
// also reads the old blobs of the changed paths, with no other Git process,
// and returns the bytes that their shard documents held (heldContentBytes),
// which the delta tombstones (deltaNeedsCompaction). So an old binary blob
// counts as its marker, not its size: a commit that changes only a binary
// asset stays a delta. The read streams each old blob once, as the delta reads
// each new one.
func scanNativeGitDeltaChanges(
	ctx context.Context,
	repoDir string,
	diff []gitDiffEntry,
) ([]gitBlobInfo, int64, error) {
	entries := make([]gitTreeEntry, 0, 2*len(diff))
	for _, entry := range diff {
		if nativeGitBlobMode(entry.newMode) {
			entries = append(entries, gitTreeEntry{mode: entry.newMode, oid: entry.newOID, path: entry.path})
		}
	}
	newEntries := len(entries)
	for _, entry := range diff {
		if nativeGitBlobMode(entry.oldMode) {
			entries = append(entries, gitTreeEntry{mode: entry.oldMode, oid: entry.oldOID, path: entry.path})
		}
	}
	if len(entries) == 0 {
		return nil, 0, nil
	}
	batch, err := startNativeGitBatch(ctx, repoDir)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		if !batch.finished {
			batch.abort(nil)
		}
	}()
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
	delta.nextState.liveBytes = max(delta.baseLiveBytes-delta.oldBytes+heldBytes, 0)
	if _, err := indexDeltaDocumentsWithRepository(buildDir, delta.repository, documents, parallelism, delta.changedPaths); err != nil {
		return fmt.Errorf("%w: %w", errNativeDeltaFallback, err)
	}
	return nil
}

// errNativeDeltaFallback marks a delta error after which buildCommittedGitStage
// builds the family in full, as the folder delta does (seedFolderDelta): a
// seed link error (a full build links no seed) and an error of the delta's
// Zoekt Builder, such as a damaged base shard record that only its Finish
// reads (a full build reads no base shard). An error that the full build gets
// again, such as a ctags failure or a full disk, then costs each search the
// delta and the failed full build until it ends: 463-556 ms against
// 94-137 ms with no fallback, on a repository of 2,000 or 20,000 files.
var errNativeDeltaFallback = errors.New("native Git delta failed")
