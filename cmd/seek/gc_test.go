package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCorpusHash returns a hex string of corpusHashHexLen deterministic from
// id — same shape as production newCorpusID but cheap to construct. Returns
// plain string so callers can use it as both a path component and a
// corpusID (string typedef converts implicitly via cast where needed).
// Distinct ids always produce distinct hashes (zero-padded hex encoding).
func fakeCorpusHash(id int) string {
	return fmt.Sprintf("%0*x", corpusHashHexLen, id)
}

// seedCorpus creates corpora/<hash>/ with a .used marker mtime'd to usedAt.
// Returns the cache-dir path.
func seedCorpus(tb testing.TB, cacheRoot, hash string, usedAt time.Time) string {
	tb.Helper()
	dir := filepath.Join(cacheRoot, corporaDir, hash)
	if err := os.MkdirAll(filepath.Join(dir, "index"), 0o755); err != nil {
		tb.Fatalf("mkdir corpus: %v", err)
	}
	usedPath := filepath.Join(dir, usedFile)
	if err := os.WriteFile(usedPath, nil, 0o644); err != nil {
		tb.Fatalf("write used: %v", err)
	}
	if err := os.Chtimes(usedPath, usedAt, usedAt); err != nil {
		tb.Fatalf("chtimes used: %v", err)
	}
	return dir
}

func cacheRootForTest(tb testing.TB) string {
	tb.Helper()
	clearGCEnvForTest(tb)
	setTestUserCache(tb)
	root, err := seekUserCacheRoot()
	if err != nil {
		tb.Fatalf("seekUserCacheRoot: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		tb.Fatalf("mkdir cache root: %v", err)
	}
	return root
}

func clearGCEnvForTest(tb testing.TB) {
	tb.Helper()
	tb.Setenv(envGCMaxAge, "")
	tb.Setenv(envGCInterval, "")
}

func resetNFSCheck(tb testing.TB) {
	tb.Helper()
	// Reset the process-lifetime NFS-detection gate used by touchUsed.
	nfsCheckOnce = sync.Once{}
	nfsCached = false
	tb.Cleanup(func() {
		nfsCheckOnce = sync.Once{}
		nfsCached = false
	})
}

// captureStdoutGC swaps os.Stdout with a pipe, runs fn, and returns
// everything fn wrote. Restores os.Stdout on return — including if fn
// panics, so a failing test does not poison stdout for the rest of the
// suite. NOT safe for t.Parallel: mutates the process-wide os.Stdout.
func captureStdoutGC(tb testing.TB, fn func()) (out string) {
	tb.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		tb.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()
	// Defer must run after fn() so a panic still restores stdout and
	// drains the reader before the value is read back into `out`.
	defer func() {
		os.Stdout = orig
		_ = w.Close()
		<-done
		out = buf.String()
	}()
	fn()
	return
}

// requireRow asserts the captured table output contains a row for the given
// corpus (matched by its truncated hash) ending in the expected action token.
// Substring match is robust to column-width tweaks.
func requireRow(tb testing.TB, out, hash, action string) {
	tb.Helper()
	short := truncateHash(corpusID(hash))
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, short) {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(line), action) {
			return
		}
		tb.Fatalf("row for %s found but action mismatch: %q (want suffix %q)\nfull output:\n%s",
			short, line, action, out)
	}
	tb.Fatalf("no row for corpus %s (action %q) in output:\n%s", short, action, out)
}

func TestGC_ThrottleGate_SkipsWhenStampFresh(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(1), time.Now().Add(-30*24*time.Hour))

	// Fresh stamp (now) — should block GC.
	if err := os.WriteFile(filepath.Join(root, gcStampFile), nil, 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stale corpus removed despite fresh stamp: %v", err)
	}
}

func TestGC_ThrottleGate_RunsWhenStampMissing(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(2), time.Now().Add(-30*24*time.Hour))

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should be evicted: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, gcStampFile)); err != nil {
		t.Fatalf("stamp should be written after run: %v", err)
	}
}

func TestGC_ThrottleGate_RunsWhenStampOld(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(3), time.Now().Add(-30*24*time.Hour))

	stamp := filepath.Join(root, gcStampFile)
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatalf("chtimes stamp: %v", err)
	}

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should be evicted with old stamp: err=%v", err)
	}
}

func TestGC_GlobalLockContention_SkipsWhenHeld(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(4), time.Now().Add(-30*24*time.Hour))

	// Externally hold gc.lock.
	lockFd, err := os.OpenFile(filepath.Join(root, gcLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open gc.lock: %v", err)
	}
	defer func() { unlockFile(lockFd); _ = lockFd.Close() }()
	if err := lockFileExclusive(lockFd); err != nil {
		t.Fatalf("hold gc.lock: %v", err)
	}

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stale corpus should survive when gc.lock contended: %v", err)
	}
}

func TestGC_EvictsCorpusBeyondTTL(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(5), time.Now().Add(-15*24*time.Hour))

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should be evicted: err=%v", err)
	}
}

func TestGC_KeepsCorpusWithinTTL(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(6), time.Now().Add(-1*24*time.Hour))

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("recently-used corpus should be kept: %v", err)
	}
}

