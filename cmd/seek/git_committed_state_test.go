package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCommittedGitStateEncodingAndDamage(t *testing.T) {
	cacheDir := t.TempDir()
	path := filepath.Join(cacheDir, committedGitStateFile)
	want := committedGitState{
		head:       gitObjectID(nativeTestOID),
		baseHead:   gitObjectID(nativeTestOID),
		budget:     gitIndexBudget{candidates: 123, indexedBytes: 456789},
		baseShards: 153,
	}
	wantRaw := "v1\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\ncandidates 123\nindexed_bytes 456789\nbase_shards 153\n"

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

	// Write the valid record without the production writer so the reader has an
	// independent input contract.
	if err := os.WriteFile(path, []byte(wantRaw), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := readCommittedGitState(cacheDir); !ok || got != want {
		t.Fatalf("state=%+v ok=%t, want %+v", got, ok, want)
	}

	for _, raw := range []string{
		"",
		"v2\nhead " + nativeTestOID + "\n",
		"v1\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\ncandidates 1\nindexed_bytes 1\nbase_shards 1",
		"v1\nhead bad\nbase_head " + nativeTestOID + "\ncandidates 1\nindexed_bytes 1\nbase_shards 1\n",
		"v1\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\ncandidates -1\nindexed_bytes 1\nbase_shards 1\n",
		"v1\nhead " + nativeTestOID + "\nbase_head " + nativeTestOID + "\ncandidates 1\nindexed_bytes 1\nbase_shards -1\n",
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
	deleteStateFiles(cacheDir)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("committed state remains after state cleanup: %v", err)
	}
}

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
	delta, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, target, scan)
	if err != nil || !eligible {
		t.Fatalf("large-base delta: eligible=%t error=%v", eligible, err)
	}
	if delta.nextState.baseShards != baseShards || delta.nextState.baseHead != state.baseHead {
		t.Fatalf("next state=%+v, want base_shards=%d base_head=%s", delta.nextState, baseShards, state.baseHead)
	}
}

func TestCommittedGitStateTracksOnlyChangedBlobTotals(t *testing.T) {
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
	if !ok || base.head != baseHead || base.baseHead != baseHead || base.budget != (gitIndexBudget{candidates: 3, indexedBytes: int64(len(baseA) + len(baseB) + len(stable))}) {
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

	// Delta admission needs one non-recursive ls-tree call for the root ignore
	// file. Reject only the recursive full-tree command. This checks the process
	// boundary without copying the delta accounting algorithm into the test.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	shim := `#!/bin/sh
command_name=
recursive=false
for arg do
  case "$arg" in
    -r) recursive=true ;;
    -*) ;;
    *)
      if [ -z "$command_name" ]; then
        command_name=$arg
      fi
      ;;
  esac
done
if [ "$command_name" = ls-tree ] && [ "$recursive" = true ]; then
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
	wantBudget := gitIndexBudget{candidates: 3, indexedBytes: int64(len(targetB) + len(targetC) + len(stable))}
	if !ok || target.head != targetHead || target.baseHead != baseHead || target.baseShards != base.baseShards || target.budget != wantBudget {
		t.Fatalf("target state=%+v ok=%t, want head=%s base_head=%s base_shards=%d budget=%+v", target, ok, targetHead, baseHead, base.baseShards, wantBudget)
	}
	for _, queryText := range []string{"DELTA_STABLE_MARKER", "DELTA_NEW_MARKER"} {
		matches, err := executeUnscopedShardSearchForTest(t.Context(), plan.indexDir, queryText)
		if err != nil || len(matches) != 1 {
			t.Fatalf("search %q: matches=%v error=%v", queryText, matches, err)
		}
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
