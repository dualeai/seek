package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sourcegraph/zoekt/gitindex"
	"github.com/sourcegraph/zoekt/index"
)

// computeStateHash computes the MD5 hash of HEAD SHA + git status porcelain output.
func computeStateHash(headSHA, statusOutput string) string {
	h := md5.New()
	h.Write([]byte(headSHA))
	h.Write([]byte(statusOutput))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// readStateFile reads the cached state hash from .zoekt-index/.state.
func readStateFile(indexDir string) string {
	data, err := os.ReadFile(filepath.Join(indexDir, ".state"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeStateFile atomically writes the state hash to .zoekt-index/.state.
func writeStateFile(indexDir, state string) error {
	tmpPath := filepath.Join(indexDir, ".state.tmp")
	if err := os.WriteFile(tmpPath, []byte(state), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(indexDir, ".state"))
}

// deleteStateFiles removes .state and .state.tmp.
func deleteStateFiles(indexDir string) {
	_ = os.Remove(filepath.Join(indexDir, ".state"))
	_ = os.Remove(filepath.Join(indexDir, ".state.tmp"))
}

// indexParallelism returns the number of parallel indexing workers.
func indexParallelism() int {
	p := runtime.NumCPU()
	if p > 16 {
		p = 16
	}
	if p < 1 {
		p = 1
	}
	return p
}

// checkCtags verifies that universal-ctags is installed. Zoekt silently skips
// symbol parsing when ctags is missing (even with CTagsMustSucceed), so we
// must detect this explicitly (§9).
func checkCtags() error {
	var opts index.Options
	opts.SetDefaults()
	if opts.CTagsPath == "" {
		return fmt.Errorf("universal-ctags required but not found.\n  macOS:  brew install universal-ctags\n  Linux:  sudo apt-get install universal-ctags\n  Or set CTAGS_COMMAND=/path/to/ctags")
	}
	return nil
}

// runIndexing orchestrates committed and uncommitted indexing with locking.
func runIndexing(ctx context.Context, repoDir, indexDir, headSHA, statusOutput, preState string) error {
	// §9: Fail fast if ctags is missing — fatal error
	if err := checkCtags(); err != nil {
		return err
	}

	lockPath := filepath.Join(indexDir, ".lock")

	// Ensure partial state file is cleaned up on all exit paths
	defer func() { _ = os.Remove(filepath.Join(indexDir, ".state.tmp")) }()

	lockFd, acquired, err := acquireLock(ctx, indexDir, lockPath)
	if err != nil {
		return err
	}
	if !acquired {
		// Lock not acquired but shards exist — use stale index
		fmt.Fprintln(os.Stderr, "Warning: another process is indexing, using existing index")
		return nil
	}
	defer releaseLock(lockFd)

	// Double-check state after acquiring lock
	cachedState := readStateFile(indexDir)
	if cachedState == preState {
		return nil
	}

	parallelism := indexParallelism()
	uncommittedFiles := parseGitStatusFiles(statusOutput)

	// Prepare uncommitted files in parallel with committed indexing
	type hardlinkResult struct {
		tempDir string
		err     error
	}
	hardlinkCh := make(chan hardlinkResult, 1)

	if len(uncommittedFiles) > 0 {
		go func() {
			tmpDir, err := prepareUncommittedFiles(repoDir, uncommittedFiles, parallelism)
			hardlinkCh <- hardlinkResult{tmpDir, err}
		}()
	} else {
		hardlinkCh <- hardlinkResult{"", nil}
	}

	// Index committed files
	committedErr := indexCommitted(ctx, repoDir, indexDir, parallelism)
	if committedErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: committed indexing failed: %v\n", committedErr)
	}

	// Wait for hardlink preparation
	hlResult := <-hardlinkCh

	// §5.5 step 2: Always clean stale uncommitted shards before rebuilding
	// (covers both current "uncommitted" and legacy ".zoekt-uncommitted" naming)
	cleanUncommittedShards(indexDir)

	// §5.5 steps 3-7: Index uncommitted files (sequential, after committed)
	if len(uncommittedFiles) > 0 {
		if hlResult.err != nil {
			fmt.Fprintf(os.Stderr, "Warning: uncommitted file preparation failed: %v\n", hlResult.err)
		} else if hlResult.tempDir != "" {
			defer func() { _ = os.RemoveAll(hlResult.tempDir) }()
			if err := indexUncommitted(ctx, indexDir, hlResult.tempDir, parallelism); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: uncommitted indexing failed: %v\n", err)
			}
		}
	}

	// §5.7: Post-indexing verification
	postHeadSHA := gitHeadSHA(ctx)
	postStatus := gitStatusPorcelain(ctx)
	postState := computeStateHash(postHeadSHA, postStatus)

	if committedErr != nil {
		// §5.7 step 4: Don't cache state, but do NOT exit — proceed to search
		deleteStateFiles(indexDir)
		fmt.Fprintln(os.Stderr, "Warning: index incomplete, will re-index on next search")
		return nil
	}

	if postState == preState {
		if err := writeStateFile(indexDir, preState); err != nil {
			return fmt.Errorf("write state file: %w", err)
		}
	} else {
		deleteStateFiles(indexDir)
		fmt.Fprintln(os.Stderr, "Warning: index may be stale, will re-index on next search")
	}

	return nil
}

// acquireLock tries to acquire an exclusive flock on the lock file.
// Returns (fd, acquired, error).
func acquireLock(ctx context.Context, indexDir, lockPath string) (*os.File, bool, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}

	// Try non-blocking lock first
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return f, true, nil
	}

	// Lock held by another process
	if shardsExist(indexDir) {
		// Stale index is acceptable
		_ = f.Close()
		return nil, false, nil
	}

	// No shards exist (first run) — block with timeout
	type lockResult struct {
		err error
	}
	ch := make(chan lockResult, 1)
	go func() {
		ch <- lockResult{syscall.Flock(int(f.Fd()), syscall.LOCK_EX)}
	}()

	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 60*time.Second)
	defer timeoutCancel()

	select {
	case result := <-ch:
		if result.err != nil {
			_ = f.Close()
			return nil, false, fmt.Errorf("flock: %w", result.err)
		}
		return f, true, nil
	case <-timeoutCtx.Done():
		_ = f.Close()
		return nil, false, fmt.Errorf("indexer lock held >60s")
	}
}

