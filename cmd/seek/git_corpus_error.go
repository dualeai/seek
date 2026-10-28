package main

import "fmt"

// gitCorpusError wraps err with the node's root and index directory. A nil err
// stays nil.
func gitCorpusError(repoDir, indexDir string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("git corpus root=%q index=%q: %w", repoDir, indexDir, err)
}
