package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// stateFile stores the hash of the last indexed corpus state.
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
	// emptyGitTreeSHA is Git's canonical SHA-1 for the empty tree.
	emptyGitTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	// stateVersion is the prefix used in state hashing to invalidate previous
	// state formats when the hash algorithm or input format changes.
	stateVersion = "v5\x00"
	// shardMax is the maximum corpus size (in bytes) per zoekt shard.
	// Smaller shards allow more parallel shard building during cold index.
	// Default zoekt value is 100MB (3 shards for k8s, ~1.7 cores used).
	// 10MB produces ~23 shards for k8s, utilizing ~5 cores → 2.7x faster.
	// No measurable impact on warm search latency.
	shardMax = 10 * 1024 * 1024 // 10 MB
)

// computeStateHash computes the xxHash64 of the given state string.
// In production, the input is a repoStateFingerprint (raw git status output
// enriched with file stats).
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

// readCacheFile reads a single-line cached value from cacheDir/name.
func readCacheFile(cacheDir, name string) string {
	data, err := os.ReadFile(filepath.Join(cacheDir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeCacheFile atomically writes value to cacheDir/name via tmp+rename.
func writeCacheFile(cacheDir, name, value string) error {
	tmpPath := filepath.Join(cacheDir, name+".tmp")
	if err := os.WriteFile(tmpPath, []byte(value), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(cacheDir, name))
}

// readStateFile reads the cached state hash.
func readStateFile(cacheDir string) string { return readCacheFile(cacheDir, stateFile) }

// writeStateFile atomically writes the state hash.
func writeStateFile(cacheDir, state string) error { return writeCacheFile(cacheDir, stateFile, state) }

// readHeadFile reads the last indexed HEAD SHA.
func readHeadFile(cacheDir string) string { return readCacheFile(cacheDir, headFile) }

// writeHeadFile atomically writes the HEAD SHA.
func writeHeadFile(cacheDir, sha string) error { return writeCacheFile(cacheDir, headFile, sha) }

// deleteStateFiles removes .state, .state.tmp, .head, and .head.tmp.
// Clearing .head alongside .state ensures that a failed or drifted
// indexing cycle forces a full re-index (including committed) on the
// next invocation, rather than relying on a potentially stale .head
// to skip committed indexing.
func deleteStateFiles(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, stateFile))
	_ = os.Remove(filepath.Join(cacheDir, stateFile+".tmp"))
	_ = os.Remove(filepath.Join(cacheDir, headFile))
	_ = os.Remove(filepath.Join(cacheDir, headFile+".tmp"))
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

func indexBuildOptions(indexDir string, parallelism int) index.Options {
	return index.Options{
		IndexDir:         indexDir,
		SizeMax:          maxIndexedDocumentBytes,
		Parallelism:      parallelism,
		CTagsMustSucceed: true,
		ShardMax:         shardMax,
	}
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

// runIndexingWithCache orchestrates committed and uncommitted indexing with
// corpus metadata in cacheDir and Zoekt shards in indexDir.
func runIndexingWithCache(ctx context.Context, paths gitPaths, cacheDir, indexDir string, state repoState, preState string) error {
	repoDir := paths.RepoDir
	// Fail fast if ctags is missing. Uses sync.Once cache so the PATH
	// lookup + --version subprocess runs at most once per process.
	if err := checkCtagsCached(); err != nil {
		deleteStateFiles(cacheDir)
		return gitCorpusError(repoDir, indexDir, err)
	}

	lockPath := filepath.Join(cacheDir, lockFile)

	// Ensure partial temp files are cleaned up on all exit paths
	defer func() {
		_ = os.Remove(filepath.Join(cacheDir, stateTmpFile))
		_ = os.Remove(filepath.Join(cacheDir, headFile+".tmp"))
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
	cachedState := readStateFile(cacheDir)
	if cachedState == preState {
		return nil
	}

	parallelism := indexParallelism()

	if err := checkGitDirtyFileBudget(repoDir, indexDir, state.Files); err != nil {
		deleteStateFiles(cacheDir)
		return err
	}

	// Stream uncommitted files through a bounded channel so file reads stay
	// proportional to the active worker count.
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
	needCommitted := state.HeadSHA != readHeadFile(cacheDir)
	if needCommitted {
		if _, err := scanGitCommittedIndexBudget(ctx, repoDir, maxGitCandidateFiles, maxCorpusIndexedBytes); err != nil {
			deleteStateFiles(cacheDir)
			return gitCorpusError(repoDir, indexDir, err)
		}
	}

	// Run committed and uncommitted indexing. They write different shard
	// files (repo name vs "uncommitted" prefix) so when both are needed
	// they run in parallel. When only one is needed, it runs alone.
	var committedErr, uncommittedErr error
	if fileCh != nil && needCommitted {
		// Both needed — run committed in a goroutine, uncommitted in
		// the current goroutine (it must drain fileCh).
		committedDone := make(chan error, 1)
		go func() {
			committedDone <- indexCommitted(repoDir, indexDir, parallelism)
		}()
		uncommittedErr = indexUncommitted(ctx, repoDir, indexDir, fileCh, parallelism)
		committedErr = <-committedDone
	} else if fileCh != nil {
		// Only uncommitted files changed — HEAD is the same.
		uncommittedErr = indexUncommitted(ctx, repoDir, indexDir, fileCh, parallelism)
	} else if needCommitted {
		committedErr = indexCommitted(repoDir, indexDir, parallelism)
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
	// next invocation's git status call in run(), which always runs
	// a full git status.
	postState := gitCorpusStateHash(paths, state)

	if committedErr != nil || uncommittedErr != nil {
		// Don't cache state when either indexing step failed — forces
		// re-index on next search so transient failures don't leave
		// uncommitted content permanently invisible.
		deleteStateFiles(cacheDir)
		if errors.Is(committedErr, errGitCapExceeded) {
			return committedErr
		}
		if errors.Is(uncommittedErr, errGitCapExceeded) {
			return uncommittedErr
		}
		slog.Warn("Index incomplete, will re-index on next search")
		return nil
	}

	if postState == preState {
		if err := writeStateFile(cacheDir, preState); err != nil {
			return fmt.Errorf("write state file: %w", err)
		}
		// Persist the HEAD SHA so subsequent runs with only working tree
		// changes can skip the committed indexer entirely.
		if err := writeHeadFile(cacheDir, state.HeadSHA); err != nil {
			slog.Warn("Failed to write head file", "error", err)
		}
	} else {
		deleteStateFiles(cacheDir)
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
func indexCommitted(repoDir, indexDir string, parallelism int) error {
	opts := gitindex.Options{
		RepoDir:      repoDir,
		Incremental:  true,
		Branches:     []string{"HEAD"},
		BuildOptions: indexBuildOptions(indexDir, parallelism),
	}
	_, err := gitindex.IndexGitRepo(opts)
	if err != nil {
		return gitCorpusError(repoDir, indexDir, err)
	}
	return nil
}

// fileContent holds a file's path and content read from the working tree.
type fileContent struct {
	name    string
	content []byte
}

// readFilesToChannel reads files from the working tree using a bounded worker
// pool and sends them to out. Files larger than maxGitDirtyFileSize,
// symlinks, and directories are skipped. Individual file failures are
// non-fatal since files may be deleted or modified between git status and
// read. The channel is closed after all workers finish.
func readFilesToChannel(repoDir string, files []string, parallelism int, out chan<- fileContent) {
	workers := fileReadWorkerCount(parallelism, len(files))
	ch := make(chan string, workers)
	var wg sync.WaitGroup

	for range workers {
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
				if size > maxGitDirtyFileSize {
					slog.Warn("Skipping large uncommitted file", "path", f, "size_mb", size/(1024*1024))
					continue
				}

				// Read using the known size from Lstat to avoid the extra
				// Fstat that os.ReadFile performs internally. A single
				// Read is not guaranteed to fill the buffer, even for
				// regular files, so use ReadFull and keep partial content
				// only when the file shrank during the read.
				fh, err := os.Open(srcPath)
				if err != nil {
					continue
				}
				buf := make([]byte, size)
				n, err := io.ReadFull(fh, buf)
				_ = fh.Close()
				if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
					continue
				}
				if n == 0 && size > 0 {
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
// working tree. The output channel is unbuffered because Zoekt's public
// Builder API accepts []byte documents, not io.Reader streams. This keeps
// seek-side buffering to at most the active reader workers instead of queueing
// already-read file contents.
func streamFiles(repoDir string, files []string, parallelism int) <-chan fileContent {
	out := make(chan fileContent)
	go readFilesToChannel(repoDir, files, parallelism, out)
	return out
}

func fileReadWorkerCount(parallelism, items int) int {
	if items <= 0 {
		return 0
	}
	if parallelism < 1 {
		return 1
	}
	if parallelism > items {
		return items
	}
	return parallelism
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
	_, err := indexDocuments(ctx, indexDir, repoUncommitted, repoDir, fileCh, parallelism)
	return err
}

func indexDocuments(
	ctx context.Context,
	indexDir string,
	repoName string,
	source string,
	fileCh <-chan fileContent,
	parallelism int,
) (bool, error) {
	var builder *index.Builder
	var addErr error

	for doc := range fileCh {
		if builder == nil {
			opts := indexBuildOptions(indexDir, parallelism)
			opts.RepositoryDescription.Name = repoName
			opts.RepositoryDescription.Source = source

			var err error
			builder, err = index.NewBuilder(opts)
			if err != nil {
				// Drain remaining items to unblock producer goroutines.
				for range fileCh {
				}
				return false, fmt.Errorf("create builder: %w", err)
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
		cleanRepositoryShards(indexDir, repoName)
		return false, nil
	}

	// Always call Finish to ensure cleanup (safe to call even after errors).
	finishErr := builder.Finish()
	if addErr != nil {
		return true, addErr
	}
	return true, finishErr
}

// cleanUncommittedShards removes stale uncommitted shard files.
func cleanUncommittedShards(indexDir string) {
	cleanRepositoryShards(indexDir, repoUncommitted)
}

func cleanRepositoryShards(indexDir, repoName string) {
	for _, m := range repositoryShardFiles(indexDir, repoName) {
		_ = os.Remove(m)
	}
}

func repositoryShardCount(indexDir, repoName string) int {
	return len(repositoryShardFiles(indexDir, repoName))
}

func repositoryShardFiles(indexDir, repoName string) []string {
	matches, err := filepath.Glob(filepath.Join(indexDir, repoName+"_v*.zoekt"))
	if err != nil {
		return nil
	}
	return matches
}

func cleanAllShards(indexDir string) {
	matches, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