func TestGC_CrashRecovery_DrainsTrash(t *testing.T) {
	root := cacheRootForTest(t)
	trashEntry := filepath.Join(root, corporaDir, gcTrashDir, "leftover-12345")
	if err := os.MkdirAll(filepath.Join(trashEntry, "index"), 0o755); err != nil {
		t.Fatalf("seed trash: %v", err)
	}
	// Add a file to verify RemoveAll actually descends.
	if err := os.WriteFile(filepath.Join(trashEntry, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed trash marker: %v", err)
	}

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(trashEntry); !os.IsNotExist(err) {
		t.Fatalf("trash should be drained: err=%v", err)
	}
}

func TestGC_TouchUsed_UpdatesMtime(t *testing.T) {
	root := cacheRootForTest(t)
	resetNFSCheck(t)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(7))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir corpus: %v", err)
	}

	before := time.Now()
	touchUsed(dir)
	st, err := os.Stat(filepath.Join(dir, usedFile))
	if err != nil {
		t.Fatalf("stat .used: %v", err)
	}
	if st.ModTime().Before(before.Add(-time.Second)) {
		t.Fatalf(".used mtime %v older than before %v", st.ModTime(), before)
	}

	// Second touch updates mtime.
	earlier := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, usedFile), earlier, earlier); err != nil {
		t.Fatalf("chtimes back-date: %v", err)
	}
	touchUsed(dir)
	st2, err := os.Stat(filepath.Join(dir, usedFile))
	if err != nil {
		t.Fatalf("stat .used: %v", err)
	}
	if !st2.ModTime().After(earlier.Add(time.Minute)) {
		t.Fatalf(".used not re-touched: %v", st2.ModTime())
	}
}

func TestGC_BackwardCompat_NoUsedFile_UsesDirMtime(t *testing.T) {
	root := cacheRootForTest(t)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(8))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir corpus: %v", err)
	}
	// No .used marker. Set dir mtime to 15d ago.
	old := time.Now().Add(-15 * 24 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes dir: %v", err)
	}

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("corpus without .used should fall back to dir mtime and evict: err=%v", err)
	}
}

func TestGC_PerCorpusActive_SkipsLockedCorpus(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(9), time.Now().Add(-30*24*time.Hour))

	// Externally hold the corpus .lock LOCK_EX.
	lockPath := filepath.Join(dir, lockFile)
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open corpus lock: %v", err)
	}
	defer func() { unlockFile(holder); _ = holder.Close() }()
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("hold corpus lock: %v", err)
	}

	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge}, defaultGCInterval)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("locked corpus should not be evicted: %v", err)
	}
}

func TestGC_TOCTOU_RefreshedUsedUnderLock_SkipsEvict(t *testing.T) {
	root := cacheRootForTest(t)
	hash := fakeCorpusHash(10)
	dir := seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))

	// Simulate concurrent USED bump: re-touch .used to "now" before runGC
	// even starts. The enumerator captures the old mtime via ReadDir +
	// stat ordering, but the under-lock re-stat in evictCorpus sees fresh
	// mtime and must skip.
	//
	// We can't easily race the in-process call, so emulate it by directly
	// calling evictCorpus with a pre-built corpusDirEntry referencing the
	// stale time while .used on disk is fresh.
	entry := corpusDirEntry{name: corpusID(hash), path: dir, usedAt: time.Now().Add(-30 * 24 * time.Hour)}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, usedFile), now, now); err != nil {
		t.Fatalf("chtimes used: %v", err)
	}

	trashDir := filepath.Join(root, corporaDir, gcTrashDir)
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		t.Fatalf("mkdir trash: %v", err)
	}
	res := evictCorpus(entry, trashDir, time.Now().Add(-defaultGCMaxAge))
	if res.action != actionKept {
		t.Fatalf("expected actionKept after .used refresh under lock, got %s err=%v", res.String(), res.err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("corpus should remain after TOCTOU skip: %v", err)
	}
}

func TestGC_EnvMaxAgeOverride(t *testing.T) {
	root := cacheRootForTest(t)
	t.Setenv(envGCMaxAge, "1h")

	stale := seedCorpus(t, root, fakeCorpusHash(11), time.Now().Add(-2*time.Hour))
	fresh := seedCorpus(t, root, fakeCorpusHash(12), time.Now().Add(-30*time.Minute))

	cfg := gcConfigFromEnv()
	runGC(context.Background(), gcOptions{maxAge: cfg.maxAge}, cfg.interval)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("2h corpus should evict under 1h TTL: err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("30m corpus should remain under 1h TTL: %v", err)
	}
}

func TestGC_TrashDir_NotEnumeratedAsCorpus(t *testing.T) {
	root := cacheRootForTest(t)
	// Place a non-hex name and the .trash dir; neither should be enumerated.
	if err := os.MkdirAll(filepath.Join(root, corporaDir, gcTrashDir, "leftover"), 0o755); err != nil {
		t.Fatalf("mkdir trash entry: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, corporaDir, "not-a-hash"), 0o755); err != nil {
		t.Fatalf("mkdir invalid: %v", err)
	}
	valid := seedCorpus(t, root, fakeCorpusHash(13), time.Now())

	entries, err := enumerateCorpusDirs(filepath.Join(root, corporaDir))
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 valid corpus, got %d: %+v", len(entries), entries)
	}
	if entries[0].path != valid {
		t.Fatalf("wrong corpus surfaced: %s", entries[0].path)
	}
}

func TestGC_TouchUsed_LatencySubMillisecond(t *testing.T) {
	if testing.Short() {
		t.Skip("latency check")
	}
	root := cacheRootForTest(t)
	resetNFSCheck(t)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(14))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	touchUsed(dir) // warm: ensure .used exists

	const N = 1000
	start := time.Now()
	for i := 0; i < N; i++ {
		touchUsed(dir)
	}
	elapsed := time.Since(start)
	avg := elapsed / N
	if avg > time.Millisecond {
		t.Fatalf("touchUsed too slow: avg=%v over %d calls", avg, N)
	}
}

func TestGC_TouchUsed_NFSGate_SkipsWhenCached(t *testing.T) {
	root := cacheRootForTest(t)
	resetNFSCheck(t)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(15))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pretend cache is on NFS.
	nfsCheckOnce.Do(func() { nfsCached = true })

	touchUsed(dir)

	if _, err := os.Stat(filepath.Join(dir, usedFile)); !os.IsNotExist(err) {
		t.Fatalf(".used should not be created on NFS: err=%v", err)
	}
}

func TestGCCmd_DryRun_NoEvictions(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(16), time.Now().Add(-30*24*time.Hour))

	// Capture stdout
	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	go func() {
		_ = runGCCommand(context.Background(), []string{"--dry-run"})
		_ = w.Close()
	}()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	out := buf.String()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dry-run should not evict: %v", err)
	}
	if !strings.Contains(out, "evict") {
		t.Fatalf("dry-run output should mention evict action: %q", out)
	}
}

