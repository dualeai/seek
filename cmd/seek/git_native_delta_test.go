package main

import (
	"bytes"
	"context"
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

func TestReadNativeGitDiffFixedStatuses(t *testing.T) {
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
		got = append(got, fmt.Sprintf("%c:%s", entry.status, entry.newPath))
	}
	sort.Strings(got)
	want := []string{
		"A:add.go",
		"A:rename-new.go",
		"D:delete.go",
		"D:rename-old.go",
		"M:mode.go",
		"M:modify.go",
		"T:type.go",
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
	want := fullRepositories[0]
	for _, repositories := range [][]*zoekt.Repository{fullRepositories, deltaRepositories} {
		for _, repository := range repositories {
			if repository.Name != want.Name || repository.Source != want.Source || repository.URL != want.URL ||
				repository.ID != want.ID || repository.TenantID != want.TenantID || repository.Rank != want.Rank ||
				repository.CommitURLTemplate != want.CommitURLTemplate || repository.FileURLTemplate != want.FileURLTemplate ||
				repository.LineFragmentTemplate != want.LineFragmentTemplate || repository.HasSymbols != want.HasSymbols ||
				!reflect.DeepEqual(repository.RawConfig, want.RawConfig) || !reflect.DeepEqual(repository.Metadata, want.Metadata) ||
				repository.LatestCommitDate.Unix() != want.LatestCommitDate.Unix() ||
				fmt.Sprint(repository.Branches) != fmt.Sprint(want.Branches) || repository.IndexOptions != want.IndexOptions {
				t.Fatalf("repository mismatch\ngot=%+v\nwant=%+v", repository, want)
			}
		}
	}
}

func TestNativeGitDeltaMatchesCleanFull(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "add",
			mutate: func(t *testing.T, repoDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repoDir, "added.go"), []byte("package added\n// DELTA_ADDED\n"), 0o644); err != nil {
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
				if err := os.WriteFile(filepath.Join(repoDir, "change.go"), []byte("package changed\n// DELTA_MODIFIED\n"), 0o644); err != nil {
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
					t.Skipf("symlink unavailable: %v", err)
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
				"keep.go":   "package keep\n// DELTA_KEEP\n",
				"change.go": "package change\n// DELTA_BASE\n",
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
			delta, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, target, scan)
			if err != nil || !eligible {
				t.Fatalf("prepare delta: eligible=%t error=%v", eligible, err)
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
			if _, err := indexNativeGitDelta(t.Context(), paths.RepoDir, deltaDir, scan.paths(familyCommitted), delta); err != nil {
				t.Fatalf("build delta: %v", err)
			}
			for path, want := range seedBytes {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("live seed changed: path=%s error=%v", path, err)
				}
			}

			fullDir := t.TempDir()
			if _, err := indexNativeGitFull(t.Context(), paths.RepoDir, fullDir, target, nil, 1); err != nil {
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
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
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
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
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
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
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
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
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
		_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
		if err != nil || eligible {
			t.Fatalf("metadata key removal: eligible=%t error=%v", eligible, err)
		}
	})
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
	_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
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
			_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
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
	_, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, captureHeadForTest(t, paths.RepoDir), scan)
	if err != nil || eligible {
		t.Fatalf("missing base: eligible=%t error=%v", eligible, err)
	}
}
