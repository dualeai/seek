package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/sourcegraph/zoekt/gitindex"
	"github.com/sourcegraph/zoekt/index"
)

const (
	// cacheDir is the directory name for seek's cache within the repo root.
	cacheDir = ".seek-cache"
	// stateFile stores the hash of the last indexed git state.
	stateFile = ".state"
	// stateTmpFile is used for atomic writes of the state file.
	stateTmpFile = ".state.tmp"
	// lockFile is used for mutual exclusion during indexing.
	lockFile = ".lock"
	// repoUncommitted is the zoekt repository name for uncommitted file shards.
	// Also used as the staging subdirectory name within cacheDir.
	repoUncommitted = "uncommitted"
	// stateVersion is the prefix used in state hashing to invalidate caches
	// when the hash algorithm or input format changes.
	stateVersion = "v3\x00"
)

// computeStateHash computes the xxHash64 of raw git status v2 output.
// The raw output already contains the HEAD SHA in the # branch.oid header,
// so no domain separator is needed.
func computeStateHash(rawOutput string) string {
	h := xxhash.New()
	_, _ = h.Write([]byte(stateVersion))
	_, _ = h.Write([]byte(rawOutput))
	return fmt.Sprintf("%016x", h.Sum64())
}

// readStateFile reads the cached state hash from the stateFile in indexDir.
func readStateFile(indexDir string) string {
	data, err := os.ReadFile(filepath.Join(indexDir, stateFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeStateFile atomically writes the state hash to the stateFile in indexDir.
func writeStateFile(indexDir, state string) error {
	tmpPath := filepath.Join(indexDir, stateTmpFile)
	if err := os.WriteFile(tmpPath, []byte(state), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(indexDir, stateFile))
}

// deleteStateFiles removes .state and .state.tmp.
func deleteStateFiles(indexDir string) {
	_ = os.Remove(filepath.Join(indexDir, stateFile))
	_ = os.Remove(filepath.Join(indexDir, stateTmpFile))
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
func runIndexing(ctx context.Context, repoDir, indexDir string, state repoState, preState string) error {
	// §9: Fail fast if ctags is missing — fatal error
	if err := checkCtags(); err != nil {
		return err
	}

	lockPath := filepath.Join(indexDir, lockFile)

	// Ensure partial state file is cleaned up on all exit paths
	defer func() { _ = os.Remove(filepath.Join(indexDir, stateTmpFile)) }()

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

	// Prepare uncommitted files in parallel with committed indexing
	type stagingResult struct {
		tempDir string
		err     error
	}
	stagingCh := make(chan stagingResult, 1)

	if len(state.Files) > 0 {
		go func() {
			tmpDir, err := prepareUncommittedFiles(repoDir, indexDir, state.Files, parallelism)
			stagingCh <- stagingResult{tmpDir, err}
		}()
	} else {
		stagingCh <- stagingResult{"", nil}
	}

	// Index committed files
	committedErr := indexCommitted(ctx, repoDir, indexDir, parallelism)
	if committedErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: committed indexing failed: %v\n", committedErr)
	}

	// Wait for uncommitted file staging
	stgResult := <-stagingCh

	// §5.5 step 2: Always clean stale uncommitted shards before rebuilding
	cleanUncommittedShards(indexDir)

	// §5.5 steps 3-7: Index uncommitted files (sequential, after committed)
	if len(state.Files) > 0 {
		if stgResult.err != nil {
			fmt.Fprintf(os.Stderr, "Warning: uncommitted file preparation failed: %v\n", stgResult.err)
		} else if stgResult.tempDir != "" {
			defer func() { _ = os.RemoveAll(stgResult.tempDir) }()
			if err := indexUncommitted(ctx, indexDir, stgResult.tempDir, parallelism); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: uncommitted indexing failed: %v\n", err)
			}
		}
	}

	// §5.7: Post-indexing verification — single atomic call eliminates TOCTOU
	postState := computeStateHash(gitRepoState(ctx).RawOutput)

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
	err = lockFileExclusive(f)
	if err == nil {
		return f, true, nil
	}

	// Lock held by another process
	if shardsExist(indexDir) {
		// Stale index is acceptable
		_ = f.Close()
		return nil, false, nil
	}

	// No shards exist (first run) — poll with exponential backoff.
	// We avoid a blocking flock goroutine to prevent fd-reuse races
	// when the timeout fires and closes the file.
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 60*time.Second)
	defer timeoutCancel()

	backoff := 100 * time.Millisecond
	const maxBackoff = 2 * time.Second

	for {
		select {
		case <-timeoutCtx.Done():
			_ = f.Close()
			return nil, false, fmt.Errorf("indexer lock held >60s")
		default:
		}

		err = lockFileExclusive(f)
		if err == nil {
			return f, true, nil
		}

		timer := time.NewTimer(backoff)
		select {
		case <-timeoutCtx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, false, fmt.Errorf("indexer lock held >60s")
		case <-timer.C:
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// releaseLock releases the flock and closes the file.
func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	unlockFile(f)
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

// prepareUncommittedFiles stages uncommitted files into a temp directory
// using clonefile (CoW), hardlink, or copy as fallback.
// The temp dir is always recreated to avoid stale files from previous crashed runs.
// Uses a worker pool for bounded memory and early cancellation on error.
func prepareUncommittedFiles(repoDir, indexDir string, files []string, parallelism int) (string, error) {
	tmpDir := filepath.Join(indexDir, repoUncommitted)

	// Remove stale temp dir from previous crashed runs to avoid phantom files
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan string, parallelism)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	// setErr records the first error and cancels remaining work.
	setErr := func(e error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel()
		}
		mu.Unlock()
	}

	// Start worker pool
	for range parallelism {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				// Check for cancellation
				if ctx.Err() != nil {
					return
				}

				srcPath := filepath.Join(repoDir, f)
				dstPath := filepath.Join(tmpDir, f)

				// Use Lstat to avoid following symlinks
				fi, err := os.Lstat(srcPath)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					setErr(err)
					return
				}

				// Skip directories (e.g. dirty submodules) and symlinks
				if fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
					continue
				}

				// Create parent directories
				if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
					setErr(err)
					return
				}

				// Try clonefile (CoW), then hardlink, then copy
				if !tryCloneFile(srcPath, dstPath) {
					if err := os.Link(srcPath, dstPath); err != nil {
						if err := copyFile(srcPath, dstPath); err != nil {
							setErr(err)
							return
						}
					}
				}
			}
		}()
	}

	// Send files to workers
	for _, f := range files {
		select {
		case ch <- f:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(ch)
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

// maxUncommittedFileSize is the maximum file size (in bytes) for uncommitted file indexing.
// Files larger than this are skipped to prevent excessive memory usage.
const maxUncommittedFileSize = 10 * 1024 * 1024 // 10 MB

// indexUncommitted indexes uncommitted files using index.NewBuilder.
func indexUncommitted(ctx context.Context, indexDir, tempDir string, parallelism int) error {
	opts := index.Options{
		IndexDir:         indexDir,
		Parallelism:      parallelism,
		CTagsMustSucceed: true,
	}
	opts.RepositoryDescription.Name = repoUncommitted
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

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxUncommittedFileSize {
			fmt.Fprintf(os.Stderr, "Warning: skipping large uncommitted file %s (%d MB)\n", relPath, info.Size()/(1024*1024))
			return nil
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
	matches, err := filepath.Glob(filepath.Join(indexDir, repoUncommitted+"_v*.zoekt"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