func TestGCCmd_Force_BypassesThrottle(t *testing.T) {
	root := cacheRootForTest(t)
	hash := fakeCorpusHash(17)
	dir := seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))
	if err := os.WriteFile(filepath.Join(root, gcStampFile), nil, 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	out := captureStdoutGC(t, func() {
		if err := runGCCommand(context.Background(), []string{"--force"}); err != nil {
			t.Fatalf("runGCCommand: %v", err)
		}
	})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("--force should bypass throttle and evict: err=%v", err)
	}
	requireRow(t, out, hash, "evicted")
}

func TestGCCmd_All_EvictsEverything(t *testing.T) {
	root := cacheRootForTest(t)
	staleHash := fakeCorpusHash(18)
	freshHash := fakeCorpusHash(19)
	stale := seedCorpus(t, root, staleHash, time.Now().Add(-30*24*time.Hour))
	fresh := seedCorpus(t, root, freshHash, time.Now())

	out := captureStdoutGC(t, func() {
		if err := runGCCommand(context.Background(), []string{"--all", "--force"}); err != nil {
			t.Fatalf("runGCCommand: %v", err)
		}
	})

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should evict under --all: err=%v", err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("fresh corpus should also evict under --all: err=%v", err)
	}
	requireRow(t, out, staleHash, "evicted")
	requireRow(t, out, freshHash, "evicted")
}

// TestGCCmd_All_BypassesThrottle covers the audit fix: `seek gc --all`
// (without --force) must wipe even when .last-gc is fresh.
func TestGCCmd_All_BypassesThrottle(t *testing.T) {
	root := cacheRootForTest(t)
	hash := fakeCorpusHash(20)
	dir := seedCorpus(t, root, hash, time.Now().Add(-1*time.Hour))
	if err := os.WriteFile(filepath.Join(root, gcStampFile), nil, 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	out := captureStdoutGC(t, func() {
		if err := runGCCommand(context.Background(), []string{"--all"}); err != nil {
			t.Fatalf("runGCCommand: %v", err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("--all must bypass throttle and evict: err=%v", err)
	}
	requireRow(t, out, hash, "evicted")
}

// TestGCCmd_Force_PrintsLiveTable confirms the manual `seek gc --force` path
// streams a banner + header + summary to stdout. Prevents regression to the
// pre-fix silent behavior where users saw nothing.
func TestGCCmd_Force_PrintsLiveTable(t *testing.T) {
	root := cacheRootForTest(t)
	hash := fakeCorpusHash(40)
	seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))

	out := captureStdoutGC(t, func() {
		if err := runGCCommand(context.Background(), []string{"--force"}); err != nil {
			t.Fatalf("runGCCommand: %v", err)
		}
	})

	if !strings.Contains(out, "seek gc: cache root") {
		t.Fatalf("live table missing banner; got:\n%s", out)
	}
	if !strings.Contains(out, "CORPUS") || !strings.Contains(out, "ACTION") {
		t.Fatalf("live table missing column header; got:\n%s", out)
	}
	if !strings.Contains(out, "evicted (") {
		t.Fatalf("live table missing summary line; got:\n%s", out)
	}
	requireRow(t, out, hash, "evicted")
}

// TestGCCmd_LiveTable_ShowsLockedAction confirms a corpus whose per-corpus
// .lock is held by an indexer/searcher shows up in the live table with
// ACTION=locked rather than being silently skipped.
func TestGCCmd_LiveTable_ShowsLockedAction(t *testing.T) {
	root := cacheRootForTest(t)
	hash := fakeCorpusHash(41)
	dir := seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))

	holder, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open corpus lock: %v", err)
	}
	defer func() { unlockFile(holder); _ = holder.Close() }()
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("hold corpus lock: %v", err)
	}

	out := captureStdoutGC(t, func() {
		if err := runGCCommand(context.Background(), []string{"--force"}); err != nil {
			t.Fatalf("runGCCommand: %v", err)
		}
	})

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("locked corpus must survive: %v", err)
	}
	requireRow(t, out, hash, "locked")
}

// TestGCCmd_LiveTable_NoCorpora confirms a fresh cache with zero corpora
// still prints the banner and a friendly "no corpora" line — not silence.
func TestGCCmd_LiveTable_NoCorpora(t *testing.T) {
	cacheRootForTest(t)
	out := captureStdoutGC(t, func() {
		if err := runGCCommand(context.Background(), []string{"--force"}); err != nil {
			t.Fatalf("runGCCommand: %v", err)
		}
	})
	if !strings.Contains(out, "seek gc: cache root") {
		t.Fatalf("expected banner even with empty cache; got:\n%s", out)
	}
	if !strings.Contains(out, "no corpora") {
		t.Fatalf("expected 'no corpora' line; got:\n%s", out)
	}
}

// TestGCCmd_OpportunisticPath_StaysSilent guards the contract that the
// post-search opportunistic GC (invoked by main) never writes to stdout —
// only the manual `seek gc` path does.
func TestGCCmd_OpportunisticPath_StaysSilent(t *testing.T) {
	root := cacheRootForTest(t)
	seedCorpus(t, root, fakeCorpusHash(42), time.Now().Add(-30*24*time.Hour))

	out := captureStdoutGC(t, func() {
		runOpportunisticGC(context.Background())
	})
	if out != "" {
		t.Fatalf("opportunistic GC must stay silent on stdout; got:\n%s", out)
	}
}

