package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fakeCorpusHash returns a hex string of corpusHashHexLen characters made
// from id: the same shape as newCorpusID, but cheap to make. It returns a
// plain string, so callers use it as a path part and convert it with
// corpusID(...) where they need a corpusID. Distinct ids give distinct hashes
// (zero-padded hex).
func fakeCorpusHash(id int) string {
	return fmt.Sprintf("%0*x", corpusHashHexLen, id)
}

// seededCorpusBytes is the ledger size of a seeded corpus.
const seededCorpusBytes = 1 << 20

// seedCorpus creates corpora/<hash>/ with an empty index directory, a .used
// marker whose modification time is usedAt, and a ledger entry of
// seededCorpusBytes. It returns the corpus directory.
func seedCorpus(tb testing.TB, cacheRoot, hash string, usedAt time.Time) string {
	tb.Helper()
	dir := filepath.Join(cacheRoot, corporaDir, hash)
	if err := os.MkdirAll(filepath.Join(dir, "index"), 0o755); err != nil {
		tb.Fatalf("mkdir corpus: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, corpusSizeFile), []byte(strconv.Itoa(seededCorpusBytes)), 0o644); err != nil {
		tb.Fatalf("write size: %v", err)
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

// cacheRootForTest gives the test a new user cache with the default GC
// interval and a cache budget of 1P (setTestUserCache). It creates the cache
// root and returns it. A test that needs a corpus over the budget sets a
// smaller budget.
func cacheRootForTest(tb testing.TB) string {
	tb.Helper()
	tb.Setenv(envGCInterval, "")
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

// resetNFSCheck clears the NFS check that touchUsed does once per process, now
// and when the test ends.
func resetNFSCheck(tb testing.TB) {
	tb.Helper()
	cacheRootOnNFS = sync.OnceValue(checkCacheRootOnNFS)
	tb.Cleanup(func() { cacheRootOnNFS = sync.OnceValue(checkCacheRootOnNFS) })
}

// captureStdoutGC is captureStdout for a function with no error.
func captureStdoutGC(tb testing.TB, fn func()) string {
	tb.Helper()
	out, _ := captureStdout(tb, func() error { fn(); return nil })
	return out
}

// requireRow asserts that the captured table output has a row for the corpus
// (found by its truncated hash) that ends with action. A substring match does
// not depend on the column widths.
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

// runGCCapture runs `seek gc <args>` and returns everything it wrote to
// stdout. It fails the test when the command returns an error.
func runGCCapture(tb testing.TB, args ...string) string {
	tb.Helper()
	return captureStdoutGC(tb, func() {
		if err := runGCCommand(context.Background(), args); err != nil {
			tb.Fatalf("gc %v: %v", args, err)
		}
	})
}

func TestGC_ThrottleGate_SkipsWhenStampFresh(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so the GC would evict the corpus unless the guard
	// under test holds it.
	t.Setenv(envCacheBudget, "1")
	dir := seedCorpus(t, root, fakeCorpusHash(1), time.Now().Add(-30*24*time.Hour))

	// A fresh stamp (now) must block GC.
	if err := os.WriteFile(filepath.Join(root, gcStampFile), nil, 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stale corpus removed despite fresh stamp: %v", err)
	}
}

func TestGC_ThrottleGate_RunsWhenStampMissing(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
	dir := seedCorpus(t, root, fakeCorpusHash(2), time.Now().Add(-30*24*time.Hour))

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should be evicted: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, gcStampFile)); err != nil {
		t.Fatalf("stamp should be written after run: %v", err)
	}
}

func TestGC_ThrottleGate_RunsWhenStampOld(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
	dir := seedCorpus(t, root, fakeCorpusHash(3), time.Now().Add(-30*24*time.Hour))

	stamp := filepath.Join(root, gcStampFile)
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatalf("chtimes stamp: %v", err)
	}

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should be evicted with old stamp: err=%v", err)
	}
}

func TestGC_GlobalLockContention_SkipsWhenHeld(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so the GC would evict the corpus unless the guard
	// under test holds it.
	t.Setenv(envCacheBudget, "1")
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

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stale corpus should survive when gc.lock contended: %v", err)
	}
}

// TestGC_EvictsLRUOverBudget puts the cache over its budget: GC evicts the
// least recently used corpus and keeps the newer one.
func TestGC_EvictsLRUOverBudget(t *testing.T) {
	root := cacheRootForTest(t)
	t.Setenv(envCacheBudget, strconv.Itoa(seededCorpusBytes*3/2))
	// Both are only days old, so the eviction comes from the budget, not
	// from age.
	old := seedCorpus(t, root, fakeCorpusHash(5), time.Now().Add(-2*24*time.Hour))
	recent := seedCorpus(t, root, fakeCorpusHash(6), time.Now().Add(-1*time.Hour))

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("least recently used corpus should be evicted: err=%v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent corpus should be kept: %v", err)
	}
}

// TestGC_KeepsOldCorpusWithinBudget checks that age alone never evicts a
// corpus; only the budget does.
func TestGC_KeepsOldCorpusWithinBudget(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(6), time.Now().Add(-90*24*time.Hour))

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("an old corpus within the budget should be kept: %v", err)
	}
}

func TestGC_CrashRecovery_DrainsTrash(t *testing.T) {
	root := cacheRootForTest(t)
	trashEntry := filepath.Join(root, corporaDir, gcTrashDir, "leftover-12345")
	if err := os.MkdirAll(filepath.Join(trashEntry, "index"), 0o755); err != nil {
		t.Fatalf("seed trash: %v", err)
	}
	// The entry holds a file, so only a recursive removal drains it.
	if err := os.WriteFile(filepath.Join(trashEntry, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed trash marker: %v", err)
	}

	runGC(context.Background(), gcOptions{})

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
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(8))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir corpus: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shard"), make([]byte, 64), 0o644); err != nil {
		t.Fatalf("write shard: %v", err)
	}
	// No .used marker. Set dir mtime to 15d ago.
	old := time.Now().Add(-15 * 24 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes dir: %v", err)
	}

	// The AGE cell of the plan shows the dir mtime: 15 days.
	rows := gcTableRows(runGCCapture(t, "--dry-run"))
	if len(rows) != 1 || strings.Fields(rows[0])[2] != "15d" {
		t.Fatalf("plan rows = %q, want one row with AGE 15d (dir mtime)", rows)
	}

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("corpus without .used should fall back to dir mtime and evict: err=%v", err)
	}
}

func TestGC_PerCorpusActive_SkipsLockedCorpus(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so the GC would evict the corpus unless the guard
	// under test holds it.
	t.Setenv(envCacheBudget, "1")
	dir := seedCorpus(t, root, fakeCorpusHash(9), time.Now().Add(-30*24*time.Hour))

	// Hold the corpus .lock exclusive, as a publish does.
	lockPath := filepath.Join(dir, lockFile)
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open corpus lock: %v", err)
	}
	defer func() { unlockFile(holder); _ = holder.Close() }()
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("hold corpus lock: %v", err)
	}

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("locked corpus should not be evicted: %v", err)
	}
}

