package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestCommittedGitStateEncodingAndDamage checks the bytes that
// writeCommittedGitState writes, that readCommittedGitState rejects each
// damaged or other-format record, and that only deleteCommittedGitState
// removes the record.
func TestCommittedGitStateEncodingAndDamage(t *testing.T) {
	cacheDir := t.TempDir()
	path := filepath.Join(cacheDir, committedGitStateFile)
	want := committedGitState{
		head:       gitObjectID(nativeTestOID),
		baseHead:   gitObjectID(nativeTestOID),
		baseShards: 153,
		liveBytes:  4096,
		deadBytes:  512,
	}
	wantRaw := "v2\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\nbase_shards 153\nlive_bytes 4096\ndead_bytes 512\n"

	if err := writeCommittedGitState(cacheDir, want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != wantRaw {
		t.Fatalf("encoded state=%q, want %q", raw, wantRaw)
	}

	// Write the valid record with no use of the production writer, so the
	// reader test does not depend on it.
	if err := os.WriteFile(path, []byte(wantRaw), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := readCommittedGitState(cacheDir); !ok || got != want {
		t.Fatalf("state=%+v ok=%t, want %+v", got, ok, want)
	}

	for _, raw := range []string{
		"",
		"v2\nhead " + nativeTestOID + "\n",
		// A record of another version, and one with no byte counts.
		"v1\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\nbase_shards 1\nlive_bytes 1\ndead_bytes 0\n",
		"v2\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\nbase_shards 1\n",
		"v2\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\nbase_shards 1\nlive_bytes -1\ndead_bytes 0\n",
		"v2\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\nbase_shards 1",
		"v2\nhead bad\nbase_head " + nativeTestOID + "\nbase_shards 1\n",
		"v2\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\nbase_shards -1\n",
		// Work-total lines (candidates, indexed_bytes) make another format.
		"v2\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\ncandidates 1\nindexed_bytes 1\nbase_shards 1\n",
	} {
		if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := readCommittedGitState(cacheDir); ok {
			t.Fatalf("damaged state %q was accepted as %+v", raw, got)
		}
	}

	if err := writeCommittedGitState(cacheDir, want); err != nil {
		t.Fatal(err)
	}
	// State cleanup keeps the committed record. It describes the delta base of
	// the family that is still on disk, and a failed build does not change that
	// family. A stale record is rejected by prepareNativeGitDelta, which
	// compares it with the commit read from the shard itself.
	deleteStateFiles(cacheDir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state cleanup removed the committed record: %v", err)
	}
	// Only the explicit remover drops it.
	deleteCommittedGitState(cacheDir)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("committed state remains after its own cleanup: %v", err)
	}
}

// TestPrepareNativeGitDeltaAcceptsLargeFullBase gives a full base of
// maxCommittedDeltaShards+1 shards. The delta limit counts the shards above
// the full base, so a commit on top of it still takes the delta path.
func TestPrepareNativeGitDeltaAcceptsLargeFullBase(t *testing.T) {
	repoDir, paths, plan := prepareNativeDeltaFixture(t)
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	var source familyMember
	for _, member := range scan.members {
		if member.shard && !member.uncommitted {
			source = member
			break
		}
	}
	if source.name == "" || !source.numbered || source.num != 0 {
		t.Fatalf("base shard=%+v, want numbered shard zero", source)
	}
	for i := 1; i <= maxCommittedDeltaShards; i++ {
		name := fmt.Sprintf("%s.%05d%s", source.prefix, i, shardSuffix)
		if err := os.Link(filepath.Join(plan.indexDir, source.name), filepath.Join(plan.indexDir, name)); err != nil {
			t.Fatalf("link synthetic base shard %d: %v", i, err)
		}
	}
	scan, err = scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	baseShards := maxCommittedDeltaShards + 1
	if got := nativeCommittedShardCount(scan); got != baseShards || !scan.contiguous(familyCommitted) {
		t.Fatalf("synthetic full base has %d shards and contiguous=%t, want %d contiguous shards", got, scan.contiguous(familyCommitted), baseShards)
	}
	names := make([]string, 0, len(scan.members))
	for _, member := range scan.members {
		names = append(names, member.name)
	}
	if err := writeFamilyManifest(plan.indexDir, names); err != nil {
		t.Fatal(err)
	}
	state, ok := readCommittedGitState(plan.cacheDir)
	if !ok {
		t.Fatal("read committed state")
	}
	state.baseShards = baseShards
	if err := writeCommittedGitState(plan.cacheDir, state); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, "next.go"), []byte("package next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", "next.go")
	gitRunIn(t, repoDir, "commit", "-m", "delta above large full base")
	target := captureHeadForTest(t, repoDir)
	delta, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, target, scan)
	if err != nil || !eligible {
		t.Fatalf("large-base delta: eligible=%t error=%v", eligible, err)
	}
	if delta.nextState.baseShards != baseShards || delta.nextState.baseHead != state.baseHead {
		t.Fatalf("next state=%+v, want base_shards=%d base_head=%s", delta.nextState, baseShards, state.baseHead)
	}
}

