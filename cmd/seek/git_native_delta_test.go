package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
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
		t.Skipf("symlink unavailable: %v", err)
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
		got = append(got, fmt.Sprintf("%s>%s:%s", entry.oldMode, entry.newMode, entry.newPath))
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
	searchers, err := loadShardsOptional(indexDir)
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
		// Delta shards can store path tombstones that a clean full shard does not
		// need. Document parity above tests their effect. All repository metadata
		// must be equal.
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
		name       string
		wantBudget gitIndexBudget
		mutate     func(*testing.T, string)
	}{
		{
			name:       "add",
			wantBudget: gitIndexBudget{candidates: 3, indexedBytes: int64(len(keepContent) + len(baseContent) + len(addedContent))},
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repoDir, "added.go"), []byte(addedContent), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "delete",
			wantBudget: gitIndexBudget{candidates: 1, indexedBytes: int64(len(keepContent))},
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.Remove(filepath.Join(repoDir, "change.go")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "modify",
			wantBudget: gitIndexBudget{candidates: 2, indexedBytes: int64(len(keepContent) + len(modifiedContent))},
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repoDir, "change.go"), []byte(modifiedContent), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "type-change",
			wantBudget: gitIndexBudget{candidates: 2, indexedBytes: int64(len(keepContent) + len("keep.go"))},
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.Remove(filepath.Join(repoDir, "change.go")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("keep.go", filepath.Join(repoDir, "change.go")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
		{
			name:       "mode-only",
			wantBudget: gitIndexBudget{candidates: 2, indexedBytes: int64(len(keepContent) + len(baseContent))},
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.Chmod(filepath.Join(repoDir, "change.go"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "rename",
			wantBudget: gitIndexBudget{candidates: 2, indexedBytes: int64(len(keepContent) + len(baseContent))},
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				gitRunIn(t, repoDir, "mv", "change.go", "renamed.go")
			},
		},
		{
			name:       "empty-diff",
			wantBudget: gitIndexBudget{candidates: 2, indexedBytes: int64(len(keepContent) + len(baseContent))},
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
			delta, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, target, scan)
			if err != nil || !eligible {
				t.Fatalf("prepare delta: eligible=%t error=%v", eligible, err)
			}
			if delta.nextState.budget != test.wantBudget {
				t.Fatalf("delta budget=%+v, want %+v", delta.nextState.budget, test.wantBudget)
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
			if err := indexNativeGitDelta(t.Context(), paths.RepoDir, deltaDir, scan.paths(familyCommitted), delta); err != nil {
				t.Fatalf("build delta: %v", err)
			}
			for path, want := range seedBytes {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("live seed changed: path=%s error=%v", path, err)
				}
			}

			fullDir := t.TempDir()
			if err := indexNativeGitFull(t.Context(), paths.RepoDir, fullDir, target, nil, 1); err != nil {
				t.Fatalf("build clean full: %v", err)
			}
			if got, want := nativeVisibleDocuments(t, deltaDir), nativeVisibleDocuments(t, fullDir); !slices.Equal(got, want) {
				t.Fatalf("visible documents differ\ndelta=%+v\nfull=%+v", got, want)
			}
			assertNativeRepositoryParity(t, deltaDir, fullDir)
		})
	}
}