func TestGC_TOCTOU_RefreshedUsedUnderLock_SkipsEvict(t *testing.T) {
	root := cacheRootForTest(t)
	hash := fakeCorpusHash(10)
	dir := seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))

	// A search can touch .used after enumerateCorpusDirs read the old time
	// and before evictCorpus takes the corpus lock. evictCorpus reads .used
	// again under the lock and must keep the corpus.
	//
	// An in-process race is hard to time. So the test calls evictCorpus
	// directly with a corpusDirEntry that holds the old time, while .used on
	// disk is fresh.
	entry := corpusDirEntry{name: corpusID(hash), path: dir, usedAt: time.Now().Add(-30 * 24 * time.Hour)}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, usedFile), now, now); err != nil {
		t.Fatalf("chtimes used: %v", err)
	}

	trashDir := filepath.Join(root, corporaDir, gcTrashDir)
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		t.Fatalf("mkdir trash: %v", err)
	}
	res := evictCorpus(entry, trashDir)
	if res.action != actionKept {
		t.Fatalf("expected actionKept after .used refresh under lock, got %s err=%v", res.String(), res.err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("corpus should remain after TOCTOU skip: %v", err)
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

func TestGC_TouchUsed_NFSGate_SkipsWhenCached(t *testing.T) {
	root := cacheRootForTest(t)
	resetNFSCheck(t)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(15))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pretend cache is on NFS.
	cacheRootOnNFS = func() bool { return true }

	touchUsed(dir)

	if _, err := os.Stat(filepath.Join(dir, usedFile)); !os.IsNotExist(err) {
		t.Fatalf(".used should not be created on NFS: err=%v", err)
	}
}

func TestGCCmd_DryRun_NoEvictions(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so the plan evicts the corpus and a live run would
	// remove it.
	t.Setenv(envCacheBudget, "1")
	hash := fakeCorpusHash(16)
	dir := seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))

	out := runGCCapture(t, "--dry-run")

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dry-run should not evict: %v", err)
	}
	requireRow(t, out, hash, "evicted")
	// Dry-run summary verb is "evictable" (the live path says "evicted").
	if !strings.Contains(out, "evictable (") {
		t.Fatalf("dry-run summary should say 'evictable'; got: %q", out)
	}
}

// TestGCCmd_DryRun_TableLayout compares the header and the row of a one-corpus
// dry-run table with hand-written lines. In both lines the columns start at
// byte 2 (CORPUS), 14 (ROOT), 68 (AGE), 76 (SIZE) and 84 (ACTION). The other
// table tests find rows by their hash and action only, so this test is the one
// that pins the header text and the column widths.
func TestGCCmd_DryRun_TableLayout(t *testing.T) {
	root := cacheRootForTest(t)
	seedCorpus(t, root, fakeCorpusHash(1), time.Now().Add(-49*time.Hour))

	out := runGCCapture(t, "--dry-run")

	header := "  CORPUS      ROOT" + strings.Repeat(" ", 50) + "AGE     SIZE    ACTION"
	// The seeded corpus has no shard, so ROOT shows [empty]. Its files are
	// the size ledger, which the size skips, and an empty .used marker.
	row := "  00000...01  [empty]" + strings.Repeat(" ", 45) + "  2d      0B      kept"
	if lines := strings.Split(out, "\n"); len(lines) < 3 || lines[1] != header || lines[2] != row {
		t.Fatalf("table lines\n%q\nwant header and row\n%q\n%q", lines, header, row)
	}
}

// TestGCCmd_DryRun_DoesNotCreateMissingCacheRoot checks the user-facing
// promise that a dry run does not change the filesystem.
func TestGCCmd_DryRun_DoesNotCreateMissingCacheRoot(t *testing.T) {
	t.Setenv(envGCInterval, "")
	root := filepath.Join(t.TempDir(), "missing-cache")
	t.Setenv("SEEK_CACHE_DIR", root)

	out := runGCCapture(t, "--dry-run")
	if !strings.Contains(out, "no corpora") {
		t.Fatalf("missing cache plan should report no corpora; got:\n%s", out)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("dry-run created missing cache root: %v", err)
	}
}

func TestGCCmd_Force_BypassesThrottle(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
	hash := fakeCorpusHash(17)
	dir := seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))
	if err := os.WriteFile(filepath.Join(root, gcStampFile), nil, 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	out := runGCCapture(t, "--force")

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

	out := runGCCapture(t, "--all", "--force")

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should evict under --all: err=%v", err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("fresh corpus should also evict under --all: err=%v", err)
	}
	requireRow(t, out, staleHash, "evicted")
	requireRow(t, out, freshHash, "evicted")
}

// TestGCCmd_All_BypassesThrottle checks that `seek gc --all` (without
// --force) evicts every corpus even when .last-gc is fresh.
func TestGCCmd_All_BypassesThrottle(t *testing.T) {
	root := cacheRootForTest(t)
	hash := fakeCorpusHash(20)
	dir := seedCorpus(t, root, hash, time.Now().Add(-1*time.Hour))
	if err := os.WriteFile(filepath.Join(root, gcStampFile), nil, 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	out := runGCCapture(t, "--all")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("--all must bypass throttle and evict: err=%v", err)
	}
	requireRow(t, out, hash, "evicted")
}

// TestGCCmd_Force_PrintsLiveTable checks that `seek gc --force` prints the
// banner, the column header, a row for the evicted corpus, and the summary
// line.
func TestGCCmd_Force_PrintsLiveTable(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
	hash := fakeCorpusHash(40)
	seedCorpus(t, root, hash, time.Now().Add(-30*24*time.Hour))

	out := runGCCapture(t, "--force")

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

// TestGCCmd_LiveTable_ShowsLockedAction holds the .lock of a corpus, as a
// publish does. The live table must show the corpus with the action "locked",
// and not leave out its row.
func TestGCCmd_LiveTable_ShowsLockedAction(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
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

	out := runGCCapture(t, "--force")

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("locked corpus must survive: %v", err)
	}
	requireRow(t, out, hash, "locked")
}

// TestGCCmd_LiveTable_NoCorpora runs `seek gc --force` on a cache with no
// corpus. The output has the banner and the "no corpora" line, and the run
// writes .last-gc. reportGCPlan returns after the banner. A live run that did
// the same would skip the stamp, and GC would run again at every search.
func TestGCCmd_LiveTable_NoCorpora(t *testing.T) {
	root := cacheRootForTest(t)
	out := runGCCapture(t, "--force")
	if !strings.Contains(out, "seek gc: cache root") {
		t.Fatalf("expected banner even with empty cache; got:\n%s", out)
	}
	if !strings.Contains(out, "no corpora") {
		t.Fatalf("expected 'no corpora' line; got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, gcStampFile)); err != nil {
		t.Fatalf("empty-cache live run must still stamp .last-gc: %v", err)
	}
}

// TestGCCmd_OpportunisticPath_StaysSilent checks that the GC that main runs
// after a search (runOpportunisticGC) writes nothing to stdout. Only
// `seek gc` prints.
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
// --force` path tells the user that another gc holds gc.lock, so a skipped
// run does not return in silence.
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

	out := runGCCapture(t, "--force")

	if !strings.Contains(out, "another gc is already running") {
		t.Fatalf("expected contention message; got:\n%s", out)
	}
}

