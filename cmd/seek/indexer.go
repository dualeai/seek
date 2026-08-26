package main

import (
	"bufio"
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
	// emptyFile stores the state hash for a layer that is known to have
	// no indexable shards. This prevents a missing/corrupt shard directory
	// from being confused with an intentionally empty scoped layer.
	emptyFile = ".empty"
	// lockFile is the publish/read lock: readers take it LOCK_SH across their
	// whole glob+open+search; a temp-swap publish takes it LOCK_EX briefly.
	lockFile = ".lock"
	// buildLockFile is the single-flight build lock: a builder holds it LOCK_EX
	// across the entire (possibly multi-minute) build. Readers NEVER take it, so
	// a long build does not block searches — only the brief publish swap does.
	buildLockFile = ".build.lock"
	// swappingMarkerFile records, while present, that a temp-swap publish was
	// interrupted mid-rename; the named shard family may be torn and must be
	// cleaned + fully rebuilt on the next build (see recoverIncompleteSwap).
	swappingMarkerFile = ".swapping"
	// buildDirPrefix names per-build temp index dirs (cacheDir/index/.build-*).
	// Dot-prefixed so gc enumeration / shardsExist / size walks ignore them.
	// Reaped by sweepOrphanBuildDirs after buildTmpMaxAge (gc.go).
	buildDirPrefix = ".build-"
	// repoUncommitted is the zoekt repository name for uncommitted file shards.
	repoUncommitted = "uncommitted"
	// emptyGitTreeSHA is Git's canonical SHA-1 for the empty tree.
	emptyGitTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	// stateVersion is the prefix used in state hashing to invalidate previous
	// state formats when the hash algorithm or input format changes.
	stateVersion = "v6\x00"
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

// repoStateFingerprint returns raw Git status output plus the path, mtime,
// size, and inode of each dirty file. Git status porcelain does not include
// working-tree content hashes, so these fields detect common changes to files
// that are already dirty. This is a metadata fingerprint, not a content hash;
// an edit that preserves every observed field can keep the same fingerprint.
//
// Called twice per indexing cycle: once before indexing (to compute the
// pre-state hash) and once after (to detect drift). The second call
// reads the same metadata again so changes to an observed field cause the
// build to be discarded.
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

func readEmptyStateFile(cacheDir string) string { return readCacheFile(cacheDir, emptyFile) }

func writeEmptyStateFile(cacheDir, state string) error {
	return writeCacheFile(cacheDir, emptyFile, state)
}

func deleteEmptyStateFiles(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, emptyFile))
	_ = os.Remove(filepath.Join(cacheDir, emptyFile+".tmp"))
}

// deleteStateFiles removes .state, .head, .empty, and their tmp files.
// Clearing .head alongside .state ensures that a failed or drifted
// indexing cycle forces a full re-index (including committed) on the
// next invocation, rather than relying on a potentially stale .head
// to skip committed indexing.
func deleteStateFiles(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, stateFile))
	_ = os.Remove(filepath.Join(cacheDir, stateFile+".tmp"))
	_ = os.Remove(filepath.Join(cacheDir, headFile))
	_ = os.Remove(filepath.Join(cacheDir, headFile+".tmp"))
	deleteEmptyStateFiles(cacheDir)
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

// ctagsOnce caches executable discovery for one process.
var (
	ctagsOnce sync.Once
	ctagsErr  error
)

// ctagsUnavailableError identifies a failure to locate an executable Ctags
// command. An explicit command records its CTAGS_COMMAND value. The wrapped
// cause keeps lookup details for verbose logs.
type ctagsUnavailableError struct {
	command string
	cause   error
}

func (e *ctagsUnavailableError) Error() string {
	if e.command != "" {
		return fmt.Sprintf("cannot use CTAGS_COMMAND=%q: %v", e.command, e.cause)
	}
	if e.cause != nil {
		return fmt.Sprintf("universal-ctags executable not found: %v", e.cause)
	}
	return "universal-ctags executable not found"
}

func (e *ctagsUnavailableError) Unwrap() error {
	return e.cause
}

// checkCtagsCached returns the cached result of checkCtags, running the
// check at most once per process.
func checkCtagsCached() error {
	ctagsOnce.Do(func() { ctagsErr = checkCtags() })
	return ctagsErr
}

