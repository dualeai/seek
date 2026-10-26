package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

func TestReadNativeGitDiffFixedChanges(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	for name := range map[string]struct{}{
		"delete.go":     {},
		"modify.go":     {},
		"mode.go":       {},
		"type.go":       {},
		"rename-old.go": {},
	} {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte("package base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "diff base")
	base := gitObjectID(gitOutputIn(t, repoDir, "rev-parse", "HEAD"))

	if err := os.Remove(filepath.Join(repoDir, "delete.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "modify.go"), []byte("package modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repoDir, "mode.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repoDir, "type.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("modify.go", filepath.Join(repoDir, "type.go")); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "mv", "rename-old.go", "rename-new.go")
	if err := os.WriteFile(filepath.Join(repoDir, "add.go"), []byte("package added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", "-A")
	gitRunIn(t, repoDir, "commit", "-m", "all diff statuses")
	target := gitObjectID(gitOutputIn(t, repoDir, "rev-parse", "HEAD"))

	entries, err := readNativeGitDiff(t.Context(), repoDir, base, target, 20)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, fmt.Sprintf("%s>%s:%s", entry.oldMode, entry.newMode, entry.path))
	}
	sort.Strings(got)
	want := []string{
		"000000>100644:add.go",
		"000000>100644:rename-new.go",
		"100644>000000:delete.go",
		"100644>000000:rename-old.go",
		"100644>100644:modify.go",
		"100644>100755:mode.go",
		"100644>120000:type.go",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("diff=%v, want %v", got, want)
	}
}

type nativeVisibleDocument struct {
	name     string
	content  string
	branches string
	version  string
}

func nativeVisibleDocuments(t testing.TB, indexDir string) []nativeVisibleDocument {
	t.Helper()
	searchers, err := loadShards(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, searcher := range searchers {
			searcher.Close()
		}
	}()
	documents := make([]nativeVisibleDocument, 0)
	for _, searcher := range searchers {
		result, err := searcher.Search(context.Background(), &query.Const{Value: true}, &zoekt.SearchOptions{Whole: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range result.Files {
			documents = append(documents, nativeVisibleDocument{
				name:     file.FileName,
				content:  string(file.Content),
				branches: fmt.Sprint(file.Branches),
				version:  file.Version,
			})
		}
	}
	sort.Slice(documents, func(i, j int) bool {
		left, right := documents[i], documents[j]
		if left.name != right.name {
			return left.name < right.name
		}
		if left.version != right.version {
			return left.version < right.version
		}
		if left.content != right.content {
			return left.content < right.content
		}
		return left.branches < right.branches
	})
	return documents
}

func assertNativeRepositoryParity(t testing.TB, deltaDir, fullDir string) {
	t.Helper()
	fullRepositories := zoektCommittedRepositoryMetadata(t, fullDir)
	deltaRepositories := zoektCommittedRepositoryMetadata(t, deltaDir)
	normalize := func(repository *zoekt.Repository) zoekt.Repository {
		result := *repository
		// Delta shards can store path tombstones that a clean full shard does
		// not need. The callers' document comparison tests their effect. All
		// other repository metadata must be equal.
		result.FileTombstones = nil
		return result
	}
	want := normalize(fullRepositories[0])
	for _, repositories := range [][]*zoekt.Repository{fullRepositories, deltaRepositories} {
		for _, repository := range repositories {
			got := normalize(repository)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("repository mismatch\ngot=%+v\nwant=%+v", got, want)
			}
		}
	}
}

func TestNativeGitDeltaMatchesCleanFull(t *testing.T) {
	const (
		keepContent     = "package keep\n// DELTA_KEEP\n"
		baseContent     = "package change\n// DELTA_BASE\n"
		addedContent    = "package added\n// DELTA_ADDED\n"
		modifiedContent = "package changed\n// DELTA_MODIFIED\n"
	)
	mutations := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "add",
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repoDir, "added.go"), []byte(addedContent), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "delete",
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.Remove(filepath.Join(repoDir, "change.go")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "modify",
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repoDir, "change.go"), []byte(modifiedContent), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "type-change",
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.Remove(filepath.Join(repoDir, "change.go")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("keep.go", filepath.Join(repoDir, "change.go")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "mode-only",
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.Chmod(filepath.Join(repoDir, "change.go"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "rename",
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				gitRunIn(t, repoDir, "mv", "change.go", "renamed.go")
			},
		},
		{
			name: "empty-diff",
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
			},
		},
	}

	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			requireTools(t)
			repoDir := initEmptyGitRepo(t)
			for name, content := range map[string]string{
				"keep.go":   keepContent,
				"change.go": baseContent,
			} {
				if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gitRunIn(t, repoDir, "add", ".")
			gitRunIn(t, repoDir, "commit", "-m", "delta base")
			paths, plan := planGitTestCorpus(t, repoDir)
			reindexGit(t, context.Background(), paths, plan)

			test.mutate(t, repoDir)
			gitRunIn(t, repoDir, "add", "-A")
			commitArgs := []string{"commit", "-m", test.name}
			if test.name == "empty-diff" {
				commitArgs = []string{"commit", "--allow-empty", "-m", test.name}
			}
			gitRunIn(t, repoDir, commitArgs...)
			target := captureHeadForTest(t, paths.RepoDir)
			scan, err := scanFamily(plan.indexDir)
			if err != nil {
				t.Fatal(err)
			}
			delta, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, target, scan)
			if err != nil || !eligible {
				t.Fatalf("prepare delta: eligible=%t error=%v", eligible, err)
			}
			if delta.nextState.head != target.commitOID {
				t.Fatalf("delta head=%s, want %s", delta.nextState.head, target.commitOID)
			}

			seedBytes := make(map[string][]byte)
			for _, path := range scan.paths(familyCommitted) {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				seedBytes[path] = content
			}
			deltaDir := t.TempDir()
			if err := indexNativeGitDelta(t.Context(), paths.RepoDir, deltaDir, scan.paths(familyCommitted), delta, indexParallelism()); err != nil {
				t.Fatalf("build delta: %v", err)
			}
			for path, want := range seedBytes {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("live seed changed: path=%s error=%v", path, err)
				}
			}

			fullDir := t.TempDir()
			if _, err := indexNativeGitFull(t.Context(), paths.RepoDir, fullDir, target, 1, nil); err != nil {
				t.Fatalf("build clean full: %v", err)
			}
			if got, want := nativeVisibleDocuments(t, deltaDir), nativeVisibleDocuments(t, fullDir); !slices.Equal(got, want) {
				t.Fatalf("visible documents differ\ndelta=%+v\nfull=%+v", got, want)
			}
			assertNativeRepositoryParity(t, deltaDir, fullDir)
		})
	}
}