// TestGCCmd_CtxCanceled_SummaryUsesRenderedCount runs `seek gc --force` with a
// context canceled before the call. The loop must stop before it evicts or
// measures a row, so every corpus stays on disk. The summary counts the
// printed rows (gcTableStats.rendered), not the corpora found. With no row,
// the summary line is not printed.
func TestGCCmd_CtxCanceled_SummaryUsesRenderedCount(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so an uncanceled run evicts every corpus.
	t.Setenv(envCacheBudget, "1")
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

	// No corpus is evicted.
	for i, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("ctx pre-cancel should have prevented eviction of corpus %d: %v", i, err)
		}
	}
	// The summary does not count corpora that the loop did not reach.
	if strings.Contains(out, "5 corpora") {
		t.Fatalf("summary must not name unprocessed corpora; got:\n%s", out)
	}
	if strings.Contains(out, "0 corpora") {
		t.Fatalf("summary line should be omitted when no row is rendered; got:\n%s", out)
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
		// Plain "d" has no number: parseGCDuration passes it to
		// time.ParseDuration unchanged, which errors.
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

// TestRace_ConcurrentGCInvocations runs 20 GCs at the same time on corpora
// over the budget. gc.lock lets one GC run at a time, and a GC that finds it
// held skips; at the end every corpus is evicted.
func TestRace_ConcurrentGCInvocations(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
	const N = 20
	for i := 0; i < N; i++ {
		seedCorpus(t, root, fakeCorpusHash(100+i), time.Now().Add(-30*24*time.Hour))
	}

	var wg sync.WaitGroup
	for range N {
		wg.Go(func() {
			runGC(context.Background(), gcOptions{skipThrottle: true})
		})
	}
	wg.Wait()

	// All seeded corpora are gone, whichever runs took gc.lock.
	for i := 0; i < N; i++ {
		dir := filepath.Join(root, corporaDir, fakeCorpusHash(100+i))
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("corpus %d should be evicted: err=%v", i, err)
		}
	}
}

// TestGC_SearchReadLock_SkipsCorpus holds the shared lock on the corpus .lock,
// as a search does while it reads shards (acquireReadLock). A GC over the
// budget must keep the corpus. TestGC_PerCorpusActive_SkipsLockedCorpus holds
// the exclusive lock of a publish, so it passes when GC takes a shared lock in
// place of the exclusive one. This test then fails.
func TestGC_SearchReadLock_SkipsCorpus(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so the GC would evict the corpus unless the guard
	// under test holds it.
	t.Setenv(envCacheBudget, "1")
	dir := seedCorpus(t, root, fakeCorpusHash(200), time.Now().Add(-30*24*time.Hour))

	reader, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { unlockFile(reader); _ = reader.Close() }()
	if err := acquireReadLock(t.Context(), reader); err != nil {
		t.Fatalf("hold the read lock: %v", err)
	}

	runGC(context.Background(), gcOptions{skipThrottle: true})

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("GC evicted a corpus that a search reads: %v", err)
	}
}

// TestRace_TouchUsedConcurrent runs touchUsed from 50 goroutines at the same
// time: after its touch, each goroutine finds .used.
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
	for range N {
		wg.Go(func() {
			touchUsed(dir)
			if _, err := os.Stat(filepath.Join(dir, usedFile)); err != nil {
				fails.Add(1)
			}
		})
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Fatalf("%d concurrent touchUsed observed missing .used", fails.Load())
	}
}

// TestGC_DisplayName_EmptyCorpus checks that readCorpusDisplayInfo returns
// the zero value for a corpus whose index directory holds no shard.
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

// TestGC_DisplayName_ReadsRepositorySourceFromShard builds a corpus with the
// production indexer and reads Repository.Source back from its shard.
func TestGC_DisplayName_ReadsRepositorySourceFromShard(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)

	dir := initGitRepo(t, "app.go", "package main\n// disp_name_marker\n")
	ctx := context.Background()
	_, plan := planGitTestCorpus(t, dir)

	if _, err := runSeekInPlannedGitCorpus(ctx, "disp_name_marker", plan); err != nil {
		t.Fatalf("runSeekInPlannedGitCorpus: %v", err)
	}

	info := readCorpusDisplayInfo(plan.cacheDir)
	if info.source == "" {
		t.Fatalf("Repository.Source not recovered from shard")
	}
	// canonicalCorpusPath resolves symlinks (on macOS, /var → /private/var),
	// so a match on the basename is more stable than equality.
	if !strings.Contains(info.source, filepath.Base(dir)) {
		t.Fatalf("Source %q does not contain repo basename %q", info.source, filepath.Base(dir))
	}
	if info.gone {
		t.Fatalf("real repo dir should not be flagged gone")
	}
}

// TestGC_PickDisplayShard checks that pickDisplayShard skips only a shard
// whose file name starts with `uncommitted_v` (isUncommittedShard). A
// repository name that contains "uncommitted", such as
// github.com/foo/uncommitted-tool, must not make it skip that shard.
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
			// A substring match skips both shards and falls back to
			// shards[0], the uncommitted shard. The prefix match skips
			// only the uncommitted shard.
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

// TestGC_DisplayName_GoneFlag builds a corpus and removes its source folder.
// readCorpusDisplayInfo must then set gone and keep the source path.
func TestGC_DisplayName_GoneFlag(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)

	dir := initGitRepo(t, "app.go", "package main\n// gone_marker\n")
	ctx := context.Background()
	_, plan := planGitTestCorpus(t, dir)
	if _, err := runSeekInPlannedGitCorpus(ctx, "gone_marker", plan); err != nil {
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

// TestGC_FormatRoot checks the ROOT cell: the [empty] and [gone] markers, and
// the cut of a long path to the column width (gcRootWidth, 52 bytes).
func TestGC_FormatRoot(t *testing.T) {
	cases := []struct {
		info corpusDisplayInfo
		want string
	}{
		{corpusDisplayInfo{}, "[empty]"},
		{corpusDisplayInfo{source: "/tmp/x"}, "/tmp/x"},
		{corpusDisplayInfo{source: "/tmp/x", gone: true}, "[gone] /tmp/x"},
		{corpusDisplayInfo{source: "/very/long/path/to/some/repo/inside/a/deeply/nested/workspace/folder"}, "...some/repo/inside/a/deeply/nested/workspace/folder"},
		{corpusDisplayInfo{source: "/exactly/fifty-two/bytes/of/path/for/the/root/column"}, "/exactly/fifty-two/bytes/of/path/for/the/root/column"},
	}
	for i, c := range cases {
		got := formatRoot(c.info)
		if got != c.want {
			t.Errorf("case %d: formatRoot(%+v) = %q, want %q", i, c.info, got, c.want)
		}
	}
}

// TestGC_FireOpportunisticGC_FastReturn checks that fireOpportunisticGC
// returns when runFn returns, not at the timeout. Without the done case of the
// select, each call would wait until the deadline.
func TestGC_FireOpportunisticGC_FastReturn(t *testing.T) {
	var called atomic.Int32
	runFn := func(_ context.Context) {
		called.Add(1)
	}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		fireOpportunisticGC(runFn, 5*time.Second)
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("fireOpportunisticGC should return as soon as runFn does; took %v of fake time", elapsed)
		}
	})
	if called.Load() != 1 {
		t.Fatalf("runFn should run exactly once, got %d", called.Load())
	}
}