// checkCtags locates an executable command before Zoekt creates a builder. It
// verifies the generic "ctags" fallback by version because that name can refer
// to other implementations. Zoekt validates required Universal Ctags features
// when CTagsMustSucceed is set.
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
			return &ctagsUnavailableError{command: cmd, cause: err}
		}
		return nil
	}

	// 2. Zoekt default: looks for "universal-ctags" on PATH.
	if _, err := exec.LookPath("universal-ctags"); err == nil {
		return nil
	} else {
		universalErr := err

		// 3. Fallback: Homebrew installs universal-ctags as "ctags".
		// Verify via --version to distinguish it from Exuberant Ctags.
		ctags, lookupErr := exec.LookPath("ctags")
		if lookupErr != nil {
			return &ctagsUnavailableError{cause: errors.Join(universalErr, lookupErr)}
		}
		out, versionErr := exec.Command(ctags, "--version").Output()
		if versionErr == nil && strings.Contains(string(out), "Universal Ctags") {
			_ = os.Setenv("CTAGS_COMMAND", ctags)
			return nil
		}
		if versionErr != nil {
			versionErr = fmt.Errorf("check %q --version: %w", ctags, versionErr)
		} else {
			versionErr = fmt.Errorf("%q is not Universal Ctags", ctags)
		}
		return &ctagsUnavailableError{cause: errors.Join(universalErr, versionErr)}
	}
}

// runIndexingWithCache makes the combined committed+uncommitted git index
// current using the single-flight temp-swap protocol:
//
//   - .build.lock (acquireBuildLock) serializes builders and is held across the
//     whole build; readers never take it, so a long build never blocks search.
//   - The committed family is built outside the publish lock in a temporary
//     directory seeded with current shards. HEAD is checked before the brief
//     publish-lock swap.
//   - The uncommitted family is fast and carries a cacheDir manifest, so it is
//     rebuilt in-place under that same brief publish lock (readers excluded for
//     its sub-second build), avoiding a temp/manifest desync.
//
// Readers hold the publish lock LOCK_SH across their whole glob+open, so they
// observe exactly the pre- or post-swap shard set — never torn.
func runIndexingWithCache(ctx context.Context, paths gitPaths, cacheDir, indexDir string, state repoState, preState string) error {
	repoDir := paths.RepoDir
	// Fail fast when no executable Ctags command can be found.
	if err := checkCtagsCached(); err != nil {
		deleteStateFiles(cacheDir)
		return gitCorpusError(repoDir, indexDir, err)
	}

	// Ensure partial temp files are cleaned up on all exit paths.
	defer func() {
		_ = os.Remove(filepath.Join(cacheDir, stateTmpFile))
		_ = os.Remove(filepath.Join(cacheDir, headFile+".tmp"))
	}()

	buildFd, acquired, err := acquireBuildLock(ctx, cacheDir, indexDir)
	if err != nil {
		return err
	}
	if !acquired {
		// Another builder is active and a usable index exists — serve current
		// shards; the active builder publishes the fresh ones.
		slog.Debug("Another process is indexing; serving current index")
		return nil
	}
	defer releaseLock(buildFd)

	// Repair a crash that left a half-published swap before trusting state.
	recoverIncompleteSwap(cacheDir, indexDir)

	// Double-check state after acquiring the build lock (a peer may have just
	// published this generation).
	cachedState := readStateFile(cacheDir)
	if cachedState == preState {
		return nil
	}

	parallelism := indexParallelism()

	if err := checkGitDirtyFileBudget(repoDir, indexDir, state.Files); err != nil {
		deleteStateFiles(cacheDir)
		return err
	}

	hasDirty := len(state.Files) > 0
	// Skip committed indexing when HEAD hasn't moved since the last successful
	// index (cheap incremental no-op path).
	needCommitted := state.HeadSHA != "no-head" && state.HeadSHA != readHeadFile(cacheDir)

	// Build the committed family outside the publish lock in a seeded temp dir.
	var buildDir string
	if needCommitted {
		if _, err := scanGitCommittedIndexBudget(ctx, repoDir, gitCandidateFileLimit, gitCorpusIndexedByteLimit); err != nil {
			deleteStateFiles(cacheDir)
			return gitCorpusError(repoDir, indexDir, err)
		}
		buildDir, err = newBuildDir(indexDir)
		if err != nil {
			return err
		}
		defer discardBuildDir(buildDir)
		if err := seedFamily(indexDir, buildDir, familyCommitted); err != nil {
			return err
		}
		if cErr := indexCommitted(repoDir, buildDir, parallelism); cErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if !shardsExist(indexDir) {
				deleteStateFiles(cacheDir)
			}
			// The caller decides whether existing shards make a stale search safe.
			return cErr
		}
		// HEAD must still equal the captured value before publication. Otherwise,
		// discard the temp build and keep the current published shards. The next
		// search derives fresh state and can rebuild once HEAD is stable.
		publish, err := validateCommittedBuildHead(ctx, paths, state.HeadSHA)
		if err != nil {
			return err
		}
		if !publish {
			return nil // discard build; live shards untouched
		}
	}

	// --- Publish: brief EX covering the committed swap + in-place uncommitted
	// build + the state/head write below, so readers (SH) observe one consistent
	// epoch. INVARIANT: writeStateFile/writeHeadFile (below) MUST stay inside
	// this pub-lock hold (it is released by the deferred releaseLock at function
	// return) — the same shards-then-state atomicity publishGeneration documents.
	// Moving the state write out of the lock would let a reader observe new
	// shards with a stale-matching state label (the torn-state bug). ---
	pub, err := acquirePublishLock(ctx, cacheDir)
	if err != nil {
		if errors.Is(err, errCorpusEvicted) {
			return nil // corpus gc-evicted during build; next search rebuilds
		}
		return err
	}
	defer releaseLock(pub)
	if _, sErr := os.Stat(indexDir); sErr != nil {
		return nil // evicted; discard
	}

	if needCommitted {
		if err := publishShardFamilyLocked(cacheDir, indexDir, buildDir, familyCommitted); err != nil {
			return fmt.Errorf("publish committed shards: %w", err)
		}
	}

	if hasDirty {
		uncommittedErr := gitCorpusError(
			repoDir,
			indexDir,
			indexUncommitted(ctx, repoDir, indexDir, cacheDir, state, cachedState, preState, parallelism),
		)
		if uncommittedErr != nil {
			deleteStateFiles(cacheDir)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return uncommittedErr
		}
	} else {
		cleanUncommittedShards(indexDir)
		deleteUncommittedManifest(cacheDir)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		deleteStateFiles(cacheDir)
		return ctxErr
	}

	// Re-stat the dirty files to detect changes during the (sub-second)
	// uncommitted build window.
	postState := gitCorpusStateHash(paths, state)

	if postState == preState {
		if err := writeStateFile(cacheDir, preState); err != nil {
			return fmt.Errorf("write state file: %w", err)
		}
		// Persist HEAD so working-tree-only changes skip the committed indexer.
		if err := writeHeadFile(cacheDir, state.HeadSHA); err != nil {
			slog.Warn("Failed to write head file", "error", err)
		}
	} else {
		deleteStateFiles(cacheDir)
		slog.Warn("Index may be stale, will re-index on next search")
	}

	return nil
}

