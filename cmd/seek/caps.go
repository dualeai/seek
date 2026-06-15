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

	// maxInFlightBytes bounds total bytes resident in reader buffers
	// across all reader workers. Acquired in readers, Released by the
	// consumer after the Builder.Finish() that buffered the doc — see
	// fileContent for the per-doc Release contract.
	maxInFlightBytes = 6 * maxIndexedDocumentBytes

	// defaultIndexWindowBytes caps doc weight per windowed rotation in
	// indexDocuments. Half of maxInFlightBytes leaves headroom for one
	// max-sized reader Acquire to fit alongside a fully-pending window.
	defaultIndexWindowBytes = maxInFlightBytes / 2
)

// indexWindowBytes is the live rotation threshold consumed by
// indexDocuments. Var (not const) so tests can shrink it via
// swapIndexWindowBytesForTest under testReadSemMu — production
// callers treat as read-only.
var indexWindowBytes int64 = defaultIndexWindowBytes

// Compile-time invariant: a single max-sized file fits within the
// in-flight budget. Without this, a max-sized Acquire could block
// forever on a non-cancellable context (golang/go#59002). The uint
// cast underflows at compile time if the difference is negative.
const _ = uint(maxInFlightBytes - maxIndexedDocumentBytes)

// Compile-time windowed-fit invariant: the consumer's pending window
// plus the tipping doc plus one in-rotation reader Acquire (= 2 ×
// maxIndexedDocumentBytes) must fit within maxInFlightBytes,
// otherwise the reader wedges while the consumer is in Finish.
const _ = uint(maxInFlightBytes - (defaultIndexWindowBytes + 2*maxIndexedDocumentBytes))

var (
	errGitCapExceeded    = errors.New("git cap exceeded")
	errFolderCapExceeded = errors.New("folder cap exceeded")

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
