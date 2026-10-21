package main

import (
	"context"
	"errors"
	"os/exec"
	"reflect"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/ignore"
	"github.com/sourcegraph/zoekt/index"
)

const maxNativeGitDeltaPaths = gitObjectBatchSize

type nativeGitDelta struct {
	repository   zoekt.Repository
	changedPaths []string
	targetInfos  []gitBlobInfo
	nextState    committedGitState
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
// incompatible base, too many shards or paths, changed ignore rules, a missing
// base commit, or a delta payload that reaches the build's index window. No
// other committed-data backend is a fallback.
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

	repository, err := nativeGitRepository(ctx, repoDir, target)
	if err != nil {
		return nil, false, err
	}
	opts := indexBuildOptions(indexDir, 1)
	opts.RepositoryDescription = repository
	opts.SetDefaults()
	existing, _, ok, err := opts.FindRepositoryMetadata()
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
	if state, _ := baseOptions.IndexState(); state != index.IndexStateEqual {
		return nil, false, nil
	}
	baseExists, err := nativeGitCommitExists(ctx, repoDir, baseOID)
	if err != nil {
		return nil, false, err
	}
	if !baseExists {
		return nil, false, nil
	}

	matcher, err := readNativeGitIgnore(ctx, repoDir, target)
	if err != nil {
		return nil, false, err
	}
	diff, err := readNativeGitDiff(ctx, repoDir, baseOID, target.commitOID, maxNativeGitDeltaPaths)
	if errors.Is(err, errNativeGitDeltaTooLarge) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, entry := range diff {
		if entry.path == ".sourcegraph/ignore" {
			return nil, false, nil
		}
	}

	targetInfos, err := scanNativeGitDeltaChanges(ctx, repoDir, matcher, diff)
	if err != nil {
		return nil, false, err
	}
	var payloadBytes int64
	for _, info := range targetInfos {
		if !info.oversize {
			payloadBytes += info.size
			if payloadBytes >= indexWindow(ctx) {
				return nil, false, nil
			}
		}
	}

	changedPaths := make([]string, 0, len(diff))
	for _, entry := range diff {
		if nativeGitBlobMode(entry.oldMode) && !matcher.Match(entry.path) {
			changedPaths = append(changedPaths, entry.path)
		}
	}
	return &nativeGitDelta{
		repository:   repository,
		changedPaths: changedPaths,
		targetInfos:  targetInfos,
		nextState: committedGitState{
			head:       target.commitOID,
			baseHead:   committedState.baseHead,
			baseShards: committedState.baseShards,
		},
	}, true, nil
}

// scanNativeGitDeltaChanges checks the new blobs named by diff-tree and returns
// those that the ignore rules keep, so delta admission never enumerates the
// unchanged target tree.
func scanNativeGitDeltaChanges(
	ctx context.Context,
	repoDir string,
	matcher *ignore.Matcher,
	diff []gitDiffEntry,
) ([]gitBlobInfo, error) {
	entries := make([]gitTreeEntry, 0, len(diff))
	for _, entry := range diff {
		if nativeGitBlobMode(entry.newMode) {
			entries = append(entries, gitTreeEntry{mode: entry.newMode, oid: entry.newOID, path: entry.path})
		}
	}
	targetInfos := make([]gitBlobInfo, 0, len(entries))
	err := checkNativeGitBlobs(ctx, repoDir, entries, func(infos []gitBlobInfo) error {
		for _, info := range infos {
			if !matcher.Match(info.entry.path) {
				targetInfos = append(targetInfos, info)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return targetInfos, nil
}

// indexNativeGitDelta hard-links the verified committed seed into buildDir and
// writes one delta there, with up to parallelism shard jobs at the same time.
// buildDir is staging owned by the caller; this function does not change or
// publish the live family.
func indexNativeGitDelta(
	ctx context.Context,
	repoDir string,
	buildDir string,
	seedPaths []string,
	delta *nativeGitDelta,
	parallelism int,
) error {
	if err := seedFamilyFiles(buildDir, seedPaths); err != nil {
		return err
	}
	documents := make([]fileContent, 0, len(delta.targetInfos))
	err := readNativeGitBlobs(ctx, repoDir, delta.targetInfos, func(document fileContent) error {
		document.branches = []string{"HEAD"}
		documents = append(documents, document)
		return nil
	})
	if err != nil {
		releaseFileContentWeights(documents)
		return err
	}
	_, err = indexDeltaDocumentsWithRepository(buildDir, delta.repository, documents, parallelism, delta.changedPaths)
	return err
}