// validateCommittedBuildHead distinguishes a real HEAD change from a failed
// validation. A changed HEAD discards the new build and keeps the live index.
// Cancellation and other Git failures propagate to the corpus worker.
func validateCommittedBuildHead(
	ctx context.Context,
	paths gitPaths,
	expectedTreeish string,
) (bool, error) {
	err := validateCommittedHead(ctx, paths, expectedTreeish)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errGitIndexStateChanged):
		slog.Warn("HEAD moved during committed index; will re-index on next search")
		return false, nil
	default:
		return false, err
	}
}

// shardsExist checks if any *.zoekt shard files exist in the index directory.
func shardsExist(indexDir string) bool {
	entries, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	return err == nil && len(entries) > 0
}

// maxCommittedDeltaShards bounds delta-stacked committed shards before Zoekt
// forces a full rebuild. Matches the folder side cap (folder_indexer.go) so
// search-time tombstone lookup cost stays bounded.
const maxCommittedDeltaShards = 64

// indexCommitted indexes committed files using gitindex.IndexGitRepo.
//
// IsDelta=true makes Zoekt diff the tree between the last indexed commit and
// the current HEAD, indexing only changed blobs and tombstoning the rest via
// per-shard .meta sidecars. Zoekt falls back to a full rebuild on its own when
// the prior commit is gone (force-push, GC), branch set changes, index option
// hash differs, or the shard count exceeds DeltaShardNumberFallbackThreshold.
//
// The fallback repository name gives Zoekt a non-empty shard namespace for
// local repos with no remote.origin.url and no [zoekt] name. Zoekt still
// overwrites it when repo config or origin metadata provides a real name.
func indexCommitted(repoDir, indexDir string, parallelism int) error {
	buildOpts := indexBuildOptions(indexDir, parallelism)
	buildOpts.IsDelta = true
	buildOpts.RepositoryDescription.Name = fallbackGitRepositoryName(repoDir)
	opts := gitindex.Options{
		RepoDir:                           repoDir,
		Incremental:                       true,
		Branches:                          []string{"HEAD"},
		BuildOptions:                      buildOpts,
		DeltaShardNumberFallbackThreshold: maxCommittedDeltaShards,
	}
	_, err := gitindex.IndexGitRepo(opts)
	if err != nil {
		return gitCorpusError(repoDir, indexDir, err)
	}
	return nil
}