// TestGCCmd_LockContention_PrintsSkipMessage confirms the manual `seek gc
// --force` path surfaces gc.lock contention to the user (instead of
// returning silently — pre-fix UX bug where the user could not tell whether
// the command did anything).
func TestGCCmd_LockContention_PrintsSkipMessage(t *testing.T) {
	root := cacheRootForTest(t)
	seedCorpus(t, root, fakeCorpusHash(60), time.Now().Add(-30*24*time.Hour))

	lockFd, err := os.OpenFile(filepath.Join(root, gcLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open gc.lock: %v", err)
	}
	defer func() { unlockFile(lockFd); _ = lockFd.Close() }()
	if err := lockFileExclusive(lockFd); err != nil {
		t.Fatalf("hold gc.lock: %v", err)
	}

	out := captureStdoutGC(t, func() {
		if err := runGCCommand(context.Background(), []string{"--force"}); err != nil {
			t.Fatalf("runGCCommand: %v", err)
		}
	})

	if !strings.Contains(out, "another gc is already running") {
		t.Fatalf("expected contention message; got:\n%s", out)
	}
}

// TestGCCmd_CtxCanceled_SummaryUsesProcessedCount confirms a pre-canceled
// context breaks the loop before any eviction or sizing. Asserts both the
// observable side effect (all stale corpus dirs survive on disk — the
// strong behavior contract) and that the summary line does not claim work
// that did not happen (regression guard for the pre-fix bug where the
// summary used `len(entries)` instead of processedCount).
func TestGCCmd_CtxCanceled_SummaryUsesProcessedCount(t *testing.T) {
	root := cacheRootForTest(t)
	dirs := make([]string, 5)
	for i := 0; i < 5; i++ {
		dirs[i] = seedCorpus(t, root, fakeCorpusHash(70+i), time.Now().Add(-30*24*time.Hour))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := captureStdoutGC(t, func() {
		if err := runGCCommand(ctx, []string{"--force"}); err != nil {
			t.Fatalf("runGCCommand: %v", err)
		}
	})

	// Strong contract: stale corpora must survive when ctx pre-canceled.
	for i, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("ctx pre-cancel should have prevented eviction of corpus %d: %v", i, err)
		}
	}
	// Regression guard on the summary line wording.
	if strings.Contains(out, "5 corpora") {
		t.Fatalf("summary must not name unprocessed corpora; got:\n%s", out)
	}
	if strings.Contains(out, "0 corpora") {
		t.Fatalf("summary line should be omitted when processedCount==0; got:\n%s", out)
	}
}

func TestGC_ParseGCDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"14d", 14 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"500ms", 500 * time.Millisecond, false},
		{"0s", 0, false},
		// Plain "d" without leading number → ParseDuration("h") errors.
		{"d", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseGCDuration(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseGCDuration(%q) want error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGCDuration(%q) err=%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseGCDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestGC_EnvMaxAge_DaysSuffix(t *testing.T) {
	root := cacheRootForTest(t)
	t.Setenv(envGCMaxAge, "7d")

	stale := seedCorpus(t, root, fakeCorpusHash(21), time.Now().Add(-8*24*time.Hour))
	fresh := seedCorpus(t, root, fakeCorpusHash(22), time.Now().Add(-3*24*time.Hour))

	cfg := gcConfigFromEnv()
	if cfg.maxAge != 7*24*time.Hour {
		t.Fatalf("cfg.maxAge = %v, want 7d", cfg.maxAge)
	}
	runGC(context.Background(), gcOptions{maxAge: cfg.maxAge}, cfg.interval)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("8d corpus must evict under 7d TTL: err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("3d corpus must survive under 7d TTL: %v", err)
	}
}

func TestGCCmd_UnknownFlag_Errors(t *testing.T) {
	cacheRootForTest(t)
	if err := runGCCommand(context.Background(), []string{"--whoops"}); err == nil {
		t.Fatalf("expected error for unknown flag")
	}
}

func TestGC_IsCorpusHashName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{strings.Repeat("a", corpusHashHexLen), true},
		{strings.Repeat("0", corpusHashHexLen), true},
		{strings.Repeat("f", corpusHashHexLen), true},
		{strings.Repeat("g", corpusHashHexLen), false},
		{strings.Repeat("a", corpusHashHexLen-1), false},
		{strings.Repeat("a", corpusHashHexLen+1), false},
		{"", false},
		{".trash", false},
	}
	for _, c := range cases {
		if got := isCorpusHashName(c.in); got != c.want {
			t.Errorf("isCorpusHashName(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestRace_ConcurrentGCInvocations confirms exactly one GC acquires gc.lock
// while others skip cleanly.
func TestRace_ConcurrentGCInvocations(t *testing.T) {
	root := cacheRootForTest(t)
	const N = 20
	for i := 0; i < N; i++ {
		seedCorpus(t, root, fakeCorpusHash(100+i), time.Now().Add(-30*24*time.Hour))
	}

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true}, defaultGCInterval)
		}()
	}
	wg.Wait()

	// All seeded corpora should be gone (one of the GC runs evicted them;
	// the others skipped due to gc.lock contention).
	for i := 0; i < N; i++ {
		dir := filepath.Join(root, corporaDir, fakeCorpusHash(100+i))
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("corpus %d should be evicted: err=%v", i, err)
		}
	}
}

// TestRace_GCDuringIndexing — corpus .lock held by simulated indexer →
// eviction must skip even when stale.
func TestRace_GCDuringIndexing(t *testing.T) {
	root := cacheRootForTest(t)
	hash := fakeCorpusHash(200)
	dir := seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))

	// Simulated indexer holds LOCK_EX.
	lockPath := filepath.Join(dir, lockFile)
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { unlockFile(holder); _ = holder.Close() }()
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true}, defaultGCInterval)
	}()
	wg.Wait()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("corpus must survive while indexer holds lock: %v", err)
	}
}

// TestRace_TouchUsedConcurrent — concurrent touch from many goroutines.
func TestRace_TouchUsedConcurrent(t *testing.T) {
	root := cacheRootForTest(t)
	resetNFSCheck(t)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(300))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const N = 50
	var wg sync.WaitGroup
	var fails atomic.Int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			touchUsed(dir)
			if _, err := os.Stat(filepath.Join(dir, usedFile)); err != nil {
				fails.Add(1)
			}
		}()
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Fatalf("%d concurrent touchUsed observed missing .used", fails.Load())
	}
}

