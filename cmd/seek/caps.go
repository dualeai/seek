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

	// maxCorpusIndexedBytes limits selected content admitted to one folder
	// corpus or one Git index family. Git applies the limit separately to its
	// committed and working-tree families. This is a work limit, not a bound
	// on process memory.
	maxCorpusIndexedBytes = 10 * 1024 * 1024 * 1024 // 10 GiB

	// Git has a higher file-count budget than raw folders because Git
	// provides the file universe and ignore semantics. Standard folders
	// stay lower because they intentionally do not apply
	// .gitignore/vendor/cache rules.
	maxGitCandidateFiles    = 10_000_000
	maxFolderCandidateFiles = 1_000_000

	// Per-file limits. Both reader paths honor maxIndexedDocumentBytes
	// directly so there's one source of truth.
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

	// defaultIndexWindowBytes caps doc weight per windowed rotation in
	// indexDocuments. Each of corpusWorkerCap concurrent consumers may
	// accumulate up to this much pending weight before rotating; the
	// global semaphore must accommodate every consumer's window plus
	// one in-rotation max-sized reader Acquire concurrently. The
	// formula keeps N*window + 2*doc ≤ budget; the compile-time guard below
	// pins this invariant and caps_invariant_test.go reports readable failures.
	defaultIndexWindowBytes = (maxInFlightBytes - 2*maxIndexedDocumentBytes) / (2 * corpusWorkerCap)
)

// indexWindowBytes is the live rotation threshold consumed by
// indexDocuments. Var (not const) so tests can shrink it via
// swapIndexWindowBytesForTest under testReadSemMu — production
// callers treat as read-only.
var indexWindowBytes int64 = defaultIndexWindowBytes

var (
	gitCandidateFileLimit     int64 = maxGitCandidateFiles
	gitCorpusIndexedByteLimit int64 = maxCorpusIndexedBytes
)

// Compile-time invariant: a single max-sized file fits within the
// in-flight budget. Without this, a max-sized Acquire could block
// forever on a non-cancellable context (golang/go#59002). The uint
// cast underflows at compile time if the difference is negative.
const _ = uint(maxInFlightBytes - maxIndexedDocumentBytes)

// Compile-time windowed-fit invariant: N concurrent consumers each
// accumulating up to defaultIndexWindowBytes plus one in-rotation
// max-sized reader Acquire (= 2 × maxIndexedDocumentBytes for the
// in-flight tip + the new reader) must fit within maxInFlightBytes.
// Otherwise readers can wait while every consumer is inside Finish.
const _ = uint(maxInFlightBytes - (corpusWorkerCap*defaultIndexWindowBytes + 2*maxIndexedDocumentBytes))

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
