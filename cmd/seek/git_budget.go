package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type gitIndexBudget struct {
	candidates   int64
	indexedBytes int64
}

type gitTreeBlob struct {
	name string
	oid  string
	size int64
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

func scanGitCommittedIndexBudget(ctx context.Context, repoDir string, maxFiles, maxBytes int64) (gitIndexBudget, error) {
	budget, _, err := scanGitCommittedBudget(ctx, repoDir, "HEAD", nil, maxFiles, maxBytes)
	return budget, err
}

func scanGitCommittedScopeBudgetAt(ctx context.Context, repoDir, treeish string, scope *gitDirtyScope, maxFiles, maxBytes int64) (gitIndexBudget, int, error) {
	return scanGitCommittedBudget(ctx, repoDir, treeish, scope, maxFiles, maxBytes)
}

// scanGitCommittedBudget measures blobs selected from treeish before indexing.
// A nil scope scans the full tree. A non-nil scope supplies include pathspecs
// to Git, then scope.contains applies exclusions to each returned path.
//
// candidates counts every selected blob, including blobs above the document
// size limit. indexedBytes and selected include only blobs that Seek can index.
// Cap failures come from the immutable tree scan and therefore wrap
// errGitCommittedCapExceeded.
func scanGitCommittedBudget(
	ctx context.Context,
	repoDir string,
	treeish string,
	scope *gitDirtyScope,
	maxFiles, maxBytes int64,
) (gitIndexBudget, int, error) {
	args := []string{"ls-tree", "-r", "-l", "-z", treeish}
	if scope != nil {
		args = append(args, "--")
		args = append(args, scope.gitIncludePathspecs()...)
	}
	cmd := gitCmd(ctx, args...)
	cmd.Dir = repoDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return gitIndexBudget{}, 0, fmt.Errorf("open git ls-tree stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return gitIndexBudget{}, 0, fmt.Errorf("start git ls-tree: %w", err)
	}

	var budget gitIndexBudget
	var selected int
	reader := bufio.NewReaderSize(stdout, 64*1024)
	var longRecord []byte
	var readErr error
	for {
		record, err := reader.ReadSlice(0)
		if errors.Is(err, bufio.ErrBufferFull) {
			longRecord = append(longRecord, record...)
			continue
		}
		if len(longRecord) > 0 {
			longRecord = append(longRecord, record...)
			record = longRecord
			longRecord = nil
		}
		if len(record) > 0 {
			blob, ok := parseGitTreeBlobRecord(record)
			if ok && (scope == nil || scope.contains(blob.name)) {
				if err := applyGitTreeBlobBudget(blob, &budget, maxFiles, maxBytes); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return budget, selected, err
				}
				if blob.size <= maxIndexedDocumentBytes {
					selected++
				}
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		readErr = err
		break
	}
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return budget, selected, fmt.Errorf("read git ls-tree: %w", readErr)
	}
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return budget, selected, fmt.Errorf("git ls-tree: %w: %s", err, msg)
		}
		return budget, selected, fmt.Errorf("git ls-tree: %w", err)
	}
	return budget, selected, nil
}

func parseGitTreeBlobRecord(record []byte) (gitTreeBlob, bool) {
	record = bytes.TrimSuffix(record, []byte{0})
	header, _, ok := bytes.Cut(record, []byte{'\t'})
	if !ok {
		return gitTreeBlob{}, false
	}
	fields := bytes.Fields(header)
	if len(fields) < 3 || !bytes.Equal(fields[1], []byte("blob")) {
		return gitTreeBlob{}, false
	}
	_, name, ok := bytes.Cut(record, []byte{'\t'})
	if !ok {
		return gitTreeBlob{}, false
	}
	size := int64(maxIndexedDocumentBytes + 1)
	if len(fields) >= 4 {
		if parsed, err := strconv.ParseInt(string(fields[3]), 10, 64); err == nil {
			size = parsed
		}
	}
	return gitTreeBlob{
		name: string(name),
		oid:  string(fields[2]),
		size: size,
	}, true
}

func applyGitTreeBlobBudget(blob gitTreeBlob, budget *gitIndexBudget, maxFiles, maxBytes int64) error {
	budget.candidates++
	if budget.candidates > maxFiles {
		return gitCommittedCapError("git committed file cap exceeded", indexCapCandidateFiles, budget.candidates, maxFiles)
	}
	if blob.size > maxIndexedDocumentBytes {
		return nil
	}
	budget.indexedBytes += blob.size
	if budget.indexedBytes > maxBytes {
		return gitCommittedCapError("git committed indexed byte cap exceeded", indexCapIndexedBytes, budget.indexedBytes, maxBytes)
	}
	return nil
}