// TestGC_DisplayName_EmptyCorpus returns zero value when no shards exist.
func TestGC_DisplayName_EmptyCorpus(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "index"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	info := readCorpusDisplayInfo(dir)
	if info.source != "" || info.gone {
		t.Fatalf("empty corpus: want zero, got %+v", info)
	}
}

// TestGC_DisplayName_ReadsRepositorySourceFromShard exercises the real zoekt
// metadata read against a corpus built by the production indexer. Confirms
// the data is already on disk — no new schema required.
func TestGC_DisplayName_ReadsRepositorySourceFromShard(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)

	dir := initGitRepo(t, "app.go", "package main\n// disp_name_marker\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	if _, err := runSeekInPlannedGitCorpus(ctx, "disp_name_marker", paths, plan); err != nil {
		t.Fatalf("runSeekInPlannedGitCorpus: %v", err)
	}

	info := readCorpusDisplayInfo(plan.cacheDir)
	if info.source == "" {
		t.Fatalf("Repository.Source not recovered from shard")
	}
	// canonicalCorpusPath resolves symlinks (macOS /private/var → /var) so
	// substring match is more robust than equality.
	if !strings.Contains(info.source, filepath.Base(dir)) {
		t.Fatalf("Source %q does not contain repo basename %q", info.source, filepath.Base(dir))
	}
	if info.gone {
		t.Fatalf("real repo dir should not be flagged gone")
	}
}

