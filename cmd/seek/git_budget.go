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

func gitCorpusError(repoDir, indexDir string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("git corpus root=%q index=%q: %w", repoDir, indexDir, err)
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
			gitCapError("git dirty file cap exceeded", "candidate_files", budget.candidates, maxFiles),
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
				gitCapError("git dirty indexed byte cap exceeded", "indexed_bytes", budget.indexedBytes, maxBytes),
			)
		}
	}
	return nil
}

func scanGitCommittedIndexBudget(ctx context.Context, repoDir string, maxFiles, maxBytes int64) (gitIndexBudget, error) {
	cmd := gitCmd(ctx, "ls-tree", "-r", "-l", "-z", "HEAD")
	cmd.Dir = repoDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return gitIndexBudget{}, fmt.Errorf("open git ls-tree stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return gitIndexBudget{}, fmt.Errorf("start git ls-tree: %w", err)
	}

	var budget gitIndexBudget
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
			if err := applyGitTreeBudgetRecord(record, &budget, maxFiles, maxBytes); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return budget, err
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
		return budget, fmt.Errorf("read git ls-tree: %w", readErr)
	}
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return budget, fmt.Errorf("git ls-tree: %w: %s", err, msg)
		}
		return budget, fmt.Errorf("git ls-tree: %w", err)
	}
	return budget, nil
}

func scanGitCommittedScopeBudgetAt(ctx context.Context, repoDir, treeish string, scope *gitDirtyScope, maxFiles, maxBytes int64) (gitIndexBudget, int, error) {
	return scanGitCommittedScope(ctx, repoDir, treeish, scope, maxFiles, maxBytes)
}

func scanGitCommittedScope(
	ctx context.Context,
	repoDir string,
	treeish string,
	scope *gitDirtyScope,
	maxFiles, maxBytes int64,
) (gitIndexBudget, int, error) {
	args := []string{"ls-tree", "-r", "-l", "-z", treeish, "--"}
	args = append(args, scope.gitIncludePathspecs()...)
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

func applyGitTreeBudgetRecord(record []byte, budget *gitIndexBudget, maxFiles, maxBytes int64) error {
	blob, ok := parseGitTreeBlobRecord(record)
	if !ok {
		return nil
	}
	return applyGitTreeBlobBudget(blob, budget, maxFiles, maxBytes)
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
		return gitCapError("git committed file cap exceeded", "candidate_files", budget.candidates, maxFiles)
	}
	if blob.size > maxIndexedDocumentBytes {
		return nil
	}
	budget.indexedBytes += blob.size
	if budget.indexedBytes > maxBytes {
		return gitCapError("git committed indexed byte cap exceeded", "indexed_bytes", budget.indexedBytes, maxBytes)
	}
	return nil
}
