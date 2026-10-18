package main

import (
	"errors"
	"fmt"
	"math"
)

const (
	// zoektShardFormatBytes is the most content and the largest shard file that
	// Zoekt can address: its offsets are uint32 (zoekt/index/section.go and
	// zoekt/index/shard_builder.go). It is a format limit of the index, not a
	// file-size policy. Seek sets no other per-document size limit: readers send
	// larger content by name only, and guardWrappedShards keeps each shard file
	// within this limit.
	zoektShardFormatBytes = math.MaxUint32

	// maxCorpusIndexedBytes limits content work for one folder corpus or one
	// Git index family. Before ignore filtering, the committed reader counts the
	// sizes of candidate blobs whose content fits the index
	// (documentContentFits). The folder and dirty readers count selected
	// content.
	// Git applies the limit separately to its committed and working-tree
	// families. This is a work limit, not a process-memory bound.
	maxCorpusIndexedBytes = 10 * 1024 * 1024 * 1024 // 10 GiB

	// Git has a higher file-count budget than raw folders because Git
	// provides the file universe and ignore semantics. Standard folders
	// stay lower because they intentionally do not apply
	// .gitignore/vendor/cache rules.
	maxGitCandidateFiles    = 10_000_000
	maxFolderCandidateFiles = 1_000_000

	// maxFolderIndexedBytes keeps the folder reader on the shared aggregate
	// corpus limit.
	maxFolderIndexedBytes = maxCorpusIndexedBytes

	// maxReadWeightBytes is the largest readSemaphore weight one document
	// takes (readWeight). The smallest read budget, maxInFlightBytes, is sized
	// in these units, not by the document size limit.
	maxReadWeightBytes = 100 * 1024 * 1024 // 100 MiB

	// corpusWorkerCap limits active corpus indexers. Each corpus indexer can
	// also use the internal parallelism returned by indexParallelism.
	corpusWorkerCap = 4

	// maxInFlightBytes is the smallest readSemaphore capacity: six
	// maximum-weight documents per corpus slot. readBudgetBytes grows the
	// capacity with the host's memory.
	maxInFlightBytes = 6 * maxReadWeightBytes * corpusWorkerCap

	// defaultIndexWindowBytes is the rotation point for the smallest budget,
	// maxInFlightBytes. The compile-time checks below prove the window rules
	// with it; indexWindowBytes applies the same formula to the real budget.
	defaultIndexWindowBytes = (maxInFlightBytes - 2*maxReadWeightBytes) / (2 * corpusWorkerCap)
)

// readBudgetBytes is the global readSemaphore capacity for accounted document
// content across all corpus readers and builders; workers do not own separate
// quotas. Readers acquire weight, and consumers release it after the
// Builder.Finish that stops retaining the document. Seek's objective is the
// shortest time, so the budget grows with the host: a quarter of its physical
// memory, and at least maxInFlightBytes. Zoekt needs several times the content
// it builds, so the rest of memory is left for the builds. Other allocations
// are not covered, so this value does not bound process RSS.
var readBudgetBytes = max(int64(maxInFlightBytes), hostPhysicalMemory()/4)

// indexWindowBytes is the live document-weight rotation point for
// indexDocumentsWithRepository and the payload limit for native Git delta
// admission. A consumer rotates when its pending weight reaches or passes this
// point, so one document can take it past the point. The formula keeps headroom
// beyond N rotation points: N*window + 2*doc is within readBudgetBytes. This is
// not an exact bound on pending weight or process memory. It is a variable so
// tests can shrink it through swapIndexWindowBytesForTest; production callers
// treat it as read-only.
var indexWindowBytes = (readBudgetBytes - 2*maxReadWeightBytes) / (2 * corpusWorkerCap)

// documentContentLimit is the most content seek gives Zoekt for one document:
// zoektShardFormatBytes, because one shard cannot hold more. Every reader sends
// a larger file by name only (index.SkipReasonTooLarge), so Zoekt stores its
// NOT-INDEXED marker and the name stays searchable. It is a variable so tests
// can swap it; production callers treat it as read-only.
var documentContentLimit int64 = zoektShardFormatBytes

// shardFormatLimit is the largest shard file seek keeps: zoektShardFormatBytes.
// guardWrappedShards treats a larger file as wrapped. It is a variable so tests
// can swap it; production callers treat it as read-only.
var shardFormatLimit int64 = zoektShardFormatBytes

// documentContentFits reports whether a document of size bytes can keep its
// content in the index.
func documentContentFits(size int64) bool {
	return size <= documentContentLimit
}

var (
	gitCandidateFileLimit     int64 = maxGitCandidateFiles
	gitCorpusIndexedByteLimit int64 = maxCorpusIndexedBytes
)

// These casts underflow at compile time if an invariant is false. One worker
// and one document window are the minimum values that keep indexing useful. A
// maximum-weight document must fit in the in-flight budget to prevent a
// permanent Acquire wait on a non-cancellable context (golang/go#59002). The
// final check keeps N rotation points and two maximum-weight documents within
// that budget.
const (
	_ = uint(corpusWorkerCap - 1)
	_ = uint(defaultIndexWindowBytes - maxReadWeightBytes)
	_ = uint(maxInFlightBytes - maxReadWeightBytes)
	_ = uint(maxInFlightBytes - (corpusWorkerCap*defaultIndexWindowBytes + 2*maxReadWeightBytes))
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