// TestGC_PickDisplayShard covers the picker's filename-prefix semantics.
// Substring "uncommitted" inside a real repo name (e.g.
// github.com/foo/uncommitted-tool) must not cause skipping; only the exact
// zoekt-generated `uncommitted_v<format>.<seq>.zoekt` filename qualifies.
func TestGC_PickDisplayShard(t *testing.T) {
	cases := []struct {
		name   string
		shards []string
		want   string
	}{
		{
			name:   "single non-uncommitted shard",
			shards: []string{"/c/index/github.com%2Ffoo%2Fbar_v17.00000.zoekt"},
			want:   "/c/index/github.com%2Ffoo%2Fbar_v17.00000.zoekt",
		},
		{
			name:   "single uncommitted shard",
			shards: []string{"/c/index/uncommitted_v17.00000.zoekt"},
			want:   "/c/index/uncommitted_v17.00000.zoekt",
		},
		{
			name: "real repo + uncommitted — pick real",
			shards: []string{
				"/c/index/github.com%2Ffoo%2Fbar_v17.00000.zoekt",
				"/c/index/uncommitted_v17.00000.zoekt",
			},
			want: "/c/index/github.com%2Ffoo%2Fbar_v17.00000.zoekt",
		},
		{
			name: "real repo name contains 'uncommitted' substring — picker must NOT skip",
			shards: []string{
				"/c/index/github.com%2Ffoo%2Funcommitted-tool_v17.00000.zoekt",
				"/c/index/uncommitted_v17.00000.zoekt",
			},
			want: "/c/index/github.com%2Ffoo%2Funcommitted-tool_v17.00000.zoekt",
		},
		{
			name: "uncommitted first in slice order — picker still prefers real",
			shards: []string{
				"/c/index/uncommitted_v17.00000.zoekt",
				"/c/index/github.com%2Ffoo%2Fbar_v17.00000.zoekt",
			},
			want: "/c/index/github.com%2Ffoo%2Fbar_v17.00000.zoekt",
		},
		{
			// Distinguishes HasPrefix from a buggy substring match:
			// substring would skip BOTH (uncommitted_… AND
			// github.com%2F…%2Funcommitted-tool_…) and fall back to
			// shards[0] = uncommitted_…, displaying the wrong path.
			// HasPrefix correctly skips only the real uncommitted_
			// filename and returns the substring-containing real repo.
			name: "uncommitted first + real has 'uncommitted' substring — picker must return real",
			shards: []string{
				"/c/index/uncommitted_v17.00000.zoekt",
				"/c/index/github.com%2Ffoo%2Funcommitted-tool_v17.00000.zoekt",
			},
			want: "/c/index/github.com%2Ffoo%2Funcommitted-tool_v17.00000.zoekt",
		},
	}
	for _, c := range cases {
		got := pickDisplayShard(c.shards)
		if got != c.want {
			t.Errorf("%s: pickDisplayShard = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestGC_DisplayName_GoneFlag tests the [gone] detection: build a corpus,
// rm -rf the source, confirm gone=true. Source path still returned.
func TestGC_DisplayName_GoneFlag(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)

	dir := initGitRepo(t, "app.go", "package main\n// gone_marker\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	if _, err := runSeekInPlannedGitCorpus(ctx, "gone_marker", paths, plan); err != nil {
		t.Fatalf("runSeekInPlannedGitCorpus: %v", err)
	}

	// Capture source before removing.
	infoBefore := readCorpusDisplayInfo(plan.cacheDir)
	if infoBefore.source == "" {
		t.Fatalf("precondition: source must be readable")
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("rm repo: %v", err)
	}

	infoAfter := readCorpusDisplayInfo(plan.cacheDir)
	if !infoAfter.gone {
		t.Fatalf("gone flag should be set after repo dir removed: %+v", infoAfter)
	}
	if infoAfter.source != infoBefore.source {
		t.Fatalf("source path should remain stable after deletion: before=%q after=%q",
			infoBefore.source, infoAfter.source)
	}
}

// TestGC_FormatRoot covers the column-width truncation + special markers.
func TestGC_FormatRoot(t *testing.T) {
	cases := []struct {
		info  corpusDisplayInfo
		width int
		want  string
	}{
		{corpusDisplayInfo{}, 20, "[empty]"},
		{corpusDisplayInfo{source: "/tmp/x"}, 20, "/tmp/x"},
		{corpusDisplayInfo{source: "/tmp/x", gone: true}, 20, "[gone] /tmp/x"},
		{corpusDisplayInfo{source: "/very/long/path/to/some/repo/inside"}, 20, ".../some/repo/inside"},
		{corpusDisplayInfo{source: "/exactly/twenty/c"}, 17, "/exactly/twenty/c"},
		// width less than ellipsis falls back to hard truncate.
		{corpusDisplayInfo{source: "/abcdefghij"}, 2, "/a"},
		// width 3 = exactly ellipsis length, keep=0 → hard truncate path.
		{corpusDisplayInfo{source: "/abcdefghij"}, 3, "/ab"},
	}
	for i, c := range cases {
		got := formatRoot(c.info, c.width)
		if got != c.want {
			t.Errorf("case %d: formatRoot(%+v, %d) = %q, want %q", i, c.info, c.width, got, c.want)
		}
	}
}

// TestGC_FireOpportunisticGC_FastReturn confirms the orchestration doesn't
// wait the full timeout when the GC function returns quickly. If someone
// removes the gcDone select arm, the caller would block until the deadline
// fires every time.
func TestGC_FireOpportunisticGC_FastReturn(t *testing.T) {
	var called atomic.Int32
	runFn := func(_ context.Context) {
		called.Add(1)
	}
	start := time.Now()
	fireOpportunisticGC(runFn, 5*time.Second)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("fireOpportunisticGC should return as soon as runFn does; took %v", elapsed)
	}
	if called.Load() != 1 {
		t.Fatalf("runFn should run exactly once, got %d", called.Load())
	}
}

// TestGC_FireOpportunisticGC_TimeoutBoundsHang ensures a runaway GC cannot
// hold the caller indefinitely. If the select-on-ctx-Done arm is removed,
// this test hangs forever (caught by `go test -timeout`).
func TestGC_FireOpportunisticGC_TimeoutBoundsHang(t *testing.T) {
	released := make(chan struct{})
	defer close(released)
	runFn := func(_ context.Context) {
		<-released // simulate hang; runs in goroutine, freed at test end
	}
	start := time.Now()
	fireOpportunisticGC(runFn, 100*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed < 80*time.Millisecond {
		t.Fatalf("returned before timeout: %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("returned far past timeout (no bound on wait?): %v", elapsed)
	}
}

// TestGC_FireOpportunisticGC_PassesContextWithDeadline confirms the runFn
// receives a context whose deadline matches the requested timeout. Without
// this, the runFn would believe it has unbounded time and any inner
// ctx.Err() checks would never fire.
func TestGC_FireOpportunisticGC_PassesContextWithDeadline(t *testing.T) {
	var observedDeadlineSet atomic.Bool
	var observedRemaining atomic.Int64
	runFn := func(ctx context.Context) {
		dl, ok := ctx.Deadline()
		if ok {
			observedDeadlineSet.Store(true)
			observedRemaining.Store(int64(time.Until(dl)))
		}
	}
	fireOpportunisticGC(runFn, 200*time.Millisecond)
	if !observedDeadlineSet.Load() {
		t.Fatalf("runFn must receive a context with a deadline")
	}
	rem := time.Duration(observedRemaining.Load())
	if rem <= 0 || rem > 200*time.Millisecond {
		t.Fatalf("deadline remaining out of bounds: %v", rem)
	}
}

// TestGC_RunGC_CtxCanceledBreaksLoop verifies the eviction loop honors
// ctx.Err() and exits early. Without this short-circuit, an opportunistic
// GC firing right as the parent process times out would still try to evict
// every stale corpus, blowing past the 5s budget.
func TestGC_RunGC_CtxCanceledBreaksLoop(t *testing.T) {
	resetNFSCheck(t)
	root := cacheRootForTest(t)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	for i := 0; i < 50; i++ {
		seedCorpus(t, root, fakeCorpusHash(i), stale)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled

	start := time.Now()
	runGC(ctx, gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true}, defaultGCInterval)
	elapsed := time.Since(start)

	// Pre-canceled ctx → loop's first ctx.Err() check trips immediately.
	// 50 evictions would take >10ms; clean break is <5ms in practice.
	if elapsed > 50*time.Millisecond {
		t.Fatalf("canceled ctx did not short-circuit loop: took %v", elapsed)
	}

	// Confirm few-to-no corpora actually evicted (loop broke early).
	entries, err := enumerateCorpusDirs(filepath.Join(root, corporaDir))
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(entries) < 50 {
		t.Fatalf("ctx-canceled GC should leave most corpora intact; have %d / 50", len(entries))
	}
}

// TestGC_FireOpportunisticGC_Composition wires the production combination
// fireOpportunisticGC(runOpportunisticGC, timeout) end-to-end with a stale
// corpus, asserts eviction happens within the deadline. Without this, the
// orchestration tests pass with stub runFn while the runOpportunisticGC
// tests pass on direct calls — but if someone replaces the production
// runOpportunisticGC arg with a no-op inside fireOpportunisticGC(...),
// no other test catches it.
func TestGC_FireOpportunisticGC_Composition(t *testing.T) {
	resetNFSCheck(t)
	root := cacheRootForTest(t)
	staleDir := seedCorpus(t, root, fakeCorpusHash(0), time.Now().Add(-30*24*time.Hour))

	// Force GC to bypass throttle gate so a fresh stamp doesn't prevent
	// eviction; this mirrors how the manual `seek gc --force` path is
	// driven through the same composition.
	t.Setenv(envGCInterval, "0s")
	t.Setenv(envGCMaxAge, "14d")

	start := time.Now()
	fireOpportunisticGC(runOpportunisticGC, 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 4500*time.Millisecond {
		t.Fatalf("composition pinned to timeout — runOpportunisticGC may be a no-op: %v", elapsed)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should be evicted via composition: err=%v", err)
	}
}

// TestGC_FireOpportunisticGC_ZeroTimeout — pathological timeout. Context
// fires immediately; fireOpportunisticGC must not hang. runFn may not even
// observe a chance to run; it must not panic if it does.
func TestGC_FireOpportunisticGC_ZeroTimeout(t *testing.T) {
	start := time.Now()
	fireOpportunisticGC(func(ctx context.Context) {
		// If we get here, ctx is already canceled — verify deadline path
		// is observable without panicking.
		_ = ctx.Err()
	}, 0)
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("zero timeout should return immediately")
	}
}

// TestGC_FireOpportunisticGC_RunFnReturnsAfterTimeout — weird case. runFn
// outlives the deadline. fireOpportunisticGC must return at timeout WITHOUT
// blocking on the goroutine completing. The goroutine keeps running until
// runFn finishes; this is acceptable as long as it doesn't gate the caller.
func TestGC_FireOpportunisticGC_RunFnReturnsAfterTimeout(t *testing.T) {
	completed := make(chan struct{})
	fireOpportunisticGC(func(_ context.Context) {
		// Long sleep, well past the 50ms timeout below. The goroutine
		// will outlive fireOpportunisticGC's return.
		time.Sleep(300 * time.Millisecond)
		close(completed)
	}, 50*time.Millisecond)
	// fireOpportunisticGC returned. Did the goroutine survive?
	select {
	case <-completed:
		t.Fatalf("goroutine completed too fast (should still be sleeping)")
	case <-time.After(10 * time.Millisecond):
		// expected — goroutine still working post-return
	}
	// Wait for goroutine to actually finish so the test process is clean.
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatalf("goroutine never completed")
	}
}

// TestGC_FireOpportunisticGC_Sequential — successive calls must not bleed
// state between invocations (context, channel, goroutine leak).
func TestGC_FireOpportunisticGC_Sequential(t *testing.T) {
	var ran atomic.Int32
	runFn := func(_ context.Context) { ran.Add(1) }
	for i := 0; i < 5; i++ {
		fireOpportunisticGC(runFn, 200*time.Millisecond)
	}
	if ran.Load() != 5 {
		t.Fatalf("want 5 runs, got %d", ran.Load())
	}
}

// TestGC_FireOpportunisticGC_RunFnReadsCtxMidExecution — runFn observes
// cancellation via the passed ctx after the timeout fires. Mirrors what
// runGC's loop-internal ctx.Err() check relies on.
func TestGC_FireOpportunisticGC_RunFnReadsCtxMidExecution(t *testing.T) {
	observedCancel := make(chan struct{})
	runFn := func(ctx context.Context) {
		<-ctx.Done()
		close(observedCancel)
	}
	fireOpportunisticGC(runFn, 50*time.Millisecond)
	select {
	case <-observedCancel:
	case <-time.After(time.Second):
		t.Fatalf("runFn never observed ctx cancellation")
	}
}

// TestGC_RunGC_CtxCanceledDuringEnumerate is a stricter variant of the
// loop-break test: cancel happens BEFORE runGC is called, no corpora are
// seeded — confirms the loop is benign for zero entries plus a canceled
// context (no double-decrement, no panic on len(entries)==0).
func TestGC_RunGC_CtxCanceledEmptyCorpora(t *testing.T) {
	resetNFSCheck(t)
	root := cacheRootForTest(t)
	_ = root
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Must complete without panic; nothing to evict.
	runGC(ctx, gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true}, defaultGCInterval)
}

// TestGC_RunGC_CtxCancelMidLoop — context canceled WHILE the eviction
// loop is running. Verifies partial completion: some corpora evicted, the
// remainder spared. Differs from pre-canceled: covers the "racy" case
// closer to production semantics.
func TestGC_RunGC_CtxCancelMidLoop(t *testing.T) {
	resetNFSCheck(t)
	root := cacheRootForTest(t)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	const seeded = 100
	for i := 0; i < seeded; i++ {
		seedCorpus(t, root, fakeCorpusHash(i), stale)
	}
	corporaPath := filepath.Join(root, corporaDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel deterministically on the FIRST observed eviction instead of
	// racing a wall-clock timer. A fixed delay is consumed by GC setup on a
	// loaded runner (lock, drainTrash, enumerate, sort), so it can evict
	// nothing before firing. Polling real progress (corpora count dropping
	// below the seeded total) guarantees the partial-completion property at
	// any machine speed — the tight spin observes the drop within microseconds,
	// long before the ~100 filesystem renames a full sweep would take.
	done := make(chan struct{})
	go func() {
		runGC(ctx, gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true}, defaultGCInterval)
		close(done)
	}()
poll:
	for {
		select {
		case <-done:
			break poll
		default:
			if entries, _ := enumerateCorpusDirs(corporaPath); len(entries) < seeded {
				cancel()
				break poll
			}
		}
	}
	<-done

	entries, err := enumerateCorpusDirs(corporaPath)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	// All seeded corpora are stale, so an uncancelled run evicts everything;
	// cancelling on the first eviction must spare the remainder.
	if len(entries) == 0 {
		t.Fatalf("mid-loop cancel should leave some corpora intact; all evicted")
	}
	if len(entries) == seeded {
		t.Fatalf("mid-loop cancel should evict at least some; none evicted")
	}
}

// TestGC_Integration_DryRunShowsRealPath wires reportGCPlan against a real
// corpus and asserts the recovered Repository.Source appears in the output.
// Without this, a mutation that replaces formatRoot(readCorpusDisplayInfo(...))
// with truncateHash(e.name) (regressing to hash-only display) would silently
// pass every other test in the file.
func TestGC_Integration_DryRunShowsRealPath(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)

	repoDir := initGitRepo(t, "app.go", "package main\n// dry_run_real_path\n")
	t.Chdir(repoDir)
	ensureTestUserCache(t)

	if _, err := runSeekInRepo(t, repoDir, "dry_run_real_path"); err != nil {
		t.Fatalf("runSeekInRepo: %v", err)
	}

	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	go func() {
		_ = runGCCommand(context.Background(), []string{"--dry-run"})
		_ = w.Close()
	}()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	out := buf.String()

	// Repository.Source is canonicalized; substring on the basename is the
	// stable comparison surface across macOS /private/var → /var resolution.
	base := filepath.Base(repoDir)
	if !strings.Contains(out, base) {
		t.Fatalf("dry-run output missing repo basename %q. output:\n%s", base, out)
	}
	if strings.Contains(out, "[empty]") {
		t.Fatalf("real corpus should not display as [empty]:\n%s", out)
	}
}

// TestGC_IsOnNFS_LocalTempDir_ReturnsFalse exercises the real isOnNFS
// function. Without this, a regression that makes isOnNFS always return true
// (silently disabling GC everywhere) would be invisible to the rest of the
// test suite — every other NFS-related test pokes the cached nfsCached bool.
func TestGC_IsOnNFS_LocalTempDir_ReturnsFalse(t *testing.T) {
	tmp := t.TempDir()
	if isOnNFS(tmp) {
		t.Fatalf("isOnNFS(%q) should be false on a local tempdir", tmp)
	}
}

// TestGC_IsCorpusHashName_AcceptsProductionHash couples the enumerator's
// filter to the real corpus-ID generator. If newCorpusID ever changes its
// output shape (length, charset), enumerateCorpusDirs would silently filter
// every real corpus and GC would no-op. This test breaks loudly instead.
func TestGC_IsCorpusHashName_AcceptsProductionHash(t *testing.T) {
	id := string(newCorpusID("kind", "git", "root", "/x", "extra", "y"))
	if !isCorpusHashName(id) {
		t.Fatalf("isCorpusHashName must accept production newCorpusID output %q (len=%d)", id, len(id))
	}
}

// TestGC_Integration_PrepareAndSearchTouchesUsed wires the GC marker through
// the real production path. The unit-level TestGC_TouchUsed_UpdatesMtime
// calls touchUsed directly — it would still pass if someone deleted the
// touchUsed(plan.cacheDir) call sites in prepareAndSearchCorpus. This test
// fails in that scenario.
func TestGC_Integration_PrepareAndSearchTouchesUsed(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)

	dir := initGitRepo(t, "app.go", "package main\n// integration_used_marker\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	if _, err := runSeekInPlannedGitCorpus(ctx, "integration_used_marker", paths, plan); err != nil {
		t.Fatalf("runSeekInPlannedGitCorpus: %v", err)
	}

	usedPath := filepath.Join(plan.cacheDir, usedFile)
	st, err := os.Stat(usedPath)
	if err != nil {
		t.Fatalf(".used should exist after successful search via prepareAndSearchCorpus: %v", err)
	}
	if time.Since(st.ModTime()) > time.Minute {
		t.Fatalf(".used mtime stale (%v ago)", time.Since(st.ModTime()))
	}
}