func prepareNativeDeltaFixture(t *testing.T) (string, gitPaths, node) {
	t.Helper()
	repoDir := initEmptyGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "base.go"), []byte("package base\n// NATIVE_DELTA_BASE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "base")
	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, context.Background(), paths, plan)
	return repoDir, paths, plan
}

// TestNativeGitIndexesFilesThatAnIgnoreFileNames commits a root
// .sourcegraph/ignore that names two files. Seek reads no ignore file: after
// the delta that adds it and the next delta, the named files match.
func TestNativeGitIndexesFilesThatAnIgnoreFileNames(t *testing.T) {
	requireTools(t)
	repoDir, paths, plan := prepareNativeDeltaFixture(t)
	for name, content := range map[string]string{
		".sourcegraph/ignore": "base.go\nnamed.go\n",
		"named.go":            "package named\n// IGNORE_NAMED_FIRST\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repoDir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "ignore file")
	reindexGit(t, t.Context(), paths, plan)
	if err := os.WriteFile(filepath.Join(repoDir, "named.go"), []byte("package named\n// IGNORE_NAMED_SECOND\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "commit", "-a", "-m", "named change")
	reindexGit(t, t.Context(), paths, plan)

	for marker, want := range map[string]int{"NATIVE_DELTA_BASE": 1, "IGNORE_NAMED_FIRST": 0, "IGNORE_NAMED_SECOND": 1} {
		results, err := searchPlannedCorpusForTest(t.Context(), plan, marker)
		if err != nil || len(results) != want {
			t.Fatalf("%s matches %d files, want %d; error=%v", marker, len(results), want, err)
		}
	}
}

// TestNativeGitTombstonedBytesCompactTheFamily commits the removal of 3 of 10
// files of one size, then of 2 more. The first delta tombstones 3 files
// against 7 live ones and stays a delta. With the second, deltas tombstoned 5
// files against 5 live ones, so the build replaces the family
// (deltaNeedsCompaction).
func TestNativeGitTombstonedBytesCompactTheFamily(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	repoDir := initEmptyGitRepo(t)
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(repoDir, fmt.Sprintf("c%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "ten files")
	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, t.Context(), paths, plan)
	committed := func() shardFileState {
		t.Helper()
		return readShardFileState(t, plan.indexDir, fallbackGitRepositoryName(repoDir))
	}
	remove := func(files ...int) {
		t.Helper()
		for _, i := range files {
			gitRunIn(t, repoDir, "rm", "-q", fmt.Sprintf("c%02d.go", i))
		}
		gitRunIn(t, repoDir, "commit", "-m", "remove")
		reindexGit(t, t.Context(), paths, plan)
	}

	before := committed()
	remove(0, 1, 2)
	requireTombstoneOnlyDelta(t, before, committed())

	before = committed()
	remove(3, 4)
	requireFullRebuild(t, before, committed())
	for i := range 10 {
		results, err := searchPlannedCorpusForTest(t.Context(), plan, fmt.Sprintf("compact_marker_%02d", i))
		if want := min(max(i-4, 0), 1); err != nil || len(results) != want {
			t.Fatalf("compact_marker_%02d matches %d files, want %d; error=%v", i, len(results), want, err)
		}
	}
}

// TestNativeGitBinaryBlobsCountTheirMarker commits the removal of 8 of 10
// text files beside a blob of about 10,000 bytes whose NUL byte comes after the
// reader's probe: the reader sends its content, and Zoekt keeps it by name
// only. The full build counts the blob as its marker, so 8 dead text files
// against 2 live ones and the marker build the family in full
// (deltaNeedsCompaction).
func TestNativeGitBinaryBlobsCountTheirMarker(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	repoDir := initEmptyGitRepo(t)
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(repoDir, fmt.Sprintf("c%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lateBinary := append(bytes.Repeat([]byte("binary filler\n"), 700), 0)
	if err := os.WriteFile(filepath.Join(repoDir, "blob.bin"), lateBinary, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "ten files and a binary")
	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, t.Context(), paths, plan)
	before := readShardFileState(t, plan.indexDir, fallbackGitRepositoryName(repoDir))
	for i := range 8 {
		gitRunIn(t, repoDir, "rm", "-q", fmt.Sprintf("c%02d.go", i))
	}
	gitRunIn(t, repoDir, "commit", "-m", "remove")
	reindexGit(t, t.Context(), paths, plan)
	requireFullRebuild(t, before, readShardFileState(t, plan.indexDir, fallbackGitRepositoryName(repoDir)))
}

// TestNativeGitDeltaCountsANewBinaryBlobAsItsMarker commits a binary blob of
// 8,000 bytes. The delta admits it at its size, and its read adds its 36-byte
// marker, not its 8,000 bytes, to the live bytes of the committed state.
func TestNativeGitDeltaCountsANewBinaryBlobAsItsMarker(t *testing.T) {
	requireTools(t)
	repoDir := initGitRepo(t, "a.go", "package a\n// base\n")
	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, t.Context(), paths, plan)
	base, ok := readCommittedGitState(plan.cacheDir)
	if !ok {
		t.Fatal("no committed state after the full build")
	}
	if err := os.WriteFile(filepath.Join(repoDir, "blob.bin"), binaryFixtureFile, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", "blob.bin")
	gitRunIn(t, repoDir, "commit", "-qm", "binary")
	reindexGit(t, t.Context(), paths, plan)
	next, ok := readCommittedGitState(plan.cacheDir)
	if !ok || next.baseHead != base.baseHead {
		t.Fatalf("state ok=%t base=%s, want the delta on base %s", ok, next.baseHead, base.baseHead)
	}
	if got, want := next.liveBytes-base.liveBytes, int64(len("NOT-INDEXED: contains binary content")); got != want {
		t.Fatalf("live bytes grew by %d, want %d", got, want)
	}
}

// TestNativeGitBinaryOnlyCommitsStayDeltas commits 3 changes of only a binary
// blob of 8,000 bytes beside 30 text files. The delta reads each old blob, so
// each commit tombstones its 36-byte marker, not its 8,000 bytes: every commit
// stays a delta, and the dead bytes grow by 36 each time.
func TestNativeGitBinaryOnlyCommitsStayDeltas(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	repoDir := initEmptyGitRepo(t)
	for i := range 30 {
		if err := os.WriteFile(filepath.Join(repoDir, fmt.Sprintf("c%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "blob.bin"), binaryFixtureFile, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-qm", "base")
	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, t.Context(), paths, plan)
	marker := int64(len("NOT-INDEXED: contains binary content"))
	for step := 1; step <= 3; step++ {
		blob := bytes.Repeat([]byte{0, byte(step + 10), 2, 3}, 2000)
		if err := os.WriteFile(filepath.Join(repoDir, "blob.bin"), blob, 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, repoDir, "commit", "-qam", fmt.Sprintf("binary %d", step))
		before := readShardFileState(t, plan.indexDir, fallbackGitRepositoryName(repoDir))
		reindexGit(t, t.Context(), paths, plan)
		requireTombstoneOnlyDeltaOrNewShard(t, before, readShardFileState(t, plan.indexDir, fallbackGitRepositoryName(repoDir)))
		state, ok := readCommittedGitState(plan.cacheDir)
		if !ok || state.deadBytes != marker*int64(step) {
			t.Fatalf("commit %d: dead_bytes=%d ok=%t, want %d", step, state.deadBytes, ok, marker*int64(step))
		}
	}
}

func TestPrepareNativeGitDeltaFallsBackBeforeSeeding(t *testing.T) {
	t.Run("payload-window", func(t *testing.T) {
		repoDir, paths, plan := prepareNativeDeltaFixture(t)
		if err := os.WriteFile(filepath.Join(repoDir, "base.go"), []byte("package changed\n// payload larger than test window\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, repoDir, "add", ".")
		gitRunIn(t, repoDir, "commit", "-m", "large payload")
		scan, err := scanFamily(plan.indexDir)
		if err != nil {
			t.Fatal(err)
		}
		oldWindow := indexWindowBytes
		indexWindowBytes = 1
		defer func() { indexWindowBytes = oldWindow }()
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, captureHeadForTest(t, paths.RepoDir), scan)
		if err != nil || eligible {
			t.Fatalf("payload cap: eligible=%t error=%v", eligible, err)
		}
	})

	t.Run("invalid-manifest", func(t *testing.T) {
		repoDir, paths, plan := prepareNativeDeltaFixture(t)
		if err := os.WriteFile(filepath.Join(repoDir, "next.go"), []byte("package next\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, repoDir, "add", ".")
		gitRunIn(t, repoDir, "commit", "-m", "next")
		scan, err := scanFamily(plan.indexDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plan.indexDir, familyManifestFile), []byte("invalid\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, captureHeadForTest(t, paths.RepoDir), scan)
		if err != nil || eligible {
			t.Fatalf("invalid manifest: eligible=%t error=%v", eligible, err)
		}
	})

}

// TestPrepareNativeGitDeltaIgnoresZoektConfig changes the zoekt.* Git config
// with a commit. Seek reads no such config, so the delta stays eligible.
func TestPrepareNativeGitDeltaIgnoresZoektConfig(t *testing.T) {
	requireTools(t)
	repoDir, paths, plan := prepareNativeDeltaFixture(t)
	if err := os.WriteFile(filepath.Join(repoDir, "next.go"), []byte("package next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "config change")
	gitRunIn(t, repoDir, "config", "zoekt.name", "config-change")
	gitRunIn(t, repoDir, "config", "zoekt.web-url", "https://example.test/config-change")
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, captureHeadForTest(t, paths.RepoDir), scan)
	if err != nil || !eligible {
		t.Fatalf("zoekt config change: eligible=%t error=%v", eligible, err)
	}
}

func TestPrepareNativeGitDeltaRejectsCommittedStateDamage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*testing.T, node, gitSnapshot)
	}{
		{
			name: "missing",
			damage: func(t *testing.T, plan node, _ gitSnapshot) {
				t.Helper()
				if err := os.Remove(filepath.Join(plan.cacheDir, committedGitStateFile)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed",
			damage: func(t *testing.T, plan node, _ gitSnapshot) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(plan.cacheDir, committedGitStateFile), []byte("not state\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong-head",
			damage: func(t *testing.T, plan node, target gitSnapshot) {
				t.Helper()
				state, ok := readCommittedGitState(plan.cacheDir)
				if !ok {
					t.Fatal("read committed state")
				}
				state.head = target.commitOID
				if err := writeCommittedGitState(plan.cacheDir, state); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "base-shards-ahead",
			damage: func(t *testing.T, plan node, _ gitSnapshot) {
				t.Helper()
				state, ok := readCommittedGitState(plan.cacheDir)
				if !ok {
					t.Fatal("read committed state")
				}
				state.baseShards++
				if err := writeCommittedGitState(plan.cacheDir, state); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir, paths, plan := prepareNativeDeltaFixture(t)
			if err := os.WriteFile(filepath.Join(repoDir, "next.go"), []byte("package next\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRunIn(t, repoDir, "add", "next.go")
			gitRunIn(t, repoDir, "commit", "-m", "next")
			target := captureHeadForTest(t, repoDir)
			tc.damage(t, plan, target)
			scan, err := scanFamily(plan.indexDir)
			if err != nil {
				t.Fatal(err)
			}
			_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, target, scan)
			if err != nil || eligible {
				t.Fatalf("damaged state: eligible=%t error=%v", eligible, err)
			}
		})
	}
}

func TestPrepareNativeGitDeltaAcceptsExplicitPriority(t *testing.T) {
	requireTools(t)
	repoDir := initGitRepo(t, "base.go", "package base\n")
	gitRunIn(t, repoDir, "config", "zoekt.priority", "7")
	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, context.Background(), paths, plan)
	if err := os.WriteFile(filepath.Join(repoDir, "base.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", "base.go")
	gitRunIn(t, repoDir, "commit", "-m", "priority delta")
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, captureHeadForTest(t, paths.RepoDir), scan)
	if err != nil || !eligible {
		t.Fatalf("priority delta: eligible=%t error=%v", eligible, err)
	}
}

func TestPrepareNativeGitDeltaMissingBaseSelectsFull(t *testing.T) {
	repoDir, paths, plan := prepareNativeDeltaFixture(t)
	oldBranch := gitOutputIn(t, repoDir, "branch", "--show-current")
	oldCommit := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	gitRunIn(t, repoDir, "checkout", "--orphan", "replacement")
	gitRunIn(t, repoDir, "rm", "-rf", ".")
	if err := os.WriteFile(filepath.Join(repoDir, "replacement.go"), []byte("package replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "replacement root")
	gitRunIn(t, repoDir, "branch", "-D", oldBranch)
	gitRunIn(t, repoDir, "reflog", "expire", "--expire=now", "--all")
	gitRunIn(t, repoDir, "gc", "--prune=now")
	probe := exec.Command("git", "cat-file", "-e", oldCommit+"^{commit}")
	probe.Dir = repoDir
	if err := probe.Run(); err == nil {
		t.Fatal("Git kept the unreachable base commit after gc --prune=now, so the base is not missing")
	}
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, captureHeadForTest(t, paths.RepoDir), scan)
	if err != nil || eligible {
		t.Fatalf("missing base: eligible=%t error=%v", eligible, err)
	}
}

// TestNativeDeltaWithNullShardMetadataIsNoCrash writes "null" into the .meta
// file of committed shard 0, so that Zoekt's metadata reader panics, and then
// asks for a native delta. The read runs through the shard-call guard: the
// delta is not possible, and the process keeps running.
func TestNativeDeltaWithNullShardMetadataIsNoCrash(t *testing.T) {
	requireTools(t)
	repoDir, paths, plan := prepareNativeDeltaFixture(t)
	writeFixtureFile(t, filepath.Join(repoDir, "next.go"), "package next\n")
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-q", "-m", "next")
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	shard, ok := committedShardZeroPath(scan)
	if !ok {
		t.Fatal("no shard 0")
	}
	writeFixtureFile(t, shard+".meta", "null")
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("the delta check panicked: %v", recovered)
		}
	}()
	if _, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, captureHeadForTest(t, paths.RepoDir), scan); eligible {
		t.Fatalf("a delta on a base with a null record was eligible (err=%v)", err)
	}
}

// TestNativeDeltaSeedLinkErrorFallsBack gives the delta a seed shard that
// does not link: a full build links no seed, so the error wraps both
// errNativeDeltaFallback, which asks for a full build, and the link error.
func TestNativeDeltaSeedLinkErrorFallsBack(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "git-x_v16.00000.zoekt")
	err := indexNativeGitDelta(t.Context(), t.TempDir(), t.TempDir(), []string{missing}, &nativeGitDelta{}, 1)
	if !errors.Is(err, errNativeDeltaFallback) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("seed link error %v, want errNativeDeltaFallback with the link error", err)
	}
}

// TestNativeDeltaOnADamagedRecordBuildsInFull commits three times, so the
// committed family has later shards with records, damages the record of
// shard 1 with same-size bytes that keep the file time, and commits once
// more. The native delta reads that record in its Finish, which panics on
// "null" and returns a JSON error on other bytes: in both cases the stage
// builds the family in full, with no warning, and the search finds the new
// file.
func TestNativeDeltaOnADamagedRecordBuildsInFull(t *testing.T) {
	requireTools(t)
	for _, record := range []string{"null", "{garbage"} {
		t.Run(record, func(t *testing.T) {
			setTestUserCache(t)
			repo := initGitRepo(t, "f0.go", "package f\n// zqdelta0\n")
			t.Chdir(repo)
			search := func(query string) string {
				t.Helper()
				output, err := seek(t, "--lexical-only", query)
				if err != nil {
					t.Fatalf("%s: %v\n%s", query, err, output)
				}
				return output
			}
			search("zqdelta0")
			for index := 1; index <= 3; index++ {
				writeFileAt(t, repo, fmt.Sprintf("f%d.go", index), fmt.Sprintf("package f\n// zqdelta%d\n", index))
				gitRunIn(t, repo, "add", ".")
				gitRunIn(t, repo, "commit", "-q", "-m", fmt.Sprintf("f%d", index))
				search(fmt.Sprintf("zqdelta%d", index))
			}
			cache, err := seekUserCacheRoot()
			if err != nil {
				t.Fatal(err)
			}
			metas, _ := filepath.Glob(filepath.Join(cache, "corpora", "*", "index", "git-*_v16.00001.zoekt.meta"))
			if len(metas) != 1 {
				t.Fatalf("no record on shard 1 (%v): the deltas wrote no later shard", metas)
			}
			info, err := os.Stat(metas[0])
			if err != nil {
				t.Fatal(err)
			}
			damaged := []byte(record)
			damaged = append(damaged, bytes.Repeat([]byte(" "), int(info.Size())-len(damaged))...)
			if err := os.WriteFile(metas[0], damaged, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(metas[0], info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			writeFileAt(t, repo, "f4.go", "package f\n// zqdelta4\n")
			gitRunIn(t, repo, "add", ".")
			gitRunIn(t, repo, "commit", "-q", "-m", "f4")
			logs := captureTestLogs(t, slog.LevelWarn)
			if output := search("zqdelta4"); !strings.Contains(output, "f4.go") {
				t.Fatalf("the search after the damage lost the new file:\n%s", output)
			}
			if records := logs.Records(); len(records) > 0 {
				t.Fatalf("the damaged delta gave the warning %q", records[0].Message)
			}
		})
	}
}
