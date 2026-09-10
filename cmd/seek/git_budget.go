package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type gitIndexBudget struct {
	candidates   int64
	indexedBytes int64
}

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

func gitCorpusError(repoDir, indexDir string, err error) error {
	if err == nil {
		return nil
	}
	return &gitCorpusContextError{root: repoDir, indexDir: indexDir, cause: err}
}

func checkGitDirtyFileBudget(repoDir, indexDir string, files []string) error {
	return checkGitDirtyFileBudgetWithLimits(repoDir, indexDir, files, gitCandidateFileLimit, gitCorpusIndexedByteLimit)
}

func checkGitDirtyFileBudgetWithLimits(repoDir, indexDir string, files []string, maxFiles, maxBytes int64) error {
	if len(files) == 0 {
		return nil
	}
	budget := gitIndexBudget{candidates: int64(len(files))}
	if budget.candidates > maxFiles {
		return gitCorpusError(
			repoDir,
			indexDir,
			gitCapError("git dirty file cap exceeded", indexCapCandidateFiles, budget.candidates, maxFiles),
		)
	}
	for _, name := range files {
		info, err := os.Lstat(filepath.Join(repoDir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxGitDirtyFileSize {
			continue
		}
		budget.indexedBytes += info.Size()
		if budget.indexedBytes > maxBytes {
			return gitCorpusError(
				repoDir,
				indexDir,
				gitCapError("git dirty indexed byte cap exceeded", indexCapIndexedBytes, budget.indexedBytes, maxBytes),
			)
		}
	}
	return nil
}