func prepareNativeDeltaFixture(t *testing.T) (string, gitPaths, corpusPlan) {
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

func TestPrepareNativeGitDeltaFallsBackBeforeSeeding(t *testing.T) {
	t.Run("ignore-change", func(t *testing.T) {
		repoDir, paths, plan := prepareNativeDeltaFixture(t)
		if err := os.MkdirAll(filepath.Join(repoDir, ".sourcegraph"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, ".sourcegraph", "ignore"), []byte("base.go\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, repoDir, "add", ".")
		gitRunIn(t, repoDir, "commit", "-m", "ignore change")
		scan, err := scanFamily(plan.indexDir)
		if err != nil {
			t.Fatal(err)
		}
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
		if err != nil || eligible {
			t.Fatalf("ignore change: eligible=%t error=%v", eligible, err)
		}
	})

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
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
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
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
		if err != nil || eligible {
			t.Fatalf("invalid manifest: eligible=%t error=%v", eligible, err)
		}
	})

	t.Run("mutable-metadata-change", func(t *testing.T) {
		repoDir, paths, plan := prepareNativeDeltaFixture(t)
		if err := os.WriteFile(filepath.Join(repoDir, "next.go"), []byte("package next\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, repoDir, "add", ".")
		gitRunIn(t, repoDir, "commit", "-m", "metadata change")
		gitRunIn(t, repoDir, "config", "zoekt.latestCommitDate", "1")
		scan, err := scanFamily(plan.indexDir)
		if err != nil {
			t.Fatal(err)
		}
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
		if err != nil || eligible {
			t.Fatalf("mutable metadata change: eligible=%t error=%v", eligible, err)
		}
	})

	t.Run("metadata-key-removal", func(t *testing.T) {
		repoDir := initGitRepo(t, "base.go", "package base\n")
		gitRunIn(t, repoDir, "config", "zoekt.name", "metadata-removal")
		gitRunIn(t, repoDir, "config", "zoekt.tenantID", "9")
		gitRunIn(t, repoDir, "config", "zoekt.github-stars", "100")
		paths, plan := planGitTestCorpus(t, repoDir)
		reindexGit(t, context.Background(), paths, plan)
		gitRunIn(t, repoDir, "config", "--unset", "zoekt.tenantID")
		gitRunIn(t, repoDir, "config", "--unset", "zoekt.github-stars")
		if err := os.WriteFile(filepath.Join(repoDir, "next.go"), []byte("package next\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, repoDir, "add", ".")
		gitRunIn(t, repoDir, "commit", "-m", "remove metadata")
		scan, err := scanFamily(plan.indexDir)
		if err != nil {
			t.Fatal(err)
		}
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
		if err != nil || eligible {
			t.Fatalf("metadata key removal: eligible=%t error=%v", eligible, err)
		}
	})
}

func TestPrepareNativeGitDeltaRejectsCommittedStateDamage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*testing.T, corpusPlan, gitSnapshot)
	}{
		{
			name: "missing",
			damage: func(t *testing.T, plan corpusPlan, _ gitSnapshot) {
				t.Helper()
				if err := os.Remove(filepath.Join(plan.cacheDir, committedGitStateFile)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed",
			damage: func(t *testing.T, plan corpusPlan, _ gitSnapshot) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(plan.cacheDir, committedGitStateFile), []byte("not state\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong-head",
			damage: func(t *testing.T, plan corpusPlan, target gitSnapshot) {
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
			damage: func(t *testing.T, plan corpusPlan, _ gitSnapshot) {
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
			_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, target, scan)
			if err != nil || eligible {
				t.Fatalf("damaged state: eligible=%t error=%v", eligible, err)
			}
		})
	}
}

func TestPrepareNativeGitDeltaChecksUpdatedCorpusTotals(t *testing.T) {
	for _, metric := range []indexCapMetric{indexCapCandidateFiles, indexCapIndexedBytes} {
		t.Run(string(metric), func(t *testing.T) {
			repoDir, paths, plan := prepareNativeDeltaFixture(t)
			baseState, ok := readCommittedGitState(plan.cacheDir)
			if !ok {
				t.Fatal("read committed state")
			}
			if err := os.WriteFile(filepath.Join(repoDir, "added.go"), []byte("package added\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRunIn(t, repoDir, "add", "added.go")
			gitRunIn(t, repoDir, "commit", "-m", "cross saved total")

			oldFiles, oldBytes := gitCandidateFileLimit, gitCorpusIndexedByteLimit
			if metric == indexCapCandidateFiles {
				gitCandidateFileLimit = baseState.budget.candidates
			} else {
				gitCorpusIndexedByteLimit = baseState.budget.indexedBytes
			}
			t.Cleanup(func() {
				gitCandidateFileLimit = oldFiles
				gitCorpusIndexedByteLimit = oldBytes
			})

			scan, err := scanFamily(plan.indexDir)
			if err != nil {
				t.Fatal(err)
			}
			_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, repoDir), scan)
			if eligible || !errors.Is(err, errGitCommittedCapExceeded) {
				t.Fatalf("eligible=%t error=%v, want committed %s cap error", eligible, err, metric)
			}
			capErr, ok := errors.AsType[indexCapExceededError](err)
			if !ok || capErr.metric != metric {
				t.Fatalf("cap error=%+v, want metric %s", capErr, metric)
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
	_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
	if err != nil || !eligible {
		t.Fatalf("priority delta: eligible=%t error=%v", eligible, err)
	}
}

func TestPrepareNativeGitDeltaCommitDateRank(t *testing.T) {
	for _, tc := range []struct {
		name       string
		targetDate string
		eligible   bool
	}{
		{name: "same-month", targetDate: "2026-01-20T12:00:00Z", eligible: true},
		{name: "next-month", targetDate: "2026-02-01T12:00:00Z", eligible: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireTools(t)
			repoDir := initEmptyGitRepo(t)
			if err := os.WriteFile(filepath.Join(repoDir, "base.go"), []byte("package base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRunIn(t, repoDir, "add", ".")
			t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T12:00:00Z")
			t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T12:00:00Z")
			gitRunIn(t, repoDir, "commit", "-m", "base")
			gitRunIn(t, repoDir, "config", "zoekt.latestCommitDate", "")
			paths, plan := planGitTestCorpus(t, repoDir)
			reindexGit(t, t.Context(), paths, plan)

			if err := os.WriteFile(filepath.Join(repoDir, "base.go"), []byte("package changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRunIn(t, repoDir, "add", "base.go")
			t.Setenv("GIT_AUTHOR_DATE", tc.targetDate)
			t.Setenv("GIT_COMMITTER_DATE", tc.targetDate)
			gitRunIn(t, repoDir, "commit", "-m", "target")
			scan, err := scanFamily(plan.indexDir)
			if err != nil {
				t.Fatal(err)
			}
			_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
			if err != nil || eligible != tc.eligible {
				t.Fatalf("eligible=%t, want %t; error=%v", eligible, tc.eligible, err)
			}
		})
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
		t.Skip("Git retained the unreachable base object")
	}
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.cacheDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
	if err != nil || eligible {
		t.Fatalf("missing base: eligible=%t error=%v", eligible, err)
	}
}
