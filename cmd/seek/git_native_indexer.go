package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

	cmd := newGitCommand(ctx, repoDir, "show", "--no-patch", "--format=%H%x00%cI", captured+"^{commit}")
	raw, err := gitOutput(ctx, cmd, "git show")
	if err != nil {
		return gitSnapshot{}, false, err
	}
	raw = bytes.TrimSuffix(raw, []byte{'\n'})
	oidRaw, dateRaw, ok := bytes.Cut(raw, []byte{0})
	if !ok || len(dateRaw) == 0 || bytes.IndexByte(dateRaw, 0) >= 0 {
		return gitSnapshot{}, false, fmt.Errorf("malformed git show snapshot record")
	}
	got, err := parseGitObjectID(oidRaw)
	if err != nil {
		return gitSnapshot{}, false, fmt.Errorf("git show commit: %w", err)
	}
	if got != want {
		return gitSnapshot{}, false, fmt.Errorf("git show returned commit %s, want %s", got, want)
	}
	commitTime, err := time.Parse(time.RFC3339, string(dateRaw))
	if err != nil {
		return gitSnapshot{}, false, fmt.Errorf("parse Git commit time %q: %w", dateRaw, err)
	}
	return gitSnapshot{commitOID: got, commitTime: commitTime}, true, nil
}

// nativeGitConfig holds the local zoekt.* options. Git prints config names
// with the section and the option in lowercase, so each key is lowercase.
type nativeGitConfig map[string]string

func readNativeGitConfig(ctx context.Context, repoDir string) (nativeGitConfig, error) {
	cmd := newGitCommand(ctx, repoDir, "config", "--null", "--local", "--get-regexp", "^zoekt\\.")
	out, err := gitOutput(ctx, cmd, "git config")
	if err != nil {
		// Exit 1 with no output means no zoekt.* key.
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 || len(out) != 0 {
			return nil, err
		}
	}

	result := nativeGitConfig{}
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		keyRaw, valueRaw, ok := bytes.Cut(record, []byte{'\n'})
		if !ok || len(keyRaw) == 0 {
			return nil, fmt.Errorf("malformed git config record")
		}
		key := string(keyRaw)
		if strings.HasPrefix(strings.ToLower(key), "zoekt.") {
			// Git config keys are case-insensitive. Keep RawConfig deterministic by
			// storing the spelling that Git exposes: lowercase option names.
			option := strings.ToLower(key[len("zoekt."):])
			result[option] = string(valueRaw)
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
	if name := config["name"]; name != "" && !reservedGitRepositoryName(name) {
		repository.Name = name
	}
	if webURL := config["web-url"]; webURL != "" {
		repository.URL = webURL
	}

	id, _ := strconv.ParseUint(config["repoid"], 10, 32)
	repository.ID = uint32(id)
	repository.TenantID, _ = strconv.Atoi(config["tenantid"])
	if _, ok := config["latestcommitdate"]; ok {
		repository.Rank = nativeGitCommitRank(repository.LatestCommitDate)
	}
	if len(config) > 0 {
		repository.RawConfig = maps.Clone(config)
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
		return nil, fmt.Errorf(".sourcegraph/ignore exceeds the %d-byte document limit", documentContentLimit)
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

// streamNativeGitDocuments sends the documents of one captured commit: every
// file but gitlinks and the files that the root .sourcegraph/ignore names.
// The error channel gets one value when the stream ends; missing, when set,
// then holds the blobs that the stream read as missing.
func streamNativeGitDocuments(
	ctx context.Context,
	repoDir string,
	snapshot gitSnapshot,
	missing *[]missingBlob,
) (<-chan fileContent, <-chan error) {
	documents := make(chan fileContent)
	result := make(chan error, 1)
	go func() {
		defer close(documents)
		ignoreEntry, err := readNativeGitRootIgnoreEntry(ctx, repoDir, snapshot)
		if err != nil {
			result <- err
			return
		}
		batch, err := startNativeGitBatch(ctx, repoDir)
		if err != nil {
			result <- err
			return
		}
		matcher, err := readNativeGitIgnoreEntry(batch, ignoreEntry)
		if err != nil {
			_ = batch.close()
			result <- err
			return
		}

		headBranch := []string{"HEAD"}
		err = readNativeGitTree(ctx, repoDir, snapshot, func(entries []gitTreeEntry) error {
			blobs := make([]gitTreeEntry, 0, len(entries))
			for _, entry := range entries {
				if entry.mode != "160000" && !matcher.Match(entry.path) {
					blobs = append(blobs, entry)
				}
			}
			infos, err := batch.check(blobs)
			if err != nil {
				return err
			}
			if missing != nil {
				*missing = append(*missing, missingBlobsOf(infos)...)
			}
			return batch.read(infos, func(document fileContent) error {
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
		result <- err
	}()
	return documents, result
}

// indexNativeGitFull writes one captured commit to indexDir through the shared
// Builder sink. It applies the root .sourcegraph/ignore file, skips gitlinks,
// and emits oversize blob names without their bodies. In a partial clone, a
// missing blob is indexed by name and added to missing when missing is set.
// indexDir is caller-owned staging; the caller validates HEAD and publishes
// it.
func indexNativeGitFull(
	ctx context.Context,
	repoDir string,
	indexDir string,
	snapshot gitSnapshot,
	parallelism int,
	missing *[]missingBlob,
) error {
	if err := requireNativeGitVersion(ctx); err != nil {
		return err
	}
	repository, err := nativeGitRepository(ctx, repoDir, snapshot)
	if err != nil {
		return err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	documents, result := streamNativeGitDocuments(streamCtx, repoDir, snapshot, missing)
	_, buildErr := indexDocumentsWithRepository(
		streamCtx,
		indexDir,
		repository,
		documents,
		parallelism,
		cancel,
	)
	streamErr := <-result
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if buildErr != nil {
		return buildErr
	}
	return streamErr
}
