package main

import (
	"errors"
	"fmt"
)

const (
	// maxIndexedDocumentBytes is the shared per-document limit passed to
	// Zoekt. Keep every seek-side reader aligned with index.Options.SizeMax so
	// seek does not read content that Zoekt will discard immediately.
	maxIndexedDocumentBytes = 10 * 1024 * 1024 // 10 MiB

	// maxCorpusIndexedBytes is the shared selected-content budget per planned
	// corpus index. It bounds foreground indexing work for both Git and
	// standard folder corpora.
	maxCorpusIndexedBytes = 5 * 1024 * 1024 * 1024 // 5 GiB

	// Git has a higher file-count budget than raw folders because Git provides
	// the file universe and ignore semantics. Standard folders remain lower
	// because they intentionally do not apply .gitignore/vendor/cache rules.
	maxGitCandidateFiles    = 10_000_000
	maxFolderCandidateFiles = 1_000_000

	maxGitDirtyFileSize   = maxIndexedDocumentBytes
	maxFolderFileSize     = maxIndexedDocumentBytes
	maxFolderIndexedBytes = maxCorpusIndexedBytes
)

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
