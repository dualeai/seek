package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

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
	// headFile stores the HEAD SHA of the last successful committed index.
	// Used to skip incremental committed indexing when HEAD hasn't changed,
	// avoiding ~560µs of git repo opening + shard metadata checks and
	// eliminating CPU contention when running alongside uncommitted indexing.
	headFile = ".head"
	// lockFile is used for mutual exclusion during indexing.
	lockFile = ".lock"
	// repoUncommitted is the zoekt repository name for uncommitted file shards.
	repoUncommitted = "uncommitted"
	// stateVersion is the prefix used in state hashing to invalidate caches
	// when the hash algorithm or input format changes.
	stateVersion = "v5\x00"
	// maxUncommittedFileSize is the maximum file size (in bytes) for uncommitted
	// file indexing. Files larger than this are skipped to prevent excessive
	// memory usage.
	maxUncommittedFileSize = 10 * 1024 * 1024 // 10 MB
	// shardMax is the maximum corpus size (in bytes) per zoekt shard.
	// Smaller shards allow more parallel shard building during cold index.
	// Default zoekt value is 100MB (3 shards for k8s, ~1.7 cores used).
	// 10MB produces ~23 shards for k8s, utilizing ~5 cores → 2.7x faster.
	// No measurable impact on warm search latency.
	shardMax = 10 * 1024 * 1024 // 10 MB
)

// computeStateHash computes the xxHash64 of the given state string.
// In production, the input is a repoStateFingerprint (raw git status output
// enriched with file stats). The stateVersion prefix invalidates old caches
// when the hash algorithm or input format changes.
func computeStateHash(rawOutput string) string {
	h := xxhash.New()
	_, _ = h.WriteString(stateVersion)
	_, _ = h.WriteString(rawOutput)
	return formatHex16(h.Sum64())
}

