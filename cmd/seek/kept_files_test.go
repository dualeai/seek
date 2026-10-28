package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// dirtyEIOHook makes the open of the named dirty files fail with EIO while on
// is true, and records every open.
type dirtyEIOHook struct {
	mu     sync.Mutex
	on     bool
	names  []string
	opened []string
}

// installDirtyEIO installs a dirtyEIOHook that fails the opens of names until
// set(false), and restores dirtyOpenFile when the test ends.
func installDirtyEIO(t *testing.T, names ...string) *dirtyEIOHook {
	t.Helper()
	hook := &dirtyEIOHook{on: true, names: names}
	realOpen := dirtyOpenFile
	t.Cleanup(func() { dirtyOpenFile = realOpen })
	dirtyOpenFile = func(path string) (*os.File, error) {
		hook.mu.Lock()
		hook.opened = append(hook.opened, filepath.Base(path))
		failing := hook.on && slices.Contains(hook.names, filepath.Base(path))
		hook.mu.Unlock()
		if failing {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EIO}
		}
		return realOpen(path)
	}
	return hook
}

// set turns the failure on or off and clears the record of opens.
func (h *dirtyEIOHook) set(on bool) {
	h.mu.Lock()
	h.on, h.opened = on, nil
	h.mu.Unlock()
}

// reads returns the base names of the files opened since the last set.
func (h *dirtyEIOHook) reads() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.opened)
}

// TestKeptFileWarningReachesTheSearch keeps an untracked file by name through
// the root command. The search prints the other file and writes one warning.
// The warning gives the count, the first path and the cause, so the kept-file
// error reaches the warning from the build with its type.
func TestKeptFileWarningReachesTheSearch(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "c.txt", "zqwarn committed\n")
	for _, name := range []string{"a.txt", "b.txt"} {
		writeFixtureFile(t, filepath.Join(repo, name), "zqwarn dirty "+name+"\n")
	}
	setTestUserCache(t)
	installDirtyEIO(t, "b.txt")
	logs := captureTestLogs(t, slog.LevelWarn)
	if out, headers := searchHeaders(t, "zqwarn dirty", repo); !slices.Equal(headers, []string{filepath.Join(repo, "a.txt")}) {
		t.Fatalf("headers=%v, want a.txt:\n%s", headers, out)
	}
	records := logs.Records()
	want := map[string]any{"files": int64(1), "first": "b.txt", "cause": "input/output error"}
	if len(records) != 1 || records[0].Message !=
		"Some uncommitted files could not be read; they are searchable by name only until a later search reads them" ||
		!maps.Equal(testLogAttrs(records[0]), want) {
		t.Fatalf("warnings=%v, want one kept-file warning with %v", records, want)
	}
}

// TestKeptFileRemovedLeavesNoGhost keeps an untracked file by name, then
// removes it: its marked manifest entry gives the tombstone.
func TestKeptFileRemovedLeavesNoGhost(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "c.txt", "zqghost committed\n")
	for _, name := range []string{"a.txt", "zqghostb.txt", "d.txt"} {
		writeFixtureFile(t, filepath.Join(repo, name), "zqghost dirty "+name+"\n")
	}
	setTestUserCache(t)
	hook := installDirtyEIO(t, "zqghostb.txt")
	searchHeaders(t, "zqghostb", repo)
	hook.set(false)
	if err := os.Remove(filepath.Join(repo, "zqghostb.txt")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if out, headers := searchHeaders(t, "zqghostb", repo); len(headers) != 0 {
			t.Fatalf("the removed file still shows: %v\n%s", headers, out)
		}
	}
}

// TestKeptFileRevertedLeavesNoGhost keeps a modified tracked file by name,
// then reverts it: the next search shows the committed file.
func TestKeptFileRevertedLeavesNoGhost(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "zqrevb.txt", "zqrev committed\n")
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "zqrev dirty a\n")
	writeFixtureFile(t, filepath.Join(repo, "zqrevb.txt"), "zqrev dirty b\n")
	setTestUserCache(t)
	hook := installDirtyEIO(t, "zqrevb.txt")
	searchHeaders(t, "zqrevb", repo)
	hook.set(false)
	gitRunIn(t, repo, "checkout", "--", "zqrevb.txt")
	if out, _ := searchHeaders(t, "zqrevb", repo); strings.Contains(out, "[uncommitted]") || !strings.Contains(out, "zqrevb.txt") {
		t.Fatalf("want the committed file, no uncommitted ghost:\n%s", out)
	}
}

