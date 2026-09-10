package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/ignore"
)

type gitSnapshot struct {
	commitOID  gitObjectID
	commitTime time.Time
}

type nativeGitStreamResult struct {
	budget gitIndexBudget
	err    error
}

// captureGitSnapshot verifies a full SHA-1 or SHA-256 commit ID and reads its
// committer time. The "no-head" value returns ok=false without an error.
func captureGitSnapshot(ctx context.Context, repoDir, captured string) (gitSnapshot, bool, error) {
	if captured == "no-head" {
		return gitSnapshot{}, false, nil
	}
	want, err := parseGitObjectID([]byte(captured))
	if err != nil {
		return gitSnapshot{}, false, fmt.Errorf("captured Git commit: %w", err)
	}

	cmd := nativeGitCmd(ctx, repoDir, "show", "-s", "--no-patch", "--format=%H%x00%cI", captured+"^{commit}")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return gitSnapshot{}, false, fmt.Errorf("open git show stdout: %w", err)
	}
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return gitSnapshot{}, false, nativeGitCommandError(ctx, "start git show", err, &stderr)
	}
	raw, readErr := io.ReadAll(io.LimitReader(stdout, 258))
	if readErr != nil {
		abortNativeGitChild(cmd, nil)
		return gitSnapshot{}, false, nativeGitReadError(ctx, "read git show", readErr)
	}
	if len(raw) > 257 {
		abortNativeGitChild(cmd, nil)
		return gitSnapshot{}, false, fmt.Errorf("git show returned an oversize snapshot record")
	}
	if err := waitNativeGitChild(ctx, cmd, "git show", &stderr); err != nil {
		return gitSnapshot{}, false, err
	}
	raw = bytes.TrimSuffix(raw, []byte{'\n'})
	oidRaw, unixRaw, ok := bytes.Cut(raw, []byte{0})
	if !ok || len(unixRaw) == 0 || bytes.IndexByte(unixRaw, 0) >= 0 {
		return gitSnapshot{}, false, fmt.Errorf("malformed git show snapshot record")
	}
	got, err := parseGitObjectID(oidRaw)
	if err != nil {
		return gitSnapshot{}, false, fmt.Errorf("git show commit: %w", err)
	}
	if got != want {
		return gitSnapshot{}, false, fmt.Errorf("git show returned commit %s, want %s", got, want)
	}
	commitTime, err := time.Parse(time.RFC3339, string(unixRaw))
	if err != nil {
		return gitSnapshot{}, false, fmt.Errorf("parse Git commit time %q: %w", unixRaw, err)
	}
	return gitSnapshot{commitOID: got, commitTime: commitTime}, true, nil
}

type nativeGitConfig struct {
	zoekt map[string]string
}

func (config nativeGitConfig) zoektValue(name string) string {
	if value, ok := config.zoekt[name]; ok {
		return value
	}
	for key, value := range config.zoekt {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func readNativeGitConfig(ctx context.Context, repoDir string) (nativeGitConfig, error) {
	cmd := nativeGitCmd(ctx, repoDir, "config", "--null", "--local", "--get-regexp", "^zoekt\\.")
	var stdout bytes.Buffer
	var stderr boundedGitStderr
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nativeGitConfig{}, ctxErr
		}
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 || stdout.Len() != 0 {
			return nativeGitConfig{}, nativeGitCommandError(ctx, "git config", err, &stderr)
		}
	}

	result := nativeGitConfig{zoekt: make(map[string]string)}
	for _, record := range bytes.Split(stdout.Bytes(), []byte{0}) {
		if len(record) == 0 {
			continue
		}
		keyRaw, valueRaw, ok := bytes.Cut(record, []byte{'\n'})
		if !ok || len(keyRaw) == 0 {
			return nativeGitConfig{}, fmt.Errorf("malformed git config record")
		}
		key := string(keyRaw)
		if strings.HasPrefix(strings.ToLower(key), "zoekt.") {
			// Git config keys are case-insensitive. Keep RawConfig deterministic by
			// storing the spelling that Git exposes: lowercase option names.
			option := strings.ToLower(key[len("zoekt."):])
			result.zoekt[option] = string(valueRaw)
		}
	}
	return result, nil
}

