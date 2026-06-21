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
	//
	// Bumped from 5 GiB to 10 GiB to give dev-parent folder corpora more
	// headroom after nested-git discovery carves out per-repo subtrees.
	// Discovery is the primary fix for the cap-exhausted UX; the 10 GiB
	// ceiling is a belt-and-suspenders cushion for pathological folders
	// whose remaining non-git content still exceeds 5 GiB.
	//
	// Peak RSS during indexing is independently bounded by
	// maxInFlightBytes (600 MiB × corpusWorkerCap) — raising this cap
	// does NOT change peak memory, only total content admitted.
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

	// corpusWorkerCap bounds the number of corpus indexers that may run
	// concurrently. 4 balances wall-clock vs peak memory: empirical
	// benchmarks showed ~50% reduction on multi-corpus workloads while
	// keeping the peak in-flight budget at 2.4 GiB (4 × 6 × 100 MiB).
	// Increasing further yields diminishing returns because per-corpus
	// indexing already saturates min(NumCPU, 16) builders internally
	// (indexer.go:indexParallelism).
	corpusWorkerCap = 4

	// maxInFlightBytes bounds total bytes resident in reader buffers
	// across all reader workers. Per-worker budget is 600 MiB (six
	// max-sized 100 MiB files) — empirical headroom that keeps the
	// windowed-fit invariant satisfied at any corpusWorkerCap ≥ 1.
	// Acquired in readers, Released by the consumer after the
	// Builder.Finish() that buffered the doc — see fileContent for the
	// per-doc Release contract.
	maxInFlightBytes = 6 * maxIndexedDocumentBytes * corpusWorkerCap

	// defaultIndexWindowBytes caps doc weight per windowed rotation in
	// indexDocuments. Each of corpusWorkerCap concurrent consumers may
	// accumulate up to this much pending weight before rotating; the
	// global semaphore must accommodate every consumer's window plus
	// one in-rotation max-sized reader Acquire concurrently. The
	// formula keeps N*window + 2*doc ≤ budget for any N ≥ 1; the
	// compile-time guard below pins this invariant and
	// caps_invariant_test.go asserts it at runtime.
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
// Otherwise readers wedge while consumers are in Finish — the original
// 3-way deadlock at N=1 scales to an N+1-way deadlock at N>1.
const _ = uint(maxInFlightBytes - (corpusWorkerCap*defaultIndexWindowBytes + 2*maxIndexedDocumentBytes))

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