// TestKeptFileFailingAgainWritesNoShard keeps one file unreadable over many
// searches: each search reads only that file and writes no shard.
func TestKeptFileFailingAgainWritesNoShard(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "c.txt", "zqgrow committed\n")
	for index := range 20 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("f%02d.txt", index)), fmt.Sprintf("zqgrow dirty %d\n", index))
	}
	setTestUserCache(t)
	hook := installDirtyEIO(t, "f07.txt")
	plans, err := planCorpora(context.Background(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	searchHeaders(t, "zqgrow", repo)
	shards := len(repositoryShardFiles(plans[0].indexDir, repoUncommitted))
	for run := range 4 {
		hook.set(true)
		searchHeaders(t, "zqgrow", repo)
		if got := len(repositoryShardFiles(plans[0].indexDir, repoUncommitted)); got != shards || !slices.Equal(hook.reads(), []string{"f07.txt"}) {
			t.Fatalf("search %d: shards %d (want %d), reads %v (want f07.txt alone)", run+2, got, shards, hook.reads())
		}
	}
	hook.set(false)
	if _, headers := searchHeaders(t, "zqgrow", repo); len(headers) != 21 {
		t.Fatalf("after the error: %d hits, want 21", len(headers))
	}
}

// TestCancelDuringKeptDeltaKeepsChanges cancels a search while its delta
// reads changed files and keeps one by name. The cancel runs in the debug line
// that keeps the file. The delta writes nothing, so the next search reads every
// change.
func TestCancelDuringKeptDeltaKeepsChanges(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "c.txt", "zqcancel committed\n")
	for _, name := range []string{"x1.txt", "x2.txt", "x3.txt", "x4.txt"} {
		writeFixtureFile(t, filepath.Join(repo, name), "zqcancel old "+name+"\n")
	}
	setTestUserCache(t)
	if _, headers := searchHeaders(t, "zqcancel", repo); len(headers) != 5 {
		t.Fatalf("warm: %v", headers)
	}
	for _, name := range []string{"x1.txt", "x2.txt", "x3.txt"} {
		writeFixtureFile(t, filepath.Join(repo, name), "zqcancel new zqnew"+strings.TrimSuffix(name, ".txt")+"\n")
	}
	hook := installDirtyEIO(t, "x2.txt")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	captureTestLogsWithAction(t, slog.LevelWarn, "Indexing an uncommitted file by name only after a read error", cancel)
	// One read worker (indexParallelism follows GOMAXPROCS) reads the files in
	// order: x1.txt, then x2.txt, which the delta keeps by name.
	old := runtime.GOMAXPROCS(1)
	_, _ = captureStdout(t, func() error { return seekRoot(ctx, t, nil, "--lexical-only", "zqcancel", repo) })
	runtime.GOMAXPROCS(old)
	if ctx.Err() == nil {
		t.Fatal("the delta did not keep x2.txt by name, so the test did not cancel")
	}
	hook.set(false)
	for _, token := range []string{"zqnewx1", "zqnewx2", "zqnewx3"} {
		if _, headers := searchHeaders(t, token, repo); len(headers) != 1 {
			t.Errorf("%s: found %v, want its file", token, headers)
		}
	}
}

// TestKeptBuildRecordsTheBudget keeps a dirty file by name on a cold node. The
// build publishes with no state. The budget still records the ledger and
// evicts an older corpus.
func TestKeptBuildRecordsTheBudget(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "c.txt", "zqbudget committed\n")
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "zqbudget dirty a\n")
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "zqbudget dirty b\n")
	setTestUserCache(t)
	root, err := seekUserCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	older := seedCorpus(t, root, fakeCorpusHash(201), time.Now().Add(-48*time.Hour))
	seedCorpus(t, root, fakeCorpusHash(202), time.Now().Add(-24*time.Hour))
	t.Setenv(envCacheBudget, fmt.Sprint(seededCorpusBytes*3/2))
	installDirtyEIO(t, "b.txt")
	searchHeaders(t, "zqbudget", repo)
	plans, err := planCorpora(context.Background(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plans[0].cacheDir, corpusSizeFile)); err != nil {
		t.Fatalf("no ledger after a kept build: %v", err)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("the older corpus stays above the budget: %v", err)
	}
}