// releaseLock releases the flock and closes the file.
func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// shardsExist checks if any *.zoekt shard files exist in the index directory.
func shardsExist(indexDir string) bool {
	entries, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	return err == nil && len(entries) > 0
}

// indexCommitted indexes committed files using gitindex.IndexGitRepo.
func indexCommitted(ctx context.Context, repoDir, indexDir string, parallelism int) error {
	opts := gitindex.Options{
		RepoDir:     repoDir,
		Incremental: true,
		Branches:    []string{"HEAD"},
		BuildOptions: index.Options{
			IndexDir:         indexDir,
			Parallelism:      parallelism,
			CTagsMustSucceed: true,
		},
	}
	_, err := gitindex.IndexGitRepo(opts)
	return err
}

// prepareUncommittedFiles hardlinks uncommitted files into a temp directory.
func prepareUncommittedFiles(repoDir string, files []string, parallelism int) (string, error) {
	tmpDir := filepath.Join(repoDir, ".zoekt-uncommitted")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}

	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, file := range files {
		wg.Add(1)
		go func(f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			srcPath := filepath.Join(repoDir, f)
			dstPath := filepath.Join(tmpDir, f)

			// Skip files that don't exist on disk (deleted files)
			if _, err := os.Stat(srcPath); os.IsNotExist(err) {
				return
			}

			// Create parent directories
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}

			// Try hardlink first, fallback to copy
			if err := os.Link(srcPath, dstPath); err != nil {
				if err := copyFile(srcPath, dstPath); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}(file)
	}

	wg.Wait()
	if firstErr != nil {
		_ = os.RemoveAll(tmpDir)
		return "", firstErr
	}

	return tmpDir, nil
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// indexUncommitted indexes uncommitted files using index.NewBuilder.
func indexUncommitted(ctx context.Context, indexDir, tempDir string, parallelism int) error {
	opts := index.Options{
		IndexDir:         indexDir,
		Parallelism:      parallelism,
		CTagsMustSucceed: true,
	}
	opts.RepositoryDescription.Name = "uncommitted"
	opts.RepositoryDescription.Source = tempDir
	opts.SetDefaults()

	builder, err := index.NewBuilder(opts)
	if err != nil {
		return fmt.Errorf("create builder: %w", err)
	}

	err = filepath.WalkDir(tempDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(tempDir, path)
		if err != nil {
			return err
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return builder.Add(index.Document{
			Name:    relPath,
			Content: content,
		})
	})
	if err != nil {
		return fmt.Errorf("walk uncommitted files: %w", err)
	}

	return builder.Finish()
}

// cleanUncommittedShards removes stale uncommitted shard files.
func cleanUncommittedShards(indexDir string) {
	matches, err := filepath.Glob(filepath.Join(indexDir, "*uncommitted*.zoekt"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