func indexScopedCommitted(ctx context.Context, repoDir, indexDir, treeish string, scope *gitDirtyScope, parallelism int) (bool, error) {
	repoName := fallbackGitRepositoryName(repoDir)
	errCh := make(chan error, 1)
	fileCh := streamGitTreeBlobs(ctx, repoDir, treeish, scope, errCh)
	indexedAny, err := indexDocuments(ctx, indexDir, repoName, repoDir, fileCh, parallelism)
	if err != nil {
		return indexedAny, err
	}
	select {
	case err := <-errCh:
		if err != nil {
			return indexedAny, err
		}
	default:
	}
	return indexedAny, nil
}

func streamGitTreeBlobs(ctx context.Context, repoDir, treeish string, scope *gitDirtyScope, errCh chan<- error) <-chan fileContent {
	out := make(chan fileContent)
	go func() {
		defer close(out)

		args := []string{"ls-tree", "-r", "-l", "-z", treeish, "--"}
		args = append(args, scope.gitIncludePathspecs()...)
		lsCmd := gitCmd(ctx, args...)
		lsCmd.Dir = repoDir
		lsStdout, err := lsCmd.StdoutPipe()
		if err != nil {
			sendGitBlobStreamErr(errCh, fmt.Errorf("open git ls-tree stdout: %w", err))
			return
		}
		var lsStderr strings.Builder
		lsCmd.Stderr = &lsStderr
		if err := lsCmd.Start(); err != nil {
			sendGitBlobStreamErr(errCh, fmt.Errorf("start git ls-tree: %w", err))
			return
		}
		abortLs := func() {
			if lsCmd.Process != nil {
				_ = lsCmd.Process.Kill()
			}
			_ = lsCmd.Wait()
		}

		var catCmd *exec.Cmd
		var catStdin io.WriteCloser
		var catReader *bufio.Reader
		var catStderr strings.Builder
		closeCat := func() {
			if catCmd == nil {
				return
			}
			_ = catStdin.Close()
			if err := catCmd.Wait(); err != nil {
				if msg := strings.TrimSpace(catStderr.String()); msg != "" {
					sendGitBlobStreamErr(errCh, fmt.Errorf("git cat-file: %w: %s", err, msg))
					return
				}
				sendGitBlobStreamErr(errCh, fmt.Errorf("git cat-file: %w", err))
			}
		}
		abortCat := func() {
			if catCmd == nil {
				return
			}
			if catCmd.Process != nil {
				_ = catCmd.Process.Kill()
			}
			_ = catCmd.Wait()
			catCmd = nil
		}
		defer closeCat()
		startCat := func() error {
			if catCmd != nil {
				return nil
			}
			cmd := gitCmd(ctx, "cat-file", "--batch")
			cmd.Dir = repoDir
			stdin, err := cmd.StdinPipe()
			if err != nil {
				return fmt.Errorf("open git cat-file stdin: %w", err)
			}
			catStdout, err := cmd.StdoutPipe()
			if err != nil {
				return fmt.Errorf("open git cat-file stdout: %w", err)
			}
			cmd.Stderr = &catStderr
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("start git cat-file: %w", err)
			}
			catCmd = cmd
			catStdin = stdin
			catReader = bufio.NewReaderSize(catStdout, 64*1024)
			return nil
		}

		lsReader := bufio.NewReaderSize(lsStdout, 64*1024)
		var longRecord []byte
		var readErr error
		for {
			record, err := lsReader.ReadSlice(0)
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
				if ok && scope.contains(blob.name) && blob.size <= maxIndexedDocumentBytes {
					if err := ctx.Err(); err != nil {
						abortLs()
						abortCat()
						sendGitBlobStreamErr(errCh, err)
						return
					}
					if err := startCat(); err != nil {
						abortLs()
						sendGitBlobStreamErr(errCh, err)
						return
					}
					if _, err := fmt.Fprintln(catStdin, blob.oid); err != nil {
						abortLs()
						abortCat()
						sendGitBlobStreamErr(errCh, fmt.Errorf("write git cat-file request: %w", err))
						return
					}
					content, weight, err := readGitBlobFromCatFile(ctx, catReader, blob)
					if err != nil {
						abortLs()
						abortCat()
						sendGitBlobStreamErr(errCh, err)
						return
					}
					if content == nil {
						continue
					}
					select {
					case out <- fileContent{name: blob.name, content: content, weight: weight}:
					case <-ctx.Done():
						if weight > 0 {
							readSemaphore.Release(weight)
						}
						abortLs()
						abortCat()
						sendGitBlobStreamErr(errCh, ctx.Err())
						return
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
			_ = lsCmd.Process.Kill()
			_ = lsCmd.Wait()
			sendGitBlobStreamErr(errCh, fmt.Errorf("read git ls-tree: %w", readErr))
			return
		}
		if err := lsCmd.Wait(); err != nil {
			if msg := strings.TrimSpace(lsStderr.String()); msg != "" {
				sendGitBlobStreamErr(errCh, fmt.Errorf("git ls-tree: %w: %s", err, msg))
				return
			}
			sendGitBlobStreamErr(errCh, fmt.Errorf("git ls-tree: %w", err))
		}
	}()
	return out
}

func readGitBlobFromCatFile(ctx context.Context, reader *bufio.Reader, blob gitTreeBlob) ([]byte, int64, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, 0, fmt.Errorf("read git cat-file header for %s: %w", blob.name, err)
	}
	fields := strings.Fields(header)
	if len(fields) < 3 || fields[1] != "blob" {
		return nil, 0, fmt.Errorf("unexpected git cat-file header for %s: %q", blob.name, strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return nil, 0, fmt.Errorf("parse git cat-file size for %s: %q", blob.name, fields[2])
	}
	if size > maxIndexedDocumentBytes {
		if _, err := io.CopyN(io.Discard, reader, size+1); err != nil {
			return nil, 0, fmt.Errorf("skip large git blob %s: %w", blob.name, err)
		}
		return nil, 0, nil
	}
	weight := size
	if err := readSemaphore.Acquire(ctx, weight); err != nil {
		return nil, 0, err
	}
	content := make([]byte, int(size))
	if _, err := io.ReadFull(reader, content); err != nil {
		if weight > 0 {
			readSemaphore.Release(weight)
		}
		return nil, 0, fmt.Errorf("read git blob %s: %w", blob.name, err)
	}
	delim, err := reader.ReadByte()
	if err != nil {
		if weight > 0 {
			readSemaphore.Release(weight)
		}
		return nil, 0, fmt.Errorf("read git blob delimiter %s: %w", blob.name, err)
	}
	if delim != '\n' {
		if weight > 0 {
			readSemaphore.Release(weight)
		}
		return nil, 0, fmt.Errorf("unexpected git blob delimiter %s: %q", blob.name, delim)
	}
	return content, weight, nil
}

func sendGitBlobStreamErr(errCh chan<- error, err error) {
	if err == nil {
		return
	}
	select {
	case errCh <- err:
	default:
	}
}

// fallbackGitRepositoryName returns a stable opaque name for Git committed
// shards when Zoekt cannot derive one from repo metadata. It is intentionally
// not based on basename: "uncommitted" is reserved for dirty-file shards.
func fallbackGitRepositoryName(repoDir string) string {
	return "git-" + hashParts("git_repository_name", canonicalCorpusPath(repoDir))
}

// fileContent carries one file's content from reader to consumer.
//
// weight is the byte count Acquired from readSemaphore at read time.
// Zoekt's Builder.Add buffers Content into b.todo and the per-shard
// goroutines spawned by flush keep referencing it until
// b.building.Wait returns inside Finish. So Release MUST happen
// strictly after that Finish:
//
//   - indexDocuments runs a sequence of windowed Builders. Each doc's
//     weight feeds the current window's pendingWeight ledger and is
//     Released when the window's Finish returns.
//   - indexDeltaDocuments runs a single Builder. The full doc set is
//     Released via releaseFileContentWeights after the terminal Finish.
//
// Zero weight means the reader did not Acquire (synchronous folder-
// delta reads via readFolderFile bypass the semaphore by design).
// Release of zero is a no-op.
type fileContent struct {
	name    string
	content []byte
	weight  int64
}

// readFilesToChannel reads files from the working tree using a bounded
// worker pool and sends them to out. Skips files larger than
// maxGitDirtyFileSize, symlinks, and directories. Individual file
// failures are non-fatal (files may be deleted or modified between
// git status and read). The channel is closed after all workers exit.
//
// Each per-file read Acquires byte weight from readSemaphore before
// opening, then transfers Release ownership to the consumer via
// fileContent.weight on successful send. On any failure path between
// Acquire and successful send, the deferred `released` sentinel
// Releases. See fileContent for the consumer's Release contract.
func readFilesToChannel(ctx context.Context, repoDir string, files []string, parallelism int, out chan<- fileContent) {
	workers := fileReadWorkerCount(parallelism, len(files))
	ch := make(chan string, workers)
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				readOneDirtyFile(ctx, repoDir, f, out)
			}
		}()
	}

	for _, f := range files {
		select {
		case ch <- f:
		case <-ctx.Done():
			// Symmetry with streamFolderFiles (folder_indexer.go:974-978):
			// stop feeding workers on cancellation so the producer never
			// outlives the consumers. Workers exit their range loop on
			// close(ch) below.
			close(ch)
			wg.Wait()
			close(out)
			return
		}
	}
	close(ch)
	wg.Wait()
	close(out)
}