// nativeGitRepository makes provider-neutral metadata for one captured commit.
// Only explicit local zoekt.* settings can replace its opaque name or set its
// web URL.
func nativeGitRepository(ctx context.Context, repoDir string, snapshot gitSnapshot) (zoekt.Repository, error) {
	repository := zoekt.Repository{
		Name:             fallbackGitRepositoryName(repoDir),
		Source:           repoDir,
		Branches:         []zoekt.RepositoryBranch{{Name: "HEAD", Version: snapshot.commitOID.String()}},
		LatestCommitDate: snapshot.commitTime,
	}
	config, err := readNativeGitConfig(ctx, repoDir)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return zoekt.Repository{}, err
		}
		slog.Debug("Cannot read Git repository metadata", "repo", repoDir, "error", err)
		return repository, nil
	}
	applyNativeGitConfig(&repository, config)
	return repository, nil
}

// applyNativeGitConfig enforces the provider-neutral architecture. Git is the
// format and object source; its hosting service does not control index
// behavior. Seek does not inspect a remote or make host-specific URL
// templates. The local [zoekt] section can set one repository name and one
// opaque web URL explicitly.
func applyNativeGitConfig(repository *zoekt.Repository, config nativeGitConfig) {
	if name := config.zoektValue("name"); name != "" && !reservedGitRepositoryName(name) {
		repository.Name = name
	}
	if webURL := config.zoektValue("web-url"); webURL != "" {
		repository.URL = webURL
	}

	id, _ := strconv.ParseUint(config.zoektValue("repoid"), 10, 32)
	repository.ID = uint32(id)
	repository.TenantID, _ = strconv.Atoi(config.zoektValue("tenantID"))
	if _, ok := config.zoekt["latestcommitdate"]; ok {
		repository.Rank = nativeGitCommitRank(repository.LatestCommitDate)
	}
	if len(config.zoekt) > 0 {
		repository.RawConfig = make(map[string]string, len(config.zoekt))
		for key, value := range config.zoekt {
			repository.RawConfig[key] = value
		}
	}
}

// nativeGitCommitRank returns Zoekt's month rank when local config enables
// latestCommitDate ranking.
func nativeGitCommitRank(commitTime time.Time) uint16 {
	if commitTime.Before(time.Unix(0, 0)) {
		return 0
	}
	months := int(commitTime.Year()-1970)*12 + int(commitTime.Month()-1)
	return uint16(min(months, math.MaxUint16))
}

// reservedGitRepositoryName reports names that overlap the fixed shard prefix
// for the dirty-file repository. The shard publisher uses this prefix to keep
// committed and dirty families separate.
func reservedGitRepositoryName(name string) bool {
	lower := strings.ToLower(name)
	return lower == repoUncommitted || strings.HasPrefix(lower, repoUncommitted+"_v")
}

func readNativeGitIgnore(ctx context.Context, repoDir string, snapshot gitSnapshot) (*ignore.Matcher, error) {
	entry, err := readNativeGitRootIgnoreEntry(ctx, repoDir, snapshot)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return &ignore.Matcher{}, nil
	}
	batch, err := startNativeGitBatch(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("start .sourcegraph/ignore object reader: %w", err)
	}
	matcher, readErr := readNativeGitIgnoreEntry(batch, entry)
	closeErr := batch.close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("finish .sourcegraph/ignore object reader: %w", closeErr)
	}
	return matcher, nil
}

