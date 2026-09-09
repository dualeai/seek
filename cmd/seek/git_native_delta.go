package main

import (
	"context"
	"errors"
	"fmt"
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
	indexDir string,
	target gitSnapshot,
	scan familyScan,
) (*nativeGitDelta, bool, error) {
	if !scan.hasCommittedShard() || !scan.contiguous(familyCommitted) || !scan.matchesManifestFamily(indexDir, familyCommitted) {
		return nil, false, nil
	}
	if nativeCommittedShardCount(scan) > maxCommittedDeltaShards {
		return nil, false, nil
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
	if len(existing.Branches) != 1 || existing.Branches[0].Name != "HEAD" {
		return nil, false, nil
	}
	baseOID, err := parseGitObjectID([]byte(existing.Branches[0].Version))
	if err != nil || len(baseOID) != len(target.commitOID) || baseOID == target.commitOID {
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

	targetInfos, err := scanNativeGitDeltaTarget(ctx, repoDir, target, matcher, diff)
	if err != nil {
		return nil, false, err
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
	}, true, nil
}

func scanNativeGitDeltaTarget(
	ctx context.Context,
	repoDir string,
	target gitSnapshot,
	matcher *ignore.Matcher,
	diff []gitDiffEntry,
) ([]gitBlobInfo, error) {
	expected := make(map[string]gitDiffEntry, len(diff))
	for _, entry := range diff {
		if nativeGitBlobMode(entry.newMode) {
			expected[entry.newPath] = entry
		}
	}
	found := make(map[string]struct{}, len(expected))
	targetInfos := make([]gitBlobInfo, 0, len(expected))
	var budget gitIndexBudget
	err := readNativeGitTree(ctx, repoDir, target, nil, func(entries []gitTreeEntry) error {
		blobs := make([]gitTreeEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.mode == "160000" {
				continue
			}
			budget.candidates++
			if budget.candidates > gitCandidateFileLimit {
				return gitCommittedCapError(
					"git committed file cap exceeded",
					indexCapCandidateFiles,
					budget.candidates,
					gitCandidateFileLimit,
				)
			}
			blobs = append(blobs, entry)
		}
		return checkNativeGitBlobs(ctx, repoDir, blobs, func(infos []gitBlobInfo) error {
			for _, info := range infos {
				if !info.oversize {
					budget.indexedBytes += info.size
					if budget.indexedBytes > gitCorpusIndexedByteLimit {
						return gitCommittedCapError(
							"git committed candidate blob byte cap exceeded",
							indexCapIndexedBytes,
							budget.indexedBytes,
							gitCorpusIndexedByteLimit,
						)
					}
				}
				entry, changed := expected[info.entry.path]
				if !changed {
					continue
				}
				if entry.newMode != info.entry.mode || entry.newOID != info.entry.oid {
					return fmt.Errorf("git delta target entry %q changed during scan", info.entry.path)
				}
				found[info.entry.path] = struct{}{}
				if !matcher.Match(info.entry.path) {
					targetInfos = append(targetInfos, info)
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if len(found) != len(expected) {
		return nil, fmt.Errorf("git delta target tree lacks %d changed blobs", len(expected)-len(found))
	}
	return targetInfos, nil
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
