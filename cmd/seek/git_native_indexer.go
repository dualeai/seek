package main

import (
	"context"
	"fmt"

	"github.com/sourcegraph/zoekt"
)

// captureGitSnapshot verifies the form of a full SHA-1 or SHA-256 commit ID.
// It runs no Git process. The git status call that captured the ID read the
// commit and its tree, and that call fails ("bad object HEAD") when either is
// missing.
// A commit that disappears after that call fails the build that reads it, and
// the build then checks the commit (capturedCommitMissing). A separate check
// here costs one Git process per committed build and delta. Only a build that
// reads the commit calls it (gitBuildPlan.snapshot), and such a build has a
// commit: the gitNoHead value is an error.
func captureGitSnapshot(captured string) (gitObjectID, error) {
	oid, err := parseGitObjectID([]byte(captured))
	if err != nil {
		return "", fmt.Errorf("captured Git commit: %w", err)
	}
	return oid, nil
}

// capturedCommitMissing reports whether the captured commit of a build that
// read it is no longer in the repository. It runs one git cat-file, and only
// after such a build failed. A failed check reports false, so the build keeps
// its other error handling.
func capturedCommitMissing(ctx context.Context, repoDir string, build gitBuildPlan, snapshot gitObjectID) bool {
	if !build.snapshot() {
		return false
	}
	exists, err := nativeGitCommitExists(ctx, repoDir, snapshot)
	return err == nil && !exists
}

// nativeGitRepository makes the provider-neutral metadata of one captured
// commit: the opaque name, the work-tree source, and the HEAD branch. It reads
// no zoekt.* Git config and no remote, so it sets no URL, ID, or raw config.
// Seek shows none of these fields, and a config read costs one Git process per
// committed build and delta. The repo: filter matches the opaque name. It
// sets no repository rank and no commit time. Only Zoekt's classic scorer
// reads the rank, and no Zoekt search reads the commit time. Every Seek search
// uses BM25 (searchOpts.UseBM25Scoring).
func nativeGitRepository(repoDir string, snapshot gitObjectID) zoekt.Repository {
	return zoekt.Repository{
		Name:     fallbackGitRepositoryName(repoDir),
		Source:   repoDir,
		Branches: []zoekt.RepositoryBranch{{Name: "HEAD", Version: snapshot.String()}},
	}
}

// streamNativeGitDocuments sends the documents of one captured commit: every
// committed file but gitlinks. Seek indexes all content, so no path exclusion
// file, such as .sourcegraph/ignore, applies. Thus the build needs no Git
// process to read such a file.
// The error channel gets one value when the stream ends. A non-nil missing
// then holds the blobs that the stream read as missing. The stream adds to a
// non-nil contentBytes the bytes that the shard documents of the sent
// documents hold (heldContentBytes). It adds them before the error channel
// gets its value. A binary blob counts as its marker.
func streamNativeGitDocuments(
	ctx context.Context,
	repoDir string,
	snapshot gitObjectID,
	missing *[]missingBlob,
	contentBytes *int64,
) (<-chan fileContent, <-chan error) {
	documents := make(chan fileContent)
	result := make(chan error, 1)
	go func() {
		defer close(documents)
		batch, err := startNativeGitBatch(ctx, repoDir)
		if err != nil {
			result <- err
			return
		}

		headBranch := []string{"HEAD"}
		err = readNativeGitTree(ctx, repoDir, snapshot, func(entries []gitTreeEntry) error {
			blobs := make([]gitTreeEntry, 0, len(entries))
			for _, entry := range entries {
				if entry.mode != "160000" {
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
				held := heldContentBytes(document)
				select {
				case documents <- document:
					if contentBytes != nil {
						*contentBytes += held
					}
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
// Builder sink. It indexes every committed blob, skips gitlinks, and emits
// oversize blob names without their bodies. In a partial clone, it indexes a
// missing blob by name, and adds it to a non-nil missing. indexDir is
// caller-owned staging; the caller validates HEAD and publishes it. It returns
// the content bytes that it indexed, the live bytes of the committed state
// (deltaNeedsCompaction).
func indexNativeGitFull(
	ctx context.Context,
	repoDir string,
	indexDir string,
	snapshot gitObjectID,
	parallelism int,
	missing *[]missingBlob,
) (int64, error) {
	if err := requireNativeGitVersion(ctx); err != nil {
		return 0, err
	}
	repository := nativeGitRepository(repoDir, snapshot)
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var contentBytes int64
	documents, result := streamNativeGitDocuments(streamCtx, repoDir, snapshot, missing, &contentBytes)
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
		return 0, ctxErr
	}
	if buildErr != nil {
		return 0, buildErr
	}
	return contentBytes, streamErr
}