// readOneDirtyFile factors the per-file read so deferred Release of
// readSemaphore weight runs on every exit path (including panics inside
// io.ReadFull). The `released` sentinel transfers ownership to the
// consumer once the channel send succeeds.
func readOneDirtyFile(ctx context.Context, repoDir, f string, out chan<- fileContent) {
	srcPath := filepath.Join(repoDir, f)

	// Use Lstat to avoid following symlinks
	fi, err := os.Lstat(srcPath)
	if err != nil {
		return
	}

	// Only process regular files — skip directories (dirty
	// submodules), symlinks, FIFOs, sockets, and devices to
	// avoid blocking or reading unexpected data.
	if !fi.Mode().IsRegular() {
		return
	}

	size := fi.Size()
	if size > maxGitDirtyFileSize {
		slog.Warn("Skipping large uncommitted file", "path", f, "size_mb", size/(1024*1024))
		return
	}
	weight := size
	if err := readSemaphore.Acquire(ctx, weight); err != nil {
		return // ctx cancelled or done; semaphore not Acquired.
	}
	released := false
	defer func() {
		if !released {
			readSemaphore.Release(weight)
		}
	}()

	// Read using the known size from Lstat to avoid the extra
	// Fstat that os.ReadFile performs internally. A single
	// Read is not guaranteed to fill the buffer, even for
	// regular files, so use ReadFull and keep partial content
	// only when the file shrank during the read.
	fh, err := os.Open(srcPath)
	if err != nil {
		return
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(fh, buf)
	_ = fh.Close()
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return
	}
	if n == 0 && size > 0 {
		return
	}

	// Transfer Release ownership to the consumer. The select unblocks
	// on ctx cancel so an abandoned consumer cannot wedge the worker
	// holding semaphore weight indefinitely. released stays false on
	// the ctx.Done path so the deferred sentinel returns the weight.
	select {
	case out <- fileContent{name: f, content: buf[:n], weight: weight}:
		released = true
	case <-ctx.Done():
		return
	}
}