// TestGC_Integration_RunFlowEvictsStale runs a real search through run(),
// then triggers GC through the same env-driven entry point used by main().
// Verifies: (1) the real search path touches .used so its corpus survives,
// (2) an alongside-seeded stale corpus is evicted by the env-configured GC.
// Couples the production code path end-to-end: search → touchUsed →
// runOpportunisticGC → evictCorpus.
func TestGC_Integration_RunFlowEvictsStale(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)

	repoDir := initGitRepo(t, "app.go", "package main\n// e2e_marker\n")
	t.Chdir(repoDir)
	ensureTestUserCache(t)

	// Force GC to run on every invocation, evict anything older than 1h.
	t.Setenv(envGCInterval, "0s")
	t.Setenv(envGCMaxAge, "1h")

	// Seed a stale fake corpus alongside the real one. Its hash shape
	// matches what newCorpusID produces.
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		t.Fatalf("seekUserCacheRoot: %v", err)
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatalf("mkdir cache root: %v", err)
	}
	staleHash := string(newCorpusID("stale", "fake"))
	staleDir := seedCorpus(t, cacheRoot, staleHash, time.Now().Add(-2*time.Hour))

	// Execute the real search flow.
	if _, err := runSeekInRepo(t, repoDir, "e2e_marker"); err != nil {
		t.Fatalf("runSeekInRepo: %v", err)
	}

	// Real corpus' .used must exist (proof that touchUsed wiring fired).
	plan, err := planCurrentGitCorpus(mustResolveGitPaths(t, repoDir))
	if err != nil {
		t.Fatalf("planCurrentGitCorpus: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plan.cacheDir, usedFile)); err != nil {
		t.Fatalf("real corpus .used missing after run(): %v", err)
	}

	// run() does not fire opportunistic GC inline — main() does. Test the
	// env-driven entry point that main wraps.
	runOpportunisticGC(context.Background())

	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("stale corpus must be evicted by runOpportunisticGC: err=%v", err)
	}
	if _, err := os.Stat(plan.cacheDir); err != nil {
		t.Fatalf("real corpus must survive (recently used): %v", err)
	}
}

func mustResolveGitPaths(t *testing.T, dir string) gitPaths {
	t.Helper()
	paths, err := resolveGitPaths(context.Background(), dir)
	if err != nil {
		t.Fatalf("resolveGitPaths: %v", err)
	}
	return paths
}

// TestRace_TrashDrainConcurrent — pre-seed 100 trash entries + 5 concurrent
// GC invocations; trash must be empty at the end.
func TestRace_TrashDrainConcurrent(t *testing.T) {
	root := cacheRootForTest(t)
	trashDir := filepath.Join(root, corporaDir, gcTrashDir)
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		t.Fatalf("mkdir trash: %v", err)
	}
	for i := 0; i < 100; i++ {
		entry := filepath.Join(trashDir, fakeCorpusHash(i)+"-x")
		if err := os.MkdirAll(entry, 0o755); err != nil {
			t.Fatalf("seed trash %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true}, defaultGCInterval)
		}()
	}
	wg.Wait()

	entries, err := os.ReadDir(trashDir)
	if err != nil {
		t.Fatalf("readdir trash: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("trash should be empty, got %d entries", len(entries))
	}
}
