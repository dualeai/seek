package main

import (
	"context"
	"fmt"
	"log/slog"
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
	repoUncommitted = "uncommitted"
	// stateVersion is the prefix used in state hashing to invalidate caches
	// when the hash algorithm or input format changes.
	stateVersion = "v4\x00"
	// maxUncommittedFileSize is the maximum file size (in bytes) for uncommitted
	// file indexing. Files larger than this are skipped to prevent excessive
	// memory usage.
	maxUncommittedFileSize = 10 * 1024 * 1024 // 10 MB
)

// computeStateHash computes the xxHash64 of the given state string.
// In production, the input is a repoStateFingerprint (raw git status output
// enriched with file stats). The stateVersion prefix invalidates old caches
// when the hash algorithm or input format changes.
func computeStateHash(rawOutput string) string {
	h := xxhash.New()
	_, _ = h.Write([]byte(stateVersion))
	_, _ = h.Write([]byte(rawOutput))
	return fmt.Sprintf("%016x", h.Sum64())
}

// repoStateFingerprint returns the raw git status output enriched with working
// tree file stats (mtime + size) for dirty files. git status --porcelain=v2
// doesn't include working tree content hashes, so consecutive edits to an
// already-modified file produce identical porcelain output. Appending file
// stats ensures the state hash changes whenever a dirty file is modified.
func repoStateFingerprint(repoDir string, state repoState) string {
	if len(state.Files) == 0 {
		return state.RawOutput
	}
	var b strings.Builder
	b.WriteString(state.RawOutput)
	for _, f := range state.Files {
		fi, err := os.Lstat(filepath.Join(repoDir, f))
		if err != nil {
			// File may have been deleted between git status and stat;
			// include a sentinel so deletions also change the hash.
			fmt.Fprintf(&b, "\x00%s\x00deleted\x00", f)
			continue
		}
		fmt.Fprintf(&b, "\x00%s\x00%d\x00%d\x00", f, fi.ModTime().UnixNano(), fi.Size())
	}
	return b.String()
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
// must detect this explicitly.
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
	// Fail fast if ctags is missing
	if err := checkCtags(); err != nil {
		return err
	}

	// Ensure cache dir is excluded from git status for post-verification.
	ensureGitExclude(repoDir, cacheDir)

	lockPath := filepath.Join(indexDir, lockFile)

	// Ensure partial state file is cleaned up on all exit paths
	defer func() { _ = os.Remove(filepath.Join(indexDir, stateTmpFile)) }()

	lockFd, acquired, err := acquireLock(ctx, indexDir, lockPath)
	if err != nil {
		return err
	}
	if !acquired {
		// Lock not acquired but shards exist — use stale index
		slog.Warn("Another process is indexing, using existing index")
		return nil
	}
	defer releaseLock(lockFd)

	// Double-check state after acquiring lock
	cachedState := readStateFile(indexDir)
	if cachedState == preState {
		return nil
	}

	parallelism := indexParallelism()

	// Read uncommitted files directly into memory, in parallel with
	// committed indexing. This avoids staging files to a temp directory
	// (and the associated hardlink mutation risks and double I/O on
	// non-CoW filesystems).
	type readResult struct {
		docs []fileContent
	}
	readCh := make(chan readResult, 1)

	if len(state.Files) > 0 {
		go func() {
			docs := readUncommittedFiles(repoDir, state.Files, parallelism)
			readCh <- readResult{docs}
		}()
	} else {
		readCh <- readResult{nil}
	}

	// Index committed files
	committedErr := indexCommitted(ctx, repoDir, indexDir, parallelism)
	if committedErr != nil {
		slog.Warn("Committed indexing failed", "error", committedErr)
	}

	// Wait for uncommitted file reads
	readRes := <-readCh

	// Clean up stale staging directory from previous seek versions
	_ = os.RemoveAll(filepath.Join(indexDir, repoUncommitted))

	// Index uncommitted files. When docs exist, zoekt's builder.Finish()
	// atomically replaces old shards (write to .tmp then os.Rename), so we
	// must NOT delete old shards before writing new ones — that creates a
	// gap where concurrent searchers see no uncommitted shard at all.
	// When no docs exist, we clean up stale uncommitted shards from a
	// previous run where uncommitted files did exist.
	var uncommittedErr error
	if len(readRes.docs) > 0 {
		uncommittedErr = indexUncommitted(ctx, repoDir, indexDir, readRes.docs, parallelism)
		if uncommittedErr != nil {
			slog.Warn("Uncommitted indexing failed", "error", uncommittedErr)
		}
	} else {
		cleanUncommittedShards(indexDir)
	}

	// Post-indexing verification — single atomic call eliminates TOCTOU.
	// Use repoDir explicitly so this works regardless of process CWD.
	postRepoState := gitRepoStateIn(ctx, repoDir)
	postState := computeStateHash(repoStateFingerprint(repoDir, postRepoState))

	if committedErr != nil || uncommittedErr != nil {
		// Don't cache state when either indexing step failed — forces
		// re-index on next search so transient failures don't leave
		// uncommitted content permanently invisible.
		deleteStateFiles(indexDir)
		slog.Warn("Index incomplete, will re-index on next search")
		return nil
	}

	if postState == preState {
		if err := writeStateFile(indexDir, preState); err != nil {
			return fmt.Errorf("write state file: %w", err)
		}
	} else {
		deleteStateFiles(indexDir)
		slog.Warn("Index may be stale, will re-index on next search")
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

// fileContent holds a file's path and content read from the working tree.
type fileContent struct {
	name    string
	content []byte
}

// readUncommittedFiles reads uncommitted files directly from the working tree
// into memory using a bounded worker pool. Files larger than maxUncommittedFileSize,
// symlinks, and directories are skipped. Individual file failures are non-fatal
// since files may be deleted or modified between git status and read.
func readUncommittedFiles(repoDir string, files []string, parallelism int) []fileContent {
	ch := make(chan string, parallelism)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []fileContent

	for range parallelism {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				srcPath := filepath.Join(repoDir, f)

				// Use Lstat to avoid following symlinks
				fi, err := os.Lstat(srcPath)
				if err != nil {
					continue
				}

				// Skip directories (e.g. dirty submodules) and symlinks
				if fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
					continue
				}

				if fi.Size() > maxUncommittedFileSize {
					slog.Warn("Skipping large uncommitted file", "path", f, "size_mb", fi.Size()/(1024*1024))
					continue
				}

				content, err := os.ReadFile(srcPath)
				if err != nil {
					continue
				}

				mu.Lock()
				results = append(results, fileContent{name: f, content: content})
				mu.Unlock()
			}
		}()
	}

	for _, f := range files {
		ch <- f
	}
	close(ch)
	wg.Wait()

	return results
}

// indexUncommitted indexes pre-read uncommitted file contents using index.NewBuilder.
func indexUncommitted(ctx context.Context, repoDir, indexDir string, docs []fileContent, parallelism int) error {
	opts := index.Options{
		IndexDir:         indexDir,
		Parallelism:      parallelism,
		CTagsMustSucceed: true,
	}
	opts.RepositoryDescription.Name = repoUncommitted
	opts.RepositoryDescription.Source = repoDir
	opts.SetDefaults()

	builder, err := index.NewBuilder(opts)
	if err != nil {
		return fmt.Errorf("create builder: %w", err)
	}

	for _, doc := range docs {
		if err := builder.Add(index.Document{
			Name:    doc.name,
			Content: doc.content,
		}); err != nil {
			return fmt.Errorf("add document %s: %w", doc.name, err)
		}
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