// streamFiles returns a channel that yields file contents read from
// the working tree. The output channel is unbuffered because Zoekt's
// public Builder API accepts []byte documents, not io.Reader streams.
// In-flight memory is bounded by two limits, whichever is tighter:
//   - the worker count (each worker holds at most one fileContent),
//   - readSemaphore's maxInFlightBytes byte budget (see caps.go).
//
// Each yielded fileContent carries a weight (= file size at Lstat) on
// the readSemaphore. The consumer MUST Release that weight after
// builder.Finish() returns. Abandoning the channel without draining
// would hang workers blocked on send until ctx cancellation.
func streamFiles(ctx context.Context, repoDir string, files []string, parallelism int) <-chan fileContent {
	out := make(chan fileContent)
	go readFilesToChannel(ctx, repoDir, files, parallelism, out)
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

// indexUncommitted indexes the working-tree dirty files for repoDir into the
// "uncommitted" Zoekt repo.
//
// State binding: the manifest is written tagged with preState (the freshly
// computed state hash for this cycle). On the next cycle, runIndexingWithCache
// reads .state — which now contains preState — and passes it back as
// cachedState. tryUncommittedDelta then loads the manifest by matching
// expectedState == cachedState. This mirrors the folder side at
// cmd/seek/folder_indexer.go:157-160 where the state file and manifest are
// written under the same stateHash.
//
// When a prior manifest is present and the existing shard count is below
// maxUncommittedDeltaShards, only files whose (size, mtime, ino) changed are
// re-read and written into a delta shard; vanished files become tombstones in
// the .meta sidecars of older shards. Falls back to a full rebuild when the
// manifest is missing/stale, the shard count exceeds the cap, or the delta
// path returns an error.
func indexUncommitted(
	ctx context.Context,
	repoDir, indexDir, cacheDir string,
	state repoState,
	cachedState, preState string,
	parallelism int,
) error {
	if len(state.Files) == 0 {
		cleanUncommittedShards(indexDir)
		deleteUncommittedManifest(cacheDir)
		return nil
	}

	candidates := statUncommittedCandidates(repoDir, state.Files)
	entries := make([]uncommittedManifestEntry, 0, len(candidates))
	for _, c := range candidates {
		entries = append(entries, c.manifestEntry())
	}

	if cachedState != "" {
		cleanEmptyShards(ctx, indexDir, repoUncommitted)
		shardCount := repositoryShardCount(indexDir, repoUncommitted)
		if shardCount > 0 && shardCount <= maxUncommittedDeltaShards {
			if err := tryUncommittedDelta(ctx, repoDir, indexDir, cacheDir, candidates, cachedState, preState, entries); err == nil {
				return nil
			} else if errors.Is(err, errDeltaPayloadExceedsWindow) {
				slog.Debug("Uncommitted delta payload exceeds window threshold; full rebuild", "error", err)
			} else {
				slog.Debug("Uncommitted delta indexing failed, falling back to full rebuild", "error", err)
			}
		} else if shardCount > maxUncommittedDeltaShards {
			slog.Debug("Uncommitted delta shard limit reached, falling back to full rebuild", "shards", shardCount)
		}
	}

	fileCh := streamFiles(ctx, repoDir, state.Files, parallelism)
	_, err := indexDocuments(ctx, indexDir, repoUncommitted, repoDir, fileCh, parallelism)
	if err != nil {
		deleteUncommittedManifest(cacheDir)
		return err
	}
	if err := writeUncommittedManifest(cacheDir, preState, entries); err != nil {
		slog.Debug("Failed to write uncommitted manifest", "error", err)
	}
	return nil
}

// tryUncommittedDelta attempts a delta-only rebuild of the uncommitted shard
// set. The manifest is looked up by cachedState (the .state value persisted
// at the end of the previous cycle) and re-written tagged with preState (the
// new .state value about to be persisted by runIndexingWithCache).
func tryUncommittedDelta(
	ctx context.Context,
	repoDir, indexDir, cacheDir string,
	candidates []uncommittedCandidate,
	cachedState, preState string,
	entries []uncommittedManifestEntry,
) error {
	manifest, ok := readUncommittedManifest(cacheDir, cachedState)
	if !ok {
		return fmt.Errorf("uncommitted manifest missing or stale")
	}
	toRead, changedPaths := diffUncommittedAgainstManifest(candidates, manifest)
	if len(changedPaths) == 0 {
		// No file content drifted since the manifest was written — the
		// state hash must have changed for some other reason (HEAD,
		// untracked file added then removed, etc.). Refresh the manifest
		// binding without touching shards.
		if err := writeUncommittedManifest(cacheDir, preState, entries); err != nil {
			slog.Debug("Failed to refresh uncommitted manifest", "error", err)
		}
		return nil
	}

	docs, err := readUncommittedCandidates(ctx, repoDir, toRead)
	if err != nil {
		// readUncommittedCandidates returns (nil, errDeltaPayloadExceedsWindow)
		// after releasing all weights internally — no further work here.
		return err
	}
	if len(docs) == 0 && len(changedPaths) > 0 {
		// Pure tombstone cycle (all changes are removals). Zoekt's
		// builder rejects empty delta builds with the same error
		// shape as the folder side; surface that as a fallback signal.
		// docs is empty here; no weights to release.
		return fmt.Errorf("uncommitted delta contains only removals")
	}
	// indexDeltaDocuments releases the per-doc readSemaphore weights
	// after builder.Finish() returns; do not release here.
	if _, err := indexDeltaDocuments(indexDir, repoUncommitted, repoDir, docs, uncommittedDeltaShardMax, changedPaths); err != nil {
		return err
	}
	if err := writeUncommittedManifest(cacheDir, preState, entries); err != nil {
		slog.Debug("Failed to write uncommitted manifest", "error", err)
	}
	return nil
}

// readUncommittedCandidates loads file contents for the given
// candidates from the working tree. Missing or non-regular files are
// skipped silently (same policy as readFilesToChannel).
//
// Two guards prevent a synchronous drain >= indexWindowBytes from
// wedging the global readSemaphore:
//   - Pre-flight: sum candidate.size (lstat already populated). If
//     it already exceeds the window threshold, return immediately
//     without spawning any reader — saves up to indexWindowBytes of
//     wasted read I/O.
//   - Bounded drain: if cumulative weight reaches the threshold
//     mid-drain, cancel the inner context so workers unwind via
//     Acquire's ctx-aware abort, then drain-and-release the remainder.
//
// Both paths return errDeltaPayloadExceedsWindow. Caller
// indexUncommitted catches via errors.Is and falls back to the
// windowed full rebuild (streamFiles → indexDocuments).
func readUncommittedCandidates(ctx context.Context, repoDir string, candidates []uncommittedCandidate) ([]fileContent, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	var preSum int64
	for _, c := range candidates {
		preSum += c.size
		if preSum >= indexWindowBytes {
			return nil, errDeltaPayloadExceedsWindow
		}
	}
	files := make([]string, len(candidates))
	for i, c := range candidates {
		files[i] = c.name
	}
	innerCtx, cancelInner := context.WithCancel(ctx)
	defer cancelInner()
	ch := streamFiles(innerCtx, repoDir, files, fileReadWorkerCount(indexParallelism(), len(files)))
	docs := make([]fileContent, 0, len(files))
	var cumulative int64
	exceeded := false
	for doc := range ch {
		if exceeded {
			// Drain the rest releasing inline so workers (still holding
			// weight on Acquired-but-not-yet-cancelled reads) and the
			// producer goroutine can exit cleanly.
			if doc.weight > 0 {
				readSemaphore.Release(doc.weight)
			}
			continue
		}
		docs = append(docs, doc)
		cumulative += doc.weight
		if cumulative >= indexWindowBytes {
			exceeded = true
			cancelInner()
		}
	}
	if exceeded {
		releaseFileContentWeights(docs)
		return nil, errDeltaPayloadExceedsWindow
	}
	return docs, nil
}

// indexDocuments consumes fileContent from fileCh and feeds each
// Content into a rotating series of *index.Builder windows. Each
// window accumulates up to indexWindowBytes of doc weight before
// builder.Finish() runs and the window's pending weight is released
// to readSemaphore. See fileContent for the per-doc Release contract.
//
// Window 0 opens with IsDelta=false so Zoekt's Finish prunes stale
// shards from prior runs via FindAllShards. Windows 1..N open with
// IsDelta=true + nil changedPaths: shard deletion + tombstone writes
// are skipped, and shard numbering resumes from FindAllShards so new
// shards do not collide with prior ones.
//
// Returns (indexed, err); indexed=true means a builder was opened.
func indexDocuments(
	ctx context.Context,
	indexDir string,
	repoName string,
	source string,
	fileCh <-chan fileContent,
	parallelism int,
) (bool, error) {
	var current *index.Builder
	var pendingWeight int64
	var addErr error
	indexedAny := false
	openedWindows := 0

	openWindow := func(isDelta bool) error {
		opts := indexBuildOptions(indexDir, parallelism)
		opts.RepositoryDescription.Name = repoName
		opts.RepositoryDescription.Source = source
		opts.IsDelta = isDelta
		b, err := index.NewBuilder(opts)
		if err != nil {
			return fmt.Errorf("create builder: %w", err)
		}
		current = b
		openedWindows++
		indexedAny = true
		return nil
	}

	// finishWindow Finishes the current builder and Releases pending
	// weight. Always Releases even on Finish error: Finish's
	// b.building.Wait() has joined the shard goroutines (or aborted
	// them via b.buildError) before returning, so no goroutine retains
	// any doc.Content past this point.
	finishWindow := func() error {
		if current == nil {
			return nil
		}
		err := current.Finish()
		if pendingWeight > 0 {
			readSemaphore.Release(pendingWeight)
			pendingWeight = 0
		}
		current = nil
		return err
	}

	drainRemaining := func() {
		for d := range fileCh {
			if d.weight > 0 {
				readSemaphore.Release(d.weight)
			}
		}
	}

	for doc := range fileCh {
		if err := ctx.Err(); err != nil {
			if doc.weight > 0 {
				readSemaphore.Release(doc.weight)
			}
			drainRemaining()
			_ = finishWindow()
			return indexedAny, err
		}
		if addErr != nil {
			if doc.weight > 0 {
				readSemaphore.Release(doc.weight)
			}
			continue
		}

		if current == nil {
			if err := openWindow(openedWindows > 0); err != nil {
				if doc.weight > 0 {
					readSemaphore.Release(doc.weight)
				}
				drainRemaining()
				return indexedAny, err
			}
		}

		// Zoekt's Builder.Add buffers doc into b.todo before any error
		// return, so doc.weight stays the window's responsibility either
		// way and is Released by finishWindow.
		pendingWeight += doc.weight
		if err := current.Add(index.Document{Name: doc.name, Content: doc.content}); err != nil {
			addErr = fmt.Errorf("add document %s: %w", doc.name, err)
			continue
		}

		if pendingWeight >= indexWindowBytes {
			if err := finishWindow(); err != nil {
				drainRemaining()
				return indexedAny, err
			}
		}
	}

	if current != nil {
		if err := finishWindow(); err != nil {
			if addErr != nil {
				return true, addErr
			}
			return true, err
		}
	} else if !indexedAny {
		cleanRepositoryShards(indexDir, repoName)
		return false, nil
	}

	if addErr != nil {
		return true, addErr
	}
	return true, nil
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