// TestGC_FireOpportunisticGC_TimeoutBoundsHang gives a runFn that does not
// return: fireOpportunisticGC must return at the timeout. Without the ctx.Done
// case of the select, every goroutine of the synctest bubble blocks, and
// synctest.Test fails at once with a deadlock.
func TestGC_FireOpportunisticGC_TimeoutBoundsHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		released := make(chan struct{})
		defer close(released)
		runFn := func(_ context.Context) {
			<-released // The deferred close frees it when the test ends.
		}
		start := time.Now()
		fireOpportunisticGC(runFn, 100*time.Millisecond)
		if elapsed := time.Since(start); elapsed != 100*time.Millisecond {
			t.Fatalf("returned after %v of fake time, want the 100ms timeout", elapsed)
		}
	})
}

// TestGC_FireOpportunisticGC_PassesContextWithDeadline checks that runFn gets
// a context whose deadline is the timeout. Without a deadline, the ctx.Err()
// checks in runGC never stop a GC that runs too long.
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
	synctest.Test(t, func(t *testing.T) { fireOpportunisticGC(runFn, 200*time.Millisecond) })
	if !observedDeadlineSet.Load() {
		t.Fatalf("runFn must receive a context with a deadline")
	}
	rem := time.Duration(observedRemaining.Load())
	if rem != 200*time.Millisecond {
		t.Fatalf("deadline in %v of fake time, want 200ms", rem)
	}
}

// TestGC_RunGC_CtxCanceledBreaksLoop checks that the eviction loop stops when
// its context ends. Without this check, an opportunistic GC whose context ends
// would still evict every planned corpus and run past its 5 s limit
// (gcRunTimeout).
func TestGC_RunGC_CtxCanceledBreaksLoop(t *testing.T) {
	resetNFSCheck(t)
	root := cacheRootForTest(t)
	// Over the budget, so the GC would evict the corpus unless the guard
	// under test holds it.
	t.Setenv(envCacheBudget, "1")
	stale := time.Now().Add(-30 * 24 * time.Hour)
	for i := 0; i < 50; i++ {
		seedCorpus(t, root, fakeCorpusHash(i), stale)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled

	runGC(ctx, gcOptions{skipThrottle: true})

	// No corpus is evicted: the loop checks ctx before the first entry.
	entries, err := enumerateCorpusDirs(filepath.Join(root, corporaDir))
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(entries) != 50 {
		t.Fatalf("a canceled GC evicted %d of 50 corpora, want none", 50-len(entries))
	}
}

// TestGC_FireOpportunisticGC_Composition runs
// fireOpportunisticGC(runOpportunisticGC, timeout), the pair that main calls,
// on a corpus over the budget, and asserts the eviction ends before the
// timeout. The other fireOpportunisticGC tests use a stub runFn, and the
// runOpportunisticGC tests call it directly; only this test runs the two
// together.
func TestGC_FireOpportunisticGC_Composition(t *testing.T) {
	resetNFSCheck(t)
	root := cacheRootForTest(t)
	staleDir := seedCorpus(t, root, fakeCorpusHash(0), time.Now().Add(-30*24*time.Hour))

	// An interval of 0s lets the throttle gate pass whatever the stamp, and a
	// budget of 1 byte puts the corpus over the budget.
	t.Setenv(envGCInterval, "0s")
	t.Setenv(envCacheBudget, "1")

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		fireOpportunisticGC(runOpportunisticGC, 5*time.Second)
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("composition took %v of fake time, want none", elapsed)
		}
	})
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("stale corpus should be evicted via composition: err=%v", err)
	}
}

// TestGC_FireOpportunisticGC_ZeroTimeout gives a timeout of 0: the context
// ends at once, and fireOpportunisticGC must return with no fake time passed.
// runFn can run after the call returns; the bubble waits for it.
func TestGC_FireOpportunisticGC_ZeroTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		fireOpportunisticGC(func(ctx context.Context) {
			_ = ctx.Err()
		}, 0)
		if time.Since(start) != 0 {
			t.Fatalf("zero timeout should return immediately")
		}
	})
}

// TestGC_FireOpportunisticGC_RunFnReturnsAfterTimeout gives a runFn that runs
// past the deadline. fireOpportunisticGC must return at the timeout and not
// wait for the goroutine. The goroutine runs until runFn ends; that is correct
// while the caller does not wait for it. The test runs in a synctest bubble:
// a call that waits for runFn fails at once as a deadlock.
func TestGC_FireOpportunisticGC_RunFnReturnsAfterTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		completed := make(chan struct{})
		start := time.Now()
		fireOpportunisticGC(func(_ context.Context) {
			// runFn ends only when the test releases it, after the call
			// returns.
			<-release
			close(completed)
		}, 50*time.Millisecond)
		if elapsed := time.Since(start); elapsed != 50*time.Millisecond {
			t.Fatalf("returned after %v of fake time, want the 50ms timeout", elapsed)
		}
		close(release)
		<-completed
	})
}

// TestGC_FireOpportunisticGC_Sequential calls fireOpportunisticGC five times
// in a row: no state of one call (context, channel, goroutine) stops the next,
// so runFn runs five times. The synctest bubble moves time only when every
// goroutine blocks, so a slow scheduler cannot fire the timeout first.
func TestGC_FireOpportunisticGC_Sequential(t *testing.T) {
	var ran atomic.Int32
	runFn := func(_ context.Context) { ran.Add(1) }
	synctest.Test(t, func(t *testing.T) {
		for i := 0; i < 5; i++ {
			start := time.Now()
			fireOpportunisticGC(runFn, 200*time.Millisecond)
			if elapsed := time.Since(start); elapsed != 0 {
				t.Fatalf("call %d took %v of fake time, want none", i+1, elapsed)
			}
		}
	})
	if ran.Load() != 5 {
		t.Fatalf("want 5 runs, got %d", ran.Load())
	}
}

