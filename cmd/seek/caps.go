package main

import (
	"errors"
	"fmt"
)

const (
	// maxIndexedDocumentBytes is the shared per-document limit passed to
	// Zoekt as index.Options.SizeMax. Seek-side readers do not read bodies above
	// this limit, so they do not load content that Zoekt will discard.
	//
	// 100 MiB accommodates vendored libraries, generated JSON/CSV dumps,
	// and large data files that occasionally appear in source trees.
	// For a selected oversize blob, committed Git emits a name-only TooLarge
	// document. Folder and dirty readers skip the file; the dirty reader also
	// logs a warning.
	maxIndexedDocumentBytes = 100 * 1024 * 1024 // 100 MiB

	// maxCorpusIndexedBytes limits content work for one folder corpus or one
	// Git index family. Before ignore filtering, the committed reader counts the
	// sizes of candidate blobs at or below maxIndexedDocumentBytes. The folder
	// and dirty readers count selected content.
	// Git applies the limit separately to its committed and working-tree
	// families. This is a work limit, not a process-memory bound.
	maxCorpusIndexedBytes = 10 * 1024 * 1024 * 1024 // 10 GiB

	// Git has a higher file-count budget than raw folders because Git
	// provides the file universe and ignore semantics. Standard folders
	// stay lower because they intentionally do not apply
	// .gitignore/vendor/cache rules.
	maxGitCandidateFiles    = 10_000_000
	maxFolderCandidateFiles = 1_000_000

	// maxGitDirtyFileSize keeps the dirty reader on the shared per-document
	// limit. maxFolderIndexedBytes keeps the folder reader on the shared
	// aggregate corpus limit.
	maxGitDirtyFileSize   = maxIndexedDocumentBytes
	maxFolderIndexedBytes = maxCorpusIndexedBytes

	// corpusWorkerCap limits active corpus indexers. Each corpus indexer can
	// also use the internal parallelism returned by indexParallelism.
	corpusWorkerCap = 4

	// maxInFlightBytes is the global semaphore capacity for accounted document
	// content across all corpus readers and builders. Capacity grows by six
	// maximum-sized documents per corpus slot; workers do not own separate
	// quotas. Readers acquire weight, and consumers release it after the
	// Builder.Finish that stops retaining the document. Other allocations are
	// not covered, so this value does not bound process RSS.
	maxInFlightBytes = 6 * maxIndexedDocumentBytes * corpusWorkerCap

	// defaultIndexWindowBytes sets the document-weight rotation point in
	// indexDocumentsWithRepository. A consumer rotates when its pending weight
	// reaches or passes this point, so one document can take it past the point.
	// The formula keeps headroom beyond N rotation points: N*window + 2*doc is
	// within the semaphore budget. This is not an exact bound on pending weight
	// or process memory. The compile-time checks below enforce these limits.
	defaultIndexWindowBytes = (maxInFlightBytes - 2*maxIndexedDocumentBytes) / (2 * corpusWorkerCap)
)

// indexWindowBytes is the live rotation threshold for
// indexDocumentsWithRepository and the payload limit for native Git delta
// admission. It is a variable so tests can shrink it through
// swapIndexWindowBytesForTest; production callers treat it as read-only.
var indexWindowBytes int64 = defaultIndexWindowBytes

var (
	gitCandidateFileLimit     int64 = maxGitCandidateFiles
	gitCorpusIndexedByteLimit int64 = maxCorpusIndexedBytes
)

// These casts underflow at compile time if an invariant is false. One worker
// and one document window are the minimum values that keep indexing useful. A
// maximum-sized file must fit in the in-flight budget to prevent a permanent
// Acquire wait on a non-cancellable context (golang/go#59002). The final check
// keeps N rotation points and two maximum-sized documents within that budget.
const (
	_ = uint(corpusWorkerCap - 1)
	_ = uint(defaultIndexWindowBytes - maxIndexedDocumentBytes)
	_ = uint(maxInFlightBytes - maxIndexedDocumentBytes)
	_ = uint(maxInFlightBytes - (corpusWorkerCap*defaultIndexWindowBytes + 2*maxIndexedDocumentBytes))
)

var (
	errGitCapExceeded = errors.New("git cap exceeded")

	// errGitCommittedCapExceeded marks a stable cap result from the immutable
	// committed-tree scan. Only this subtype can be cached by committed HEAD.
	// Working-tree checks return the broader errGitCapExceeded sentinel.
	errGitCommittedCapExceeded = fmt.Errorf("git committed cap exceeded: %w", errGitCapExceeded)
	errFolderCapExceeded       = errors.New("folder cap exceeded")

	// errDeltaPayloadExceedsWindow fires when a delta (working-tree
	// dirty set OR folder-manifest changed set) would exceed
	// indexWindowBytes. Both sites route through a windowed full
	// rebuild via indexDocuments rather than holding the whole payload
	// in indexDeltaDocuments's single terminal Finish.
	errDeltaPayloadExceedsWindow = errors.New("delta payload exceeds window threshold")
)

type indexCapExceededError struct {
	cause   error
	message string
	metric  indexCapMetric
	current int64
	limit   int64
}

type indexCapMetric string

const (
	indexCapCandidateFiles indexCapMetric = "candidate_files"
	indexCapIndexedBytes   indexCapMetric = "indexed_bytes"
)

func (e indexCapExceededError) Error() string {
	return fmt.Sprintf("%s: %s=%d limit=%d", e.message, e.metric, e.current, e.limit)
}

func (e indexCapExceededError) Unwrap() error {
	return e.cause
}

func indexCapError(cause error, message string, metric indexCapMetric, current, limit int64) error {
	return indexCapExceededError{
		cause:   cause,
		message: message,
		metric:  metric,
		current: current,
		limit:   limit,
	}
}

func gitCapError(message string, metric indexCapMetric, current, limit int64) error {
	return indexCapError(errGitCapExceeded, message, metric, current, limit)
}

func gitCommittedCapError(message string, metric indexCapMetric, current, limit int64) error {
	return indexCapError(errGitCommittedCapExceeded, message, metric, current, limit)
}

func folderCapError(message string, metric indexCapMetric, current, limit int64) error {
	return indexCapError(errFolderCapExceeded, message, metric, current, limit)
}
