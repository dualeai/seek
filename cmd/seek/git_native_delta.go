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
	cmd := nativeGitCmd(ctx, repoDir, "cat-file", "-e", oid.String()+"^{commit}")
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if errors.Is(err, exec.ErrNotFound) {
		return false, nativeGitCommandError(ctx, "git cat-file -e", err, &stderr)
	}
	if _, ok := err.(*exec.ExitError); ok {
		return false, nil
	}
	return false, nativeGitCommandError(ctx, "git cat-file -e", err, &stderr)
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
// base commit, or a delta payload that reaches the window limit. A corpus-limit
// error stops the build. No other committed-data backend is a fallback.
func prepareNativeGitDelta(
	ctx context.Context,
	repoDir string,
	cacheDir string,
	indexDir string,
	target gitSnapshot,
	scan familyScan,
) (*nativeGitDelta, bool, error) {
	if !scan.hasCommittedShard() || !scan.contiguous(familyCommitted) || !scan.matchesManifestFamily(indexDir, familyCommitted) {
		return nil, false, nil
	}
	if err := requireNativeGitVersion(ctx, repoDir); err != nil {
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
	// Builder.Finish updates LatestCommitDate in existing shard sidecars, but it
	// does not update their stored Rank. Rebuild when explicit commit-date
	// ranking crosses a month so every shard keeps one rank.
	if _, ok := repository.RawConfig["latestcommitdate"]; ok && existing.Rank != repository.Rank {
		return nil, false, nil
	}
	baseOptions := opts
	baseRepository := repository
	baseRepository.Branches = existing.Branches
	// Zoekt derives priority rank when it reads shard metadata. RawConfig
	// equality above proves that this derived value matches.
	baseRepository.Rank = existing.Rank
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
		if entry.oldPath == ".sourcegraph/ignore" || entry.newPath == ".sourcegraph/ignore" {
			return nil, false, nil
		}
	}

	targetInfos, nextBudget, consistent, err := scanNativeGitDeltaChanges(ctx, repoDir, matcher, diff, committedState.budget)
	if err != nil {
		return nil, false, err
	}
	if !consistent {
		return nil, false, nil
	}
	var payloadBytes int64
	for _, info := range targetInfos {
		if !info.oversize {
			payloadBytes += info.size
			if payloadBytes >= indexWindowBytes {
				return nil, false, nil
			}
		}
	}

	changedPaths := make([]string, 0, len(diff))
	for _, entry := range diff {
		if nativeGitBlobMode(entry.oldMode) && !matcher.Match(entry.oldPath) {
			changedPaths = append(changedPaths, entry.oldPath)
		}
	}
	return &nativeGitDelta{
		repository:   repository,
		changedPaths: changedPaths,
		targetInfos:  targetInfos,
		nextState: committedGitState{
			head:       target.commitOID,
			baseHead:   committedState.baseHead,
			budget:     nextBudget,
			baseShards: committedState.baseShards,
		},
	}, true, nil
}

// scanNativeGitDeltaChanges updates totals from the old and new objects named
// by diff-tree. The saved full-scan totals account for every unchanged blob,
// so delta admission never enumerates the unchanged target tree.
func scanNativeGitDeltaChanges(
	ctx context.Context,
	repoDir string,
	matcher *ignore.Matcher,
	diff []gitDiffEntry,
	base gitIndexBudget,
) ([]gitBlobInfo, gitIndexBudget, bool, error) {
	type requestRole struct {
		diffIndex int
		old       bool
	}
	entries := make([]gitTreeEntry, 0, 2*len(diff))
	roles := make([]requestRole, 0, 2*len(diff))
	for i, entry := range diff {
		if nativeGitBlobMode(entry.oldMode) {
			entries = append(entries, gitTreeEntry{mode: entry.oldMode, oid: entry.oldOID, path: entry.oldPath})
			roles = append(roles, requestRole{diffIndex: i, old: true})
		}
		if nativeGitBlobMode(entry.newMode) {
			entries = append(entries, gitTreeEntry{mode: entry.newMode, oid: entry.newOID, path: entry.newPath})
			roles = append(roles, requestRole{diffIndex: i})
		}
	}
	oldInfos := make([]gitBlobInfo, len(diff))
	newInfos := make([]gitBlobInfo, len(diff))
	oldPresent := make([]bool, len(diff))
	newPresent := make([]bool, len(diff))
	roleIndex := 0
	if err := checkNativeGitBlobs(ctx, repoDir, entries, func(infos []gitBlobInfo) error {
		for _, info := range infos {
			role := roles[roleIndex]
			roleIndex++
			if role.old {
				oldInfos[role.diffIndex] = info
				oldPresent[role.diffIndex] = true
			} else {
				newInfos[role.diffIndex] = info
				newPresent[role.diffIndex] = true
			}
		}
		return nil
	}); err != nil {
		return nil, gitIndexBudget{}, false, err
	}

	budget := base
	targetInfos := make([]gitBlobInfo, 0, len(diff))
	for i, entry := range diff {
		if oldPresent[i] {
			oldInfo := oldInfos[i]
			if budget.candidates == 0 || (!oldInfo.oversize && budget.indexedBytes < oldInfo.size) {
				return nil, gitIndexBudget{}, false, nil
			}
			budget.candidates--
			if !oldInfo.oversize {
				budget.indexedBytes -= oldInfo.size
			}
		}
		if !newPresent[i] {
			continue
		}
		newInfo := newInfos[i]
		budget.candidates++
		if budget.candidates > gitCandidateFileLimit {
			return nil, gitIndexBudget{}, false, gitCommittedCapError(
				"git committed file cap exceeded",
				indexCapCandidateFiles,
				budget.candidates,
				gitCandidateFileLimit,
			)
		}
		if !newInfo.oversize {
			budget.indexedBytes += newInfo.size
			if budget.indexedBytes > gitCorpusIndexedByteLimit {
				return nil, gitIndexBudget{}, false, gitCommittedCapError(
					"git committed candidate blob byte cap exceeded",
					indexCapIndexedBytes,
					budget.indexedBytes,
					gitCorpusIndexedByteLimit,
				)
			}
		}
		if !matcher.Match(entry.newPath) {
			targetInfos = append(targetInfos, newInfo)
		}
	}
	return targetInfos, budget, true, nil
}

// indexNativeGitDelta hard-links the verified committed seed into buildDir and
// writes one delta there. buildDir is staging owned by the caller; this
// function does not change or publish the live family.
func indexNativeGitDelta(
	ctx context.Context,
	repoDir string,
	buildDir string,
	seedPaths []string,
	delta *nativeGitDelta,
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
	_, err = indexDeltaDocumentsWithRepository(buildDir, delta.repository, documents, 0, delta.changedPaths)
	return err
}