func readNativeGitIgnoreEntry(batch *nativeGitBatch, entry *gitTreeEntry) (*ignore.Matcher, error) {
	if entry == nil {
		return &ignore.Matcher{}, nil
	}
	infos, err := batch.check([]gitTreeEntry{*entry})
	if err != nil {
		return nil, fmt.Errorf("check .sourcegraph/ignore: %w", err)
	}
	info := infos[0]
	if info.oversize {
		return nil, fmt.Errorf(".sourcegraph/ignore exceeds the %d-byte document limit", maxIndexedDocumentBytes)
	}
	var matcher *ignore.Matcher
	if err := batch.read([]gitBlobInfo{info}, func(document fileContent) error {
		parsed, parseErr := ignore.ParseIgnoreFile(bytes.NewReader(document.content))
		if parseErr != nil {
			return parseErr
		}
		matcher = parsed
		if document.weight > 0 {
			readSemaphore.Release(document.weight)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read .sourcegraph/ignore: %w", err)
	}
	return matcher, nil
}

func streamNativeGitDocuments(
	ctx context.Context,
	repoDir string,
	snapshot gitSnapshot,
	scope *gitDirtyScope,
) (<-chan fileContent, <-chan nativeGitStreamResult) {
	documents := make(chan fileContent)
	result := make(chan nativeGitStreamResult, 1)
	go func() {
		defer close(documents)
		ignoreEntry, err := readNativeGitRootIgnoreEntry(ctx, repoDir, snapshot)
		if err != nil {
			result <- nativeGitStreamResult{err: err}
			return
		}
		batch, err := startNativeGitBatch(ctx, repoDir)
		if err != nil {
			result <- nativeGitStreamResult{err: err}
			return
		}
		matcher, err := readNativeGitIgnoreEntry(batch, ignoreEntry)
		if err != nil {
			_ = batch.close()
			result <- nativeGitStreamResult{err: err}
			return
		}

		var budget gitIndexBudget
		headBranch := []string{"HEAD"}
		err = readNativeGitTree(ctx, repoDir, snapshot, scope, func(entries []gitTreeEntry) error {
			blobs := make([]gitTreeEntry, 0, len(entries))
			for _, entry := range entries {
				if entry.mode == "160000" || (scope != nil && !scope.contains(entry.path)) {
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
			infos, err := batch.check(blobs)
			if err != nil {
				return err
			}
			selectedInfos := make([]gitBlobInfo, 0, len(infos))
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
				if matcher.Match(info.entry.path) {
					continue
				}
				selectedInfos = append(selectedInfos, info)
			}
			return batch.read(selectedInfos, func(document fileContent) error {
				document.branches = headBranch
				select {
				case documents <- document:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		})
		if closeErr := batch.close(); err == nil {
			err = closeErr
		}
		result <- nativeGitStreamResult{budget: budget, err: err}
	}()
	return documents, result
}

// indexNativeGitFull writes one captured commit to indexDir through the shared
// Builder sink. It applies the root .sourcegraph/ignore file and an optional
// path scope, skips gitlinks, and emits oversize blob names without their
// bodies. Candidate and content limits apply before ignore filtering. indexDir
// is caller-owned staging; the caller validates HEAD and publishes it.
func indexNativeGitFull(
	ctx context.Context,
	repoDir string,
	indexDir string,
	snapshot gitSnapshot,
	scope *gitDirtyScope,
	parallelism int,
) error {
	_, err := indexNativeGitFullWithBudget(ctx, repoDir, indexDir, snapshot, scope, parallelism)
	return err
}

// indexNativeGitFullWithBudget returns the exact committed-tree work totals
// proved by the full scan. The combined-corpus publisher persists these totals
// for O(changed objects) delta admission. Scoped builds discard them.
func indexNativeGitFullWithBudget(
	ctx context.Context,
	repoDir string,
	indexDir string,
	snapshot gitSnapshot,
	scope *gitDirtyScope,
	parallelism int,
) (gitIndexBudget, error) {
	if err := requireNativeGitVersion(ctx, repoDir); err != nil {
		return gitIndexBudget{}, err
	}
	repository, err := nativeGitRepository(ctx, repoDir, snapshot)
	if err != nil {
		return gitIndexBudget{}, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	documents, result := streamNativeGitDocuments(streamCtx, repoDir, snapshot, scope)
	_, buildErr := indexDocumentsWithRepository(
		streamCtx,
		indexDir,
		repository,
		documents,
		parallelism,
		cancel,
	)
	streamResult := <-result
	if ctxErr := ctx.Err(); ctxErr != nil {
		return gitIndexBudget{}, ctxErr
	}
	if buildErr != nil {
		return gitIndexBudget{}, buildErr
	}
	if streamResult.err != nil {
		return gitIndexBudget{}, streamResult.err
	}
	return streamResult.budget, nil
}