func TestNativeGitDeltaShardLimitIsRelativeToFullBase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current int
		base    int
		want    bool
	}{
		{name: "large-full-base", current: 153, base: 153},
		{name: "sixty-four-delta-shards", current: 217, base: 153},
		{name: "sixty-five-delta-shards", current: 218, base: 153, want: true},
		{name: "missing-base", current: 1, base: 0, want: true},
		{name: "base-ahead-of-family", current: 10, base: 11, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeGitDeltaShardLimitExceeded(tc.current, tc.base); got != tc.want {
				t.Fatalf("limit exceeded=%t, want %t (current=%d base=%d)", got, tc.want, tc.current, tc.base)
			}
		})
	}
}

// TestCommittedGitDeltaRunsNoRecursiveLsTree moves HEAD by one commit: the
// committed delta runs no recursive ls-tree and keeps the base head and shard
// count.
func TestCommittedGitDeltaRunsNoRecursiveLsTree(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	baseA := []byte("package a\n")
	baseB := []byte("base b\n")
	stable := []byte("package stable\n// DELTA_STABLE_MARKER\n")
	if err := os.WriteFile(filepath.Join(repoDir, "a.go"), baseA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "b.txt"), baseB, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "stable.go"), stable, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "base")
	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, context.Background(), paths, plan)
	baseHead := gitObjectID(gitOutputIn(t, repoDir, "rev-parse", "HEAD"))
	base, ok := readCommittedGitState(plan.cacheDir)
	if !ok || base.head != baseHead || base.baseHead != baseHead {
		t.Fatalf("base state=%+v ok=%t", base, ok)
	}

	if err := os.Remove(filepath.Join(repoDir, "a.go")); err != nil {
		t.Fatal(err)
	}
	targetB := []byte("changed b content\n")
	targetC := []byte("package c\n// DELTA_NEW_MARKER\n")
	if err := os.WriteFile(filepath.Join(repoDir, "b.txt"), targetB, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "c.go"), targetC, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", "-A")
	gitRunIn(t, repoDir, "commit", "-m", "changed objects")

	// The shim rejects only a recursive ls-tree, the full-tree command, and
	// passes every other Git call to the real Git. This checks the process
	// boundary without copying the delta accounting algorithm into the test.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	shim := "#!/bin/sh\n" + gitShimSubcommand + `recursive=false
for arg do
  if [ "$arg" = -r ]; then
    recursive=true
  fi
done
if [ "$subcommand" = ls-tree ] && [ "$recursive" = true ]; then
  echo unexpected-recursive-ls-tree >&2
  exit 43
fi
exec "$SEEK_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEEK_TEST_REAL_GIT", realGit)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	probe := exec.Command("git", "--literal-pathspecs", "--no-pager", "--no-replace-objects", "ls-tree", "-r", "HEAD")
	probe.Dir = repoDir
	if err := probe.Run(); err == nil {
		t.Fatal("Git wrapper accepted a recursive ls-tree command")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 43 {
		t.Fatalf("Git wrapper precondition: %v", err)
	}

	reindexGit(t, context.Background(), paths, plan)
	targetHead := gitObjectID(gitOutputIn(t, repoDir, "rev-parse", "HEAD"))
	target, ok := readCommittedGitState(plan.cacheDir)
	if !ok || target.head != targetHead || target.baseHead != baseHead || target.baseShards != base.baseShards {
		t.Fatalf("target state=%+v ok=%t, want head=%s base_head=%s base_shards=%d ", target, ok, targetHead, baseHead, base.baseShards)
	}
	for _, queryText := range []string{"DELTA_STABLE_MARKER", "DELTA_NEW_MARKER"} {
		matches, err := searchPlannedCorpusForTest(t.Context(), plan, queryText)
		if err != nil || len(matches) != 1 {
			t.Fatalf("search %q: matches=%v error=%v", queryText, matches, err)
		}
	}
}

// TestWarmSearchReadsNoCommittedState replaces the committed state of a built
// node with a FIFO. A warm search that opened it for reading would wait for a
// writer, so the search must end with no read of it. Only a committed build
// reads the state, to admit a delta. A search that waits stops the test at
// the go test timeout.
func TestWarmSearchReadsNoCommittedState(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "warm.go", "package warm\n// warm_state_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)
	search := func() (string, error) {
		return seek(t, "--lexical-only", "warm_state_marker")
	}
	if out, err := search(); err != nil {
		t.Fatalf("cold search: %v\n%s", err, out)
	}
	paths, err := resolveGitPaths(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planGitCorpus(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(plan.cacheDir, committedGitStateFile)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(statePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := search(); err != nil {
		t.Fatalf("warm search: %v\n%s", err, out)
	}
}
