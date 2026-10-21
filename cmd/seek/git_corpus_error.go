package main

import "fmt"

// gitCorpusContextError adds the root and index directory of a Git node to an
// error.
type gitCorpusContextError struct {
	root     string
	indexDir string
	cause    error
}

func (e *gitCorpusContextError) Error() string {
	return fmt.Sprintf("git corpus root=%q index=%q: %v", e.root, e.indexDir, e.cause)
}

func (e *gitCorpusContextError) Unwrap() error {
	return e.cause
}

// gitCorpusError wraps err with the node's root and index directory. A nil err
// stays nil.
func gitCorpusError(repoDir, indexDir string, err error) error {
	if err == nil {
		return nil
	}
	return &gitCorpusContextError{root: repoDir, indexDir: indexDir, cause: err}
}
