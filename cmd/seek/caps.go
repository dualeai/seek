package main

import (
	"errors"
	"fmt"
)

const (
	// maxIndexedDocumentBytes is the shared per-document limit passed to
	// Zoekt as index.Options.SizeMax. Every seek-side reader rejects files
	// larger than this so we never read content Zoekt will discard.
	//
	// 100 MiB accommodates vendored libraries, generated JSON/CSV dumps,
	// and large data files that occasionally appear in source trees.
	// Files above this cap are skipped at read time with a slog.Warn.
	maxIndexedDocumentBytes = 100 * 1024 * 1024 // 100 MiB

	// maxCorpusIndexedBytes is the shared selected-content budget per
	// planned corpus index. Bounds foreground indexing work for both Git
	// and standard folder corpora.
	maxCorpusIndexedBytes = 5 * 1024 * 1024 * 1024 // 5 GiB

	// Git has a higher file-count budget than raw folders because Git
	// provides the file universe and ignore semantics. Standard folders
	// stay lower because they intentionally do not apply
	// .gitignore/vendor/cache rules.
	maxGitCandidateFiles    = 10_000_000
	maxFolderCandidateFiles = 1_000_000

	// Per-file limits. Both reader paths honor maxIndexedDocumentBytes so
	// the same cap propagates everywhere via one source of truth.
	maxGitDirtyFileSize   = maxIndexedDocumentBytes
	maxFolderFileSize     = maxIndexedDocumentBytes
	maxFolderIndexedBytes = maxCorpusIndexedBytes

	// inFlightHeadroomFiles sets how many max-sized files may be resident
	// in reader buffers concurrently. The readSemaphore budget is derived
	// from this so bumping maxIndexedDocumentBytes automatically scales
	// the in-flight ceiling.
	//
	// 6 means at most six max-sized reads can sit between the reader
	// pool and Zoekt's builder. Smaller → tighter ceiling, more Acquire
	// blocks under bursts of large files. Larger → looser ceiling, more
	// peak RSS under those bursts.
	inFlightHeadroomFiles = 6

	// maxInFlightBytes bounds total bytes resident in reader buffers
	// across all reader workers (Git dirty + folder candidates). Acquire
	// on the reader side; release in the consumer (indexDocuments /
	// indexDeltaDocuments) strictly after builder.Finish() returns,
	// because Zoekt's async shard writers retain Content references
	// until then (zoekt/index/builder.go:659-667 — Finish calls
	// b.building.Wait() before returning).
	maxInFlightBytes = inFlightHeadroomFiles * maxIndexedDocumentBytes
)

// Compile-time invariant: a single max-sized file must fit within the
// in-flight budget, otherwise Acquire(maxIndexedDocumentBytes) would
// block forever under a non-cancellable context (golang/go#59002). The
// cast to uint underflows at compile time if the difference is negative.
const _ = uint(maxInFlightBytes - maxIndexedDocumentBytes)

var (
	errGitCapExceeded    = errors.New("git cap exceeded")
	errFolderCapExceeded = errors.New("folder cap exceeded")
)

type indexCapExceededError struct {
	cause   error
	message string
	metric  string
	current int64
	limit   int64
}

func (e indexCapExceededError) Error() string {
	return fmt.Sprintf("%s: %s=%d limit=%d", e.message, e.metric, e.current, e.limit)
}

func (e indexCapExceededError) Unwrap() error {
	return e.cause
}

func indexCapError(cause error, message, metric string, current, limit int64) error {
	return indexCapExceededError{
		cause:   cause,
		message: message,
		metric:  metric,
		current: current,
		limit:   limit,
	}
}

func gitCapError(message, metric string, current, limit int64) error {
	return indexCapError(errGitCapExceeded, message, metric, current, limit)
}

func folderCapError(message, metric string, current, limit int64) error {
	return indexCapError(errFolderCapExceeded, message, metric, current, limit)
}