// TestGC_FireOpportunisticGC_RunFnReadsCtxMidExecution checks that runFn sees
// its context end when the timeout fires, as the ctx.Err() check in the
// eviction loop of runGC needs. The test runs in a synctest bubble: a runFn
// whose context does not end fails at once as a deadlock.
func TestGC_FireOpportunisticGC_RunFnReadsCtxMidExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		observedCancel := make(chan struct{})
		runFn := func(ctx context.Context) {
			<-ctx.Done()
			close(observedCancel)
		}
		fireOpportunisticGC(runFn, 50*time.Millisecond)
		<-observedCancel
	})
}

// TestGC_RunGC_CtxCanceledEmptyCorpora runs runGC with a context canceled
// before the call on a cache with no corpus: runGC must return without a
// panic.
func TestGC_RunGC_CtxCanceledEmptyCorpora(t *testing.T) {
	resetNFSCheck(t)
	cacheRootForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runGC(ctx, gcOptions{skipThrottle: true})
}

// TestGC_RunGC_CtxCancelMidLoop cancels the context while the eviction loop
// runs: some corpora are evicted, and the rest stay. Unlike a context that
// ends before the call, this is the order that a real timeout gives.
func TestGC_RunGC_CtxCancelMidLoop(t *testing.T) {
	resetNFSCheck(t)
	root := cacheRootForTest(t)
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
	stale := time.Now().Add(-30 * 24 * time.Hour)
	const seeded = 100
	for i := 0; i < seeded; i++ {
		seedCorpus(t, root, fakeCorpusHash(i), stale)
	}
	corporaPath := filepath.Join(root, corporaDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel on the first eviction that the poll sees, not on a timer. GC
	// setup on a loaded runner (lock, drainTrash, enumerate) can use up a
	// fixed delay, so a timer can fire before any eviction. The poll reads
	// the corpora count again and again. So it sees the count drop below the
	// seeded total before the sweep of 100 evictions ends.
	done := make(chan struct{})
	go func() {
		runGC(ctx, gcOptions{skipThrottle: true})
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
	// All seeded corpora are over the budget, so a run that is not canceled
	// evicts all of them. A cancel at the first eviction must keep the rest.
	if len(entries) == 0 {
		t.Fatalf("mid-loop cancel should leave some corpora intact; all evicted")
	}
	if len(entries) == seeded {
		t.Fatalf("mid-loop cancel should evict at least some; none evicted")
	}
}

// TestGC_Integration_DryRunShowsRealPath builds a real corpus with a search,
// then runs `seek gc --dry-run`. The ROOT column must show the repository path
// from the shard, not [empty]. The display-name tests above call
// readCorpusDisplayInfo directly; only this test checks that the table prints
// it.
func TestGC_Integration_DryRunShowsRealPath(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)

	repoDir := initGitRepo(t, "app.go", "package main\n// dry_run_real_path\n")
	t.Chdir(repoDir)
	ensureTestUserCache(t)

	if _, err := runSeekInRepo(t, repoDir, "dry_run_real_path"); err != nil {
		t.Fatalf("runSeekInRepo: %v", err)
	}

	out := runGCCapture(t, "--dry-run")

	// Repository.Source is canonicalized (on macOS, /var → /private/var), so
	// the test matches the basename, not the full path.
	base := filepath.Base(repoDir)
	if !strings.Contains(out, base) {
		t.Fatalf("dry-run output missing repo basename %q. output:\n%s", base, out)
	}
	if strings.Contains(out, "[empty]") {
		t.Fatalf("real corpus should not display as [empty]:\n%s", out)
	}
}

// TestGC_IsOnNFS_LocalTempDir_ReturnsFalse checks that isOnNFS reports a local
// temporary folder as local. A true result turns off GC and the .used marker;
// the eviction tests then fail too, but this test names the cause.
func TestGC_IsOnNFS_LocalTempDir_ReturnsFalse(t *testing.T) {
	tmp := t.TempDir()
	if isOnNFS(tmp) {
		t.Fatalf("isOnNFS(%q) should be false on a local tempdir", tmp)
	}
}

// TestGC_IsCorpusHashName_AcceptsProductionHash checks that isCorpusHashName
// accepts an id from newCorpusID. If the two disagree on the length or the
// characters, enumerateCorpusDirs skips every real corpus and GC removes
// nothing.
func TestGC_IsCorpusHashName_AcceptsProductionHash(t *testing.T) {
	id := string(newCorpusID("kind", "git", "root", "/x", "extra", "y"))
	if !isCorpusHashName(id) {
		t.Fatalf("isCorpusHashName must accept production newCorpusID output %q (len=%d)", id, len(id))
	}
}

// TestGC_Integration_PrepareAndSearchTouchesUsed checks that a warm search
// marks its corpus as used, on both routes of the root command. The first
// search builds the text index and the vectors, and the build also touches
// .used. So the test ages .used before each warm search. A warm search builds
// nothing, so only the touch of its route can make .used fresh: retrieveNode
// for a query that the model reads, strictNode for --lexical-only.
func TestGC_Integration_PrepareAndSearchTouchesUsed(t *testing.T) {
	requireTools(t)
	resetNFSCheck(t)
	setTestUserCache(t)

	dir := initGitRepo(t, "app.go", "package main\n// integration_used_marker\n")
	plans, err := planCorpora(t.Context(), []string{dir})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	usedPath := filepath.Join(plans[0].cacheDir, usedFile)
	if out, _, err := stubSearch(t, payloadFar, "integration_used_marker", dir); err != nil || !strings.Contains(out, "app.go") {
		t.Fatalf("cold search: err=%v output:\n%s", err, out)
	}
	for _, route := range []struct {
		name   string
		search func() (string, error)
	}{
		{"model", func() (string, error) {
			out, _, err := stubSearch(t, payloadFar, "integration_used_marker", dir)
			return out, err
		}},
		{"lexical-only", func() (string, error) { return seek(t, "--lexical-only", "integration_used_marker", dir) }},
	} {
		old := time.Now().Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(usedPath, old, old); err != nil {
			t.Fatalf("age .used: %v", err)
		}
		if out, err := route.search(); err != nil || !strings.Contains(out, "app.go") {
			t.Fatalf("%s warm search: err=%v output:\n%s", route.name, err, out)
		}
		st, err := os.Stat(usedPath)
		if err != nil {
			t.Fatalf("%s warm search: .used is missing: %v", route.name, err)
		}
		if time.Since(st.ModTime()) > time.Minute {
			t.Fatalf("the %s warm search did not touch .used (%v ago)", route.name, time.Since(st.ModTime()))
		}
	}
}

// TestGC_Integration_RunFlowEvictsStale runs searches as CLI processes, so
// main runs the automatic GC after each search, as for a user. A first search
// builds the corpus with a 1P budget. Then the cache gets a stale 1 MiB
// corpus. A warm search reads a GC interval of 0s and a 512K budget from its
// environment. It publishes nothing, so its budget check (enforceCacheBudget)
// evicts nothing. Only the GC of main can evict the stale corpus to fit the
// budget, and it must keep the corpus that the search used. main waits for
// the GC (fireOpportunisticGC), so the cache is final when the process exits.
func TestGC_Integration_RunFlowEvictsStale(t *testing.T) {
	requireTools(t)
	repoDir := initGitRepo(t, "app.go", "package main\n// e2e_marker\n")
	cacheRoot := t.TempDir()
	search := func(env ...string) {
		t.Helper()
		result := runCLIProcessWithCache(t, cacheRoot, repoDir, []string{"--lexical-only", "e2e_marker"}, env)
		if result.code != 0 || !strings.Contains(result.stdout, "e2e_marker") {
			t.Fatalf("search: code=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
		}
	}
	search(envCacheBudget + "=1P")
	staleDir := seedCorpus(t, cacheRoot, string(newCorpusID("stale", "fake")), time.Now().Add(-2*time.Hour))
	// An interval of 0s lets GC run at every call. The budget holds the
	// searched corpus but not the 1 MiB stale one.
	search(envGCInterval+"=0s", envCacheBudget+"=512K")
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("the GC after the search kept the stale corpus: err=%v", err)
	}
	// The stale corpus is gone, so the one .used marker left is the marker
	// that the search wrote in its own corpus.
	used := cachePathsMatching(t, cacheRoot, func(name string) bool { return name == usedFile })
	if len(used) != 1 || filepath.Dir(filepath.Dir(used[0])) != corporaDir {
		t.Fatalf("corpus .used markers %v, want only the marker of the searched corpus", used)
	}
}

// TestRace_TrashDrainConcurrent puts 100 entries in the trash and runs 5 GCs at
// the same time: the trash must be empty at the end.
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
	for range 5 {
		wg.Go(func() {
			runGC(context.Background(), gcOptions{skipThrottle: true})
		})
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

func TestSweepAgedProviderArtifacts(t *testing.T) {
	cacheRoot := t.TempDir()
	base := filepath.Join(cacheRoot, "reranker", "coreml", "mlprogram-static")
	aged := filepath.Join(base, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	fresh := filepath.Join(base, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	for _, dir := range []string{aged, fresh} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "provider-verdict"), []byte("trusted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}
	sweepAgedProviderArtifacts(t.Context(), nil, cacheRoot, time.Now().Add(-14*24*time.Hour))
	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Fatalf("aged compiled model survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh compiled model was removed: %v", err)
	}
	// A model in daily use carries a fresh access marker and survives, even
	// though nothing refreshes the directory's own modification time.
	inUse := filepath.Join(base, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if err := os.MkdirAll(inUse, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inUse, providerArtifactUsedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(inUse, old, old); err != nil {
		t.Fatal(err)
	}
	sweepAgedProviderArtifacts(t.Context(), nil, cacheRoot, time.Now().Add(-14*24*time.Hour))
	if _, err := os.Stat(inUse); err != nil {
		t.Fatalf("a model in daily use was removed: %v", err)
	}
	// An aged marker does not save it.
	if err := os.Chtimes(filepath.Join(inUse, providerArtifactUsedFile), old, old); err != nil {
		t.Fatal(err)
	}
	sweepAgedProviderArtifacts(t.Context(), nil, cacheRoot, time.Now().Add(-14*24*time.Hour))
	if _, err := os.Stat(inUse); !os.IsNotExist(err) {
		t.Fatalf("an unused model with an aged marker survived: %v", err)
	}

	// A directory a previous sweep marked and did not finish is removed
	// whatever its age, so a truncated removal cannot hide behind a refreshed
	// timestamp.
	leftover := filepath.Join(base, providerArtifactDiscardPrefix+"cccccccccccccccccccccccccccccccc")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	sweepAgedProviderArtifacts(t.Context(), nil, cacheRoot, time.Now().Add(-14*24*time.Hour))
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("an unfinished removal survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("the fresh compiled model was removed: %v", err)
	}

	// A cancelled collection stops rather than half-removing more directories.
	aged2 := filepath.Join(base, "dddddddddddddddddddddddddddddddd")
	if err := os.MkdirAll(aged2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(aged2, old, old); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	sweepAgedProviderArtifacts(cancelled, nil, cacheRoot, time.Now().Add(-14*24*time.Hour))
	if _, err := os.Stat(aged2); err != nil {
		t.Fatalf("a cancelled collection still removed a directory: %v", err)
	}

	// An absent tree is not an error.
	sweepAgedProviderArtifacts(t.Context(), nil, t.TempDir(), time.Now())
}

// TestGCDryRunReportsProviderArtifacts runs seek gc --dry-run. The plan lists
// the aged compiled model with its count and not the fresh one, and it removes
// nothing. A cache with no compiled model due prints no compiled-model section.
func TestGCDryRunReportsProviderArtifacts(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cacheRoot)
	aged := plantProviderArtifact(t, cacheRoot, "ffffffffffffffffffffffffffffffff", 30*24*time.Hour)
	fresh := plantProviderArtifact(t, cacheRoot, "11111111111111111111111111111111", 0)
	dryRun := func() string {
		t.Helper()
		return captureStdoutGC(t, func() {
			if err := runGCCommandCmd(t.Context(), gcCmdOptions{dryRun: true, sort: "name"}); err != nil {
				t.Fatal(err)
			}
		})
	}
	plan := dryRun()
	if !strings.Contains(plan, "compiled model files to remove: 1 (") || !strings.Contains(plan, aged) {
		t.Fatalf("the plan omits a compiled model the sweep would remove:\n%s", plan)
	}
	if strings.Contains(plan, fresh) {
		t.Fatalf("the plan lists a compiled model the sweep would keep:\n%s", plan)
	}
	// The plan must not remove anything.
	if _, err := os.Stat(aged); err != nil {
		t.Fatalf("the plan removed a compiled model: %v", err)
	}
	// A cache with nothing due prints no compiled-model section.
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	if empty := dryRun(); strings.Contains(empty, "compiled model files") {
		t.Fatalf("an empty plan printed a compiled-model section:\n%s", empty)
	}
}

func TestRunGCCollectsCompiledModelsAndRuntimes(t *testing.T) {
	// Other tests check the helpers directly. This test proves that runGC
	// calls both sweeps with the 14-day bound. An item of 15 days goes and one
	// of 13 days stays, so a call site that is removed or has another bound
	// fails.
	cacheRoot := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cacheRoot)
	aged := plantProviderArtifact(t, cacheRoot, "22222222222222222222222222222222", 15*24*time.Hour)
	recentModel := plantProviderArtifact(t, cacheRoot, "23232323232323232323232323232323", 13*24*time.Hour)
	runtime := plantRuntime(t, cacheRoot, "0.0.1", 15*24*time.Hour)
	recent := plantRuntime(t, cacheRoot, "0.0.2", 13*24*time.Hour)
	var out bytes.Buffer
	runGC(t.Context(), gcOptions{
		skipThrottle: true,
		writer:       &out,
	})
	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Fatalf("runGC did not collect an aged compiled model: %v", err)
	}
	if !strings.Contains(out.String(), "compiled model files removed: 1 (") {
		t.Fatalf("the collection did not report the reclaim:\n%s", out.String())
	}
	if _, err := os.Stat(runtime); !os.IsNotExist(err) {
		t.Fatalf("runGC did not remove another version's old runtime: %v", err)
	}
	if !strings.Contains(out.String(), "retired runtimes removed: 1 (") {
		t.Fatalf("the collection did not report the removed runtime:\n%s", out.String())
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("runGC removed a recent runtime: %v", err)
	}
	if _, err := os.Stat(recentModel); err != nil {
		t.Fatalf("runGC removed a recent compiled model: %v", err)
	}
}

func TestSweepSkipsAModelAnotherSearchJustUsed(t *testing.T) {
	// A search refreshes the access marker of a model whose directory is
	// old. The sweep must keep the model: the marker, not the directory
	// time, gives its age.
	cacheRoot := t.TempDir()
	dir := plantProviderArtifact(t, cacheRoot, "33333333333333333333333333333333", 30*24*time.Hour)
	// The directory is due by its own timestamp, but a fresh marker
	// overrides it.
	if err := os.WriteFile(filepath.Join(dir, providerArtifactUsedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sweepAgedProviderArtifacts(t.Context(), nil, cacheRoot, time.Now().Add(-14*24*time.Hour))
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the sweep removed a model a search had just used: %v", err)
	}
}

func TestGCDryRunListsCompiledModelsAndRuntimes(t *testing.T) {
	// Other tests check the plans directly. This test proves that the dry-run
	// command calls both with the 14-day bound and removes nothing. It lists
	// an item of 15 days, and not one of 13 days.
	cacheRoot := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cacheRoot)
	aged := plantProviderArtifact(t, cacheRoot, "44444444444444444444444444444444", 15*24*time.Hour)
	recentModel := plantProviderArtifact(t, cacheRoot, "45454545454545454545454545454545", 13*24*time.Hour)
	runtime := plantRuntime(t, cacheRoot, "0.0.1", 15*24*time.Hour)
	recent := plantRuntime(t, cacheRoot, "0.0.2", 13*24*time.Hour)
	stdout := captureStdoutGC(t, func() {
		if err := runGCCommandCmd(t.Context(), gcCmdOptions{dryRun: true, sort: "name"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, aged) {
		t.Fatalf("the dry run did not list a compiled model it would remove:\n%s", stdout)
	}
	if _, err := os.Stat(aged); err != nil {
		t.Fatalf("the dry run removed a compiled model: %v", err)
	}
	if !strings.Contains(stdout, runtime) {
		t.Fatalf("the dry run did not list a runtime it would remove:\n%s", stdout)
	}
	if _, err := os.Stat(runtime); err != nil {
		t.Fatalf("the dry run removed a runtime: %v", err)
	}
	if strings.Contains(stdout, recent) || strings.Contains(stdout, recentModel) {
		t.Fatalf("the dry run lists a recent runtime or compiled model:\n%s", stdout)
	}
}

// TestGCAllRemovesCompiledModelsAndOtherVersionsRuntimes plans and runs gc
// --all: a compiled model and a runtime of another version, both one day old,
// go too.
func TestGCAllRemovesCompiledModelsAndOtherVersionsRuntimes(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cacheRoot)
	recent := plantRuntime(t, cacheRoot, "0.0.2", 24*time.Hour)
	model := plantProviderArtifact(t, cacheRoot, "56565656565656565656565656565656", 24*time.Hour)
	stdout := captureStdoutGC(t, func() {
		if err := runGCCommandCmd(t.Context(), gcCmdOptions{dryRun: true, all: true, sort: "name"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, recent) || !strings.Contains(stdout, model) {
		t.Fatalf("the --all dry run does not list a recent runtime and a recent compiled model:\n%s", stdout)
	}
	runGC(t.Context(), gcOptions{skipThrottle: true, evictAll: true})
	if _, err := os.Stat(recent); !os.IsNotExist(err) {
		t.Fatalf("gc --all kept a recent runtime of another version: %v", err)
	}
	if _, err := os.Stat(model); !os.IsNotExist(err) {
		t.Fatalf("gc --all kept a recent compiled model: %v", err)
	}
}

// TestRetireRuntimesRenamesBeforeTheRemoval makes the removal fail part way.
// The runtime path must be gone, and the renamed leftover waits for the next
// sweep.
func TestRetireRuntimesRenamesBeforeTheRemoval(t *testing.T) {
	cacheRoot := t.TempDir()
	runtime := plantRuntime(t, cacheRoot, "0.0.1", 30*24*time.Hour)
	locked := filepath.Join(runtime, "locked")
	writeFixtureFile(t, filepath.Join(locked, "lib"), "x")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	// The removal fails part way only for a user who cannot change a mode-0500
	// folder. Root can, so the test fails for root with the cause.
	if err := os.WriteFile(filepath.Join(locked, "probe"), nil, 0o644); err == nil {
		t.Fatalf("the test user adds a file to the mode-0500 folder %s, so the removal cannot fail part way", locked)
	}
	leftover := filepath.Join(filepath.Dir(runtime), providerArtifactDiscardPrefix+filepath.Base(runtime))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(leftover, "locked"), 0o700) })
	stamp := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(runtime, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	sweepRetiredRuntimes(t.Context(), nil, cacheRoot, time.Now().Add(-providerArtifactMaxAge))
	if _, err := os.Stat(runtime); !os.IsNotExist(err) {
		t.Fatalf("a half-removed runtime stays at its path: %v", err)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("no renamed leftover: %v", err)
	}
}

// TestRetireRuntimesSkipsARuntimeUsedAfterTheList lists an old runtime, then
// uses it before the removal step. The step reads the age again and keeps it.
func TestRetireRuntimesSkipsARuntimeUsedAfterTheList(t *testing.T) {
	cacheRoot := t.TempDir()
	runtime := plantRuntime(t, cacheRoot, "0.0.1", 30*24*time.Hour)
	before := time.Now().Add(-providerArtifactMaxAge)
	due := retiredRuntimes(cacheRoot, before)
	if !slices.Contains(due, runtime) {
		t.Fatalf("due=%v, want %s", due, runtime)
	}
	touchRuntimeUse(runtime)
	var report bytes.Buffer
	retireRuntimes(t.Context(), &report, cacheRoot, before, due)
	if _, err := os.Stat(runtime); err != nil {
		t.Fatalf("the sweep removed a runtime used after the list: %v", err)
	}
	if report.Len() > 0 {
		t.Fatalf("report=%q, want no removed runtime", report.String())
	}
}

// plantRuntime creates a runtime directory of another Seek version under
// cacheRoot, ages it, and returns it.
func plantRuntime(tb testing.TB, cacheRoot, version string, age time.Duration) string {
	tb.Helper()
	dir := filepath.Join(cacheRoot, rerankerDir, version, "runtime")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(dir, stamp, stamp); err != nil {
		tb.Fatal(err)
	}
	return dir
}

// plantProviderArtifact creates a compiled model directory under cacheRoot and
// backdates it by age when age is positive. It returns the directory.
func plantProviderArtifact(tb testing.TB, cacheRoot, key string, age time.Duration) string {
	tb.Helper()
	dir := filepath.Join(cacheRoot, "reranker", "coreml", "mlprogram-static", key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatal(err)
	}
	if age > 0 {
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(dir, stamp, stamp); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

// TestOrphanManifestTmpNames checks the temp names that the orphan sweep
// removes. They are the fixed .tmp name of the family manifest and the temp
// files of writeSharedCacheFile (.bm25-v2, .size). The records and .state
// stay.
func TestOrphanManifestTmpNames(t *testing.T) {
	for name, want := range map[string]bool{
		familyManifestFile + ".tmp":           true,
		bm25StatsFile + sharedTmpInfix + "1":  true,
		corpusSizeFile + sharedTmpInfix + "2": true,
		bm25StatsFile:                         false,
		corpusSizeFile:                        false,
		stateFile:                             false,
	} {
		if got := isOrphanManifestTmpName(name); got != want {
			t.Errorf("isOrphanManifestTmpName(%q) = %t, want %t", name, got, want)
		}
	}
}

// TestSweepCorpusOrphansRemovesOldSharedTmpFiles runs the orphan sweep on a
// corpus that holds the temp files of killed writers. The files older than
// one hour go. A fresh temp file and the records stay.
func TestSweepCorpusOrphansRemovesOldSharedTmpFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	write := func(name string, at time.Time) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gone := []string{write(bm25StatsFile+sharedTmpInfix+"1", old), write(corpusSizeFile+sharedTmpInfix+"2", old)}
	kept := []string{write(bm25StatsFile+sharedTmpInfix+"3", now), write(bm25StatsFile, old), write(corpusSizeFile, old)}
	sweepCorpusOrphans(corpusDirEntry{name: "c", path: dir}, now)
	for _, path := range gone {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s stays after the sweep: %v", filepath.Base(path), err)
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the sweep removed %s: %v", filepath.Base(path), err)
		}
	}
}

// TestSweepRetiredRuntimesKeepsThisBinarysRuntimes plans and runs the runtime
// sweep. The sweep removes old runtimes of other versions and a cut sweep's
// leftover. Each runtime counts by its own age. It keeps this
// binary's runtimes, other provider caches, and recent or used runtimes. The
// plan lists each runtime that the sweep removes. An emptied or old empty
// version directory goes; a new empty one stays.
func TestSweepRetiredRuntimesKeepsThisBinarysRuntimes(t *testing.T) {
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	// dir makes root/parts with a file and ages every directory of the path.
	dir := func(at time.Time, parts ...string) string {
		t.Helper()
		path := filepath.Join(append([]string{root}, parts...)...)
		writeFixtureFile(t, filepath.Join(path, "lib"), "x")
		for p := path; p != root; p = filepath.Dir(p) {
			if err := os.Chtimes(p, at, at); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	// touched marks a directory as used now, as touchRuntimeUse does.
	touched := func(path string) string {
		t.Helper()
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
		return path
	}
	kept := []string{
		dir(old, rerankerDir, bundle.Version, bundle.SHA256),
		dir(old, "semantic", "usearch", semanticUSearchAssetKey()),
		dir(old, "semantic", "numkong", semanticNumKongAssetKey()),
		dir(old, rerankerDir, "coreml", "mlprogram-static", "key"),
		dir(now, rerankerDir, "0.0.2", "recent"),
		touched(dir(old, rerankerDir, "0.0.4", "used")),
		dir(old, rerankerDir, "otherprovider", "key"),
	}
	stale := dir(old, rerankerDir, "0.0.5", "stale")
	touched(filepath.Dir(stale))
	gone := []string{
		stale,
		dir(old, rerankerDir, "0.0.1", "retired"),
		dir(old, rerankerDir, bundle.Version, "retired-sha"),
		dir(old, "semantic", "usearch", "retired"),
		dir(old, "semantic", "numkong", "retired"),
		dir(now, "semantic", "usearch", providerArtifactDiscardPrefix+"cut"),
	}
	// An old empty version directory is no runtime, but it goes. A new one
	// can be an install in progress, so it stays.
	empty := filepath.Join(root, rerankerDir, "0.0.7")
	newEmpty := filepath.Join(root, rerankerDir, "0.0.8")
	for _, path := range []string{empty, newEmpty} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(empty, old, old); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, newEmpty)
	var report bytes.Buffer
	// The plan is the output of seek gc --dry-run on this cache.
	t.Setenv("SEEK_CACHE_DIR", root)
	plan := captureStdoutGC(t, func() {
		if err := runGCCommandCmd(t.Context(), gcCmdOptions{dryRun: true, sort: "name"}); err != nil {
			t.Fatal(err)
		}
	})
	// Each runtime holds one byte, so the bytes show the size measure too.
	if !strings.Contains(plan, "retired runtimes to remove: 6 (6B)") {
		t.Errorf("plan=%q, want 6 runtimes (6B) to remove", plan)
	}
	for _, path := range gone {
		if !strings.Contains(plan, path) {
			t.Errorf("the plan does not list %s", path)
		}
	}
	sweepRetiredRuntimes(t.Context(), &report, root, now.Add(-providerArtifactMaxAge))
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the sweep removed %s: %v", path, err)
		}
	}
	for _, path := range gone {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s stays after the sweep: %v", path, err)
		}
	}
	if !strings.Contains(report.String(), "retired runtimes removed: 6 (6B)") {
		t.Errorf("report=%q, want 6 removed runtimes (6B)", report.String())
	}
	if _, err := os.Stat(filepath.Join(root, rerankerDir, "0.0.1")); !os.IsNotExist(err) {
		t.Errorf("the emptied version directory stays: %v", err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Errorf("the old empty version directory stays: %v", err)
	}
}

// TestRuntimeUseTouchesItsDirectory ages the installed ONNX Runtime, USearch,
// and NumKong directories, then uses each runtime again. Each use must move its
// directory's time to now. sweepRetiredRuntimes reads that time.
func TestRuntimeUseTouchesItsDirectory(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		install func() (string, error)
	}{
		{"onnx runtime", func() (string, error) { return ensureLateOnRuntime(bundle) }},
		{"usearch", extractSemanticUSearchRuntime},
		{"numkong", extractSemanticNumKongRuntime},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := test.install()
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(path)
			old := time.Now().Add(-30 * 24 * time.Hour)
			if err := os.Chtimes(dir, old, old); err != nil {
				t.Fatal(err)
			}
			if _, err := test.install(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(info.ModTime()) > time.Hour {
				t.Fatalf("directory time after a use: %v, want now", info.ModTime())
			}
		})
	}
}