// formatHex16 formats a uint64 as a zero-padded 16-character hex string
// without the fmt.Sprintf allocation.
func formatHex16(v uint64) string {
	const digits = "0123456789abcdef"
	var buf [16]byte
	for i := 15; i >= 0; i-- {
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

// repoStateFingerprint returns the raw git status output enriched with working
// tree file stats (mtime, size, and inode) for dirty files. git status
// --porcelain=v2 doesn't include working tree content hashes, so consecutive
// edits to an already-modified file produce identical porcelain output.
// Appending file stats ensures the state hash changes whenever a dirty file is
// modified. The inode detects atomic-write editors (vim, emacs) that replace
// files via write-to-tmp + rename, which changes the inode but may preserve
// mtime.
//
// Called twice per indexing cycle: once before indexing (to compute the
// pre-state hash) and once after (to detect drift). The second call
// re-Lstats the same files, so any modification during indexing produces
// a different hash.
func repoStateFingerprint(repoDir string, state repoState) string {
	if len(state.Files) == 0 {
		return state.RawOutput
	}
	var b strings.Builder
	b.Grow(len(state.RawOutput) + len(state.Files)*80)
	b.WriteString(state.RawOutput)

	// Pre-build path prefix to avoid per-file filepath.Join allocation.
	// Git status paths are clean relative paths (no double slashes or dots),
	// so simple concatenation is safe.
	pathPrefix := repoDir + "/"

	// Scratch buffer for numeric formatting (avoids strconv.Format* allocs).
	var numBuf [20]byte

	for _, f := range state.Files {
		var stat syscall.Stat_t
		if err := syscall.Lstat(pathPrefix+f, &stat); err != nil {
			// File may have been deleted between git status and stat;
			// include a sentinel so deletions also change the hash.
			b.WriteByte(0)
			b.WriteString(f)
			b.WriteString("\x00deleted\x00")
			continue
		}
		mtime := statMtimeNano(stat)
		b.WriteByte(0)
		b.WriteString(f)
		b.WriteByte(0)
		b.Write(strconv.AppendInt(numBuf[:0], mtime, 10))
		b.WriteByte(0)
		b.Write(strconv.AppendInt(numBuf[:0], stat.Size, 10))
		b.WriteByte(0)
		b.Write(strconv.AppendUint(numBuf[:0], stat.Ino, 10))
		b.WriteByte(0)
	}
	return b.String()
}

// readCacheFile reads a single-line cached value from indexDir/name.
func readCacheFile(indexDir, name string) string {
	data, err := os.ReadFile(filepath.Join(indexDir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeCacheFile atomically writes value to indexDir/name via tmp+rename.
func writeCacheFile(indexDir, name, value string) error {
	tmpPath := filepath.Join(indexDir, name+".tmp")
	if err := os.WriteFile(tmpPath, []byte(value), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(indexDir, name))
}

// readStateFile reads the cached state hash.
func readStateFile(indexDir string) string { return readCacheFile(indexDir, stateFile) }

// writeStateFile atomically writes the state hash.
func writeStateFile(indexDir, state string) error { return writeCacheFile(indexDir, stateFile, state) }

// readHeadFile reads the last indexed HEAD SHA.
func readHeadFile(indexDir string) string { return readCacheFile(indexDir, headFile) }

// writeHeadFile atomically writes the HEAD SHA.
func writeHeadFile(indexDir, sha string) error { return writeCacheFile(indexDir, headFile, sha) }

// deleteStateFiles removes .state, .state.tmp, .head, and .head.tmp.
// Clearing .head alongside .state ensures that a failed or drifted
// indexing cycle forces a full re-index (including committed) on the
// next invocation, rather than relying on a potentially stale .head
// to skip committed indexing.
func deleteStateFiles(indexDir string) {
	_ = os.Remove(filepath.Join(indexDir, stateFile))
	_ = os.Remove(filepath.Join(indexDir, stateFile+".tmp"))
	_ = os.Remove(filepath.Join(indexDir, headFile))
	_ = os.Remove(filepath.Join(indexDir, headFile+".tmp"))
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

// ctagsOnce caches the result of checkCtags so the PATH lookup and
// --version subprocess run at most once per process. The result is
// deterministic within a single invocation (ctags won't be uninstalled
// between search cycles).
var (
	ctagsOnce sync.Once
	ctagsErr  error
)

// checkCtagsCached returns the cached result of checkCtags, running the
// check at most once per process.
func checkCtagsCached() error {
	ctagsOnce.Do(func() { ctagsErr = checkCtags() })
	return ctagsErr
}

// checkCtags verifies that universal-ctags is installed. Zoekt silently skips
// symbol parsing when ctags is missing (even with CTagsMustSucceed), so we
// must detect this explicitly.
//
// Detection order:
//  1. CTAGS_COMMAND env var (explicit user override)
//  2. "universal-ctags" binary on PATH (zoekt default)
//  3. "ctags" binary on PATH, verified via --version (Homebrew on macOS
//     installs universal-ctags as "ctags")
func checkCtags() error {
	// 1. Explicit env var — trust the user.
	if cmd := os.Getenv("CTAGS_COMMAND"); cmd != "" {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("CTAGS_COMMAND=%q not found on PATH: %w", cmd, err)
		}
		return nil
	}

	// 2. Zoekt default: looks for "universal-ctags" on PATH.
	var opts index.Options
	opts.SetDefaults()
	if opts.CTagsPath != "" {
		return nil
	}

	// 3. Fallback: Homebrew installs universal-ctags as "ctags".
	// Verify via --version to distinguish from Exuberant Ctags.
	if ctags, err := exec.LookPath("ctags"); err == nil {
		out, err := exec.Command(ctags, "--version").Output()
		if err == nil && strings.Contains(string(out), "Universal Ctags") {
			_ = os.Setenv("CTAGS_COMMAND", ctags)
			return nil
		}
	}

	return fmt.Errorf("universal-ctags required but not found.\n  macOS:  brew install universal-ctags\n  Linux:  sudo apt-get install universal-ctags\n  Or set CTAGS_COMMAND=/path/to/ctags")
}

// runIndexing orchestrates committed and uncommitted indexing with locking.
func runIndexing(ctx context.Context, paths gitPaths, indexDir string, state repoState, preState string) error {
	repoDir := paths.RepoDir
	// Fail fast if ctags is missing. Uses sync.Once cache so the PATH
	// lookup + --version subprocess runs at most once per process.
	if err := checkCtagsCached(); err != nil {
		return err
	}

	// ensureGitExclude is handled by run() on the first invocation
	// (before gitRepoState) and persists in .git/info/exclude — no
	// need to re-check on every dirty reindex cycle.

	lockPath := filepath.Join(indexDir, lockFile)

	// Ensure partial temp files are cleaned up on all exit paths
	defer func() {
		_ = os.Remove(filepath.Join(indexDir, stateTmpFile))
		_ = os.Remove(filepath.Join(indexDir, headFile+".tmp"))
	}()

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

	// Clean up stale staging directory from previous seek versions.
	// Must run before either indexer starts to avoid racing with shard writes.
	_ = os.RemoveAll(filepath.Join(indexDir, repoUncommitted))

	// Stream uncommitted files through a channel. The bounded channel
	// (size=parallelism) provides backpressure so at most 2*parallelism
	// files are in flight (channel buffer + blocked workers).
	var fileCh <-chan fileContent
	if len(state.Files) > 0 {
		fileCh = streamFiles(repoDir, state.Files, parallelism)
		// Ensure the producer goroutine is drained on all exit paths
		// (including panics) to prevent goroutine leaks.
		defer func() {
			for range fileCh {
			}
		}()
	}

	// Skip committed indexing when HEAD hasn't moved since the last
	// successful index. This avoids ~560µs of git repo opening + shard
	// metadata reads on the incremental no-op path, and eliminates CPU
	// contention when running alongside the uncommitted indexer.
	needCommitted := state.HeadSHA != readHeadFile(indexDir)

	// Run committed and uncommitted indexing. They write different shard
	// files (repo name vs "uncommitted" prefix) so when both are needed
	// they run in parallel. When only one is needed, it runs alone.
	var committedErr, uncommittedErr error
	if fileCh != nil && needCommitted {
		// Both needed — run committed in a goroutine, uncommitted in
		// the current goroutine (it must drain fileCh).
		committedDone := make(chan error, 1)
		go func() {
			committedDone <- indexCommitted(ctx, repoDir, indexDir, parallelism)
		}()
		uncommittedErr = indexUncommitted(ctx, repoDir, indexDir, fileCh, parallelism)
		committedErr = <-committedDone
	} else if fileCh != nil {
		// Only uncommitted files changed — HEAD is the same.
		uncommittedErr = indexUncommitted(ctx, repoDir, indexDir, fileCh, parallelism)
	} else if needCommitted {
		committedErr = indexCommitted(ctx, repoDir, indexDir, parallelism)
		cleanUncommittedShards(indexDir)
	} else {
		// HEAD unchanged, no dirty files — this shouldn't normally
		// reach here (state hash would match), but handle defensively.
		cleanUncommittedShards(indexDir)
	}

	if committedErr != nil {
		slog.Warn("Committed indexing failed", "error", committedErr)
	}
	if uncommittedErr != nil {
		slog.Warn("Uncommitted indexing failed", "error", uncommittedErr)
	}

	// Post-indexing verification — re-stat the known dirty files to detect
	// changes made during the indexing window. This replaces a full
	// gitRepoStateIn call (~250-450ms on large repos) with cheap Lstat
	// calls (~0.004ms) on only the files we just indexed.
	//
	// What this catches: any dirty file modified, deleted, or atomically
	// replaced during indexing (mtime, size, or inode change).
	//
	// What this defers to the next search: new untracked files appearing
	// or HEAD changes during the indexing window. Both are caught by the
	// next invocation's gitRepoState() call in run(), which always runs
	// a full git status.
	postState := computeStateHash(repoStateFingerprint(repoDir, state))

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
		// Persist the HEAD SHA so subsequent runs with only working tree
		// changes can skip the committed indexer entirely.
		if err := writeHeadFile(indexDir, state.HeadSHA); err != nil {
			slog.Warn("Failed to write head file", "error", err)
		}
	} else {
		deleteStateFiles(indexDir)
		slog.Warn("Index may be stale, will re-index on next search")
	}

	return nil
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
			ShardMax:         shardMax,
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

// readFilesToChannel reads files from the working tree using a bounded worker
// pool and sends them to out. Files larger than maxUncommittedFileSize,
// symlinks, and directories are skipped. Individual file failures are
// non-fatal since files may be deleted or modified between git status and
// read. The channel is closed after all workers finish.
func readFilesToChannel(repoDir string, files []string, parallelism int, out chan<- fileContent) {
	ch := make(chan string, parallelism)
	var wg sync.WaitGroup

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

				// Only process regular files — skip directories (dirty
				// submodules), symlinks, FIFOs, sockets, and devices to
				// avoid blocking or reading unexpected data.
				if !fi.Mode().IsRegular() {
					continue
				}

				size := fi.Size()
				if size > maxUncommittedFileSize {
					slog.Warn("Skipping large uncommitted file", "path", f, "size_mb", size/(1024*1024))
					continue
				}

				// Read using the known size from Lstat to avoid the
				// extra Fstat that os.ReadFile performs internally.
				fh, err := os.Open(srcPath)
				if err != nil {
					continue
				}
				buf := make([]byte, size)
				n, err := fh.Read(buf)
				_ = fh.Close()
				if err != nil && n == 0 {
					continue
				}

				out <- fileContent{name: f, content: buf[:n]}
			}
		}()
	}

	for _, f := range files {
		ch <- f
	}
	close(ch)
	wg.Wait()
	close(out)
}

// streamFiles returns a channel that yields file contents read from the
// working tree. The channel is bounded by parallelism to provide backpressure,
// so at most 2*parallelism files are in flight (buffer + blocked workers)
// rather than all dirty files at once.
func streamFiles(repoDir string, files []string, parallelism int) <-chan fileContent {
	out := make(chan fileContent, parallelism)
	go readFilesToChannel(repoDir, files, parallelism, out)
	return out
}

// indexUncommitted indexes uncommitted file contents streamed through fileCh
// using index.NewBuilder. Old uncommitted shards are not deleted before
// writing — zoekt's builder.Finish() atomically replaces them (write to
// .tmp then os.Rename), avoiding a gap where concurrent searchers see no
// uncommitted shard. The builder is created lazily on the first file to
// avoid spawning ctags processes when the channel is empty. On NewBuilder
// error the channel is explicitly drained; on Add error the loop continues
// consuming remaining items. Both paths prevent goroutine leaks in the
// producer. Finish is always called when a builder exists (even after Add
// errors) to ensure cleanup.
func indexUncommitted(ctx context.Context, repoDir, indexDir string, fileCh <-chan fileContent, parallelism int) error {
	var builder *index.Builder
	var addErr error

	for doc := range fileCh {
		if builder == nil {
			opts := index.Options{
				IndexDir:         indexDir,
				Parallelism:      parallelism,
				CTagsMustSucceed: true,
				ShardMax:         shardMax,
			}
			opts.RepositoryDescription.Name = repoUncommitted
			opts.RepositoryDescription.Source = repoDir
			opts.SetDefaults()

			var err error
			builder, err = index.NewBuilder(opts)
			if err != nil {
				// Drain remaining items to unblock producer goroutines.
				for range fileCh {
				}
				return fmt.Errorf("create builder: %w", err)
			}
		}

		if addErr == nil {
			if err := builder.Add(index.Document{
				Name:    doc.name,
				Content: doc.content,
			}); err != nil {
				addErr = fmt.Errorf("add document %s: %w", doc.name, err)
				// Continue draining the channel to unblock producer goroutines.
			}
		}
	}

	if builder == nil {
		// No files arrived — clean stale shards from a previous run.
		cleanUncommittedShards(indexDir)
		return nil
	}

	// Always call Finish to ensure cleanup (safe to call even after errors).
	finishErr := builder.Finish()
	if addErr != nil {
		return addErr
	}
	return finishErr
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
