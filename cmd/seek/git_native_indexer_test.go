package main

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt"
)

func captureHeadForTest(t *testing.T, repoDir string) gitSnapshot {
	t.Helper()
	head := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	snapshot, ok, err := captureGitSnapshot(t.Context(), repoDir, head)
	if err != nil || !ok {
		t.Fatalf("capture HEAD: ok=%t error=%v", ok, err)
	}
	return snapshot
}

func TestCaptureGitSnapshotAndRepositoryMetadata(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	if _, _, err := captureGitSnapshot(t.Context(), repoDir, "no-head"); err != nil {
		t.Fatalf("capture unborn snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", "main.go")
	gitRunIn(t, repoDir, "commit", "-m", "snapshot")
	gitRunIn(t, repoDir, "config", "zoekt.name", "native-metadata")
	gitRunIn(t, repoDir, "config", "zoekt.web-url", "https://github.com/dualeai/seek")
	gitRunIn(t, repoDir, "config", "zoekt.web-url-type", "github")
	gitRunIn(t, repoDir, "config", "zoekt.repoid", "42")
	gitRunIn(t, repoDir, "config", "zoekt.tenantID", "7")
	gitRunIn(t, repoDir, "config", "zoekt.github-stars", "100")

	head := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	snapshot, ok, err := captureGitSnapshot(t.Context(), repoDir, head)
	if err != nil || !ok {
		t.Fatalf("capture snapshot: ok=%t error=%v", ok, err)
	}
	if snapshot.commitOID.String() != head {
		t.Fatalf("snapshot commit=%s, want %s", snapshot.commitOID, head)
	}
	wantUnix, err := strconv.ParseInt(gitOutputIn(t, repoDir, "show", "-s", "--format=%ct", head), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.commitTime.Unix() != wantUnix {
		t.Fatalf("snapshot time=%d, want %d", snapshot.commitTime.Unix(), wantUnix)
	}

	repository, err := nativeGitRepository(t.Context(), repoDir, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if repository.Name != "native-metadata" || repository.Source != repoDir {
		t.Fatalf("repository identity=%q source=%q", repository.Name, repository.Source)
	}
	if repository.URL != "https://github.com/dualeai/seek" {
		t.Fatalf("repository URL=%q", repository.URL)
	}
	if repository.CommitURLTemplate != "" || repository.FileURLTemplate != "" || repository.LineFragmentTemplate != "" {
		t.Fatalf("provider-neutral repository has URL templates: %+v", repository)
	}
	if repository.ID != 42 || repository.TenantID != 7 || repository.Rank != 0 {
		t.Fatalf("repository numeric metadata=%+v", repository)
	}
	if len(repository.Branches) != 1 || repository.Branches[0].Name != "HEAD" || repository.Branches[0].Version != head {
		t.Fatalf("repository branches=%v", repository.Branches)
	}
	if repository.LatestCommitDate.Unix() != wantUnix || repository.RawConfig["name"] != "native-metadata" {
		t.Fatalf("repository time/raw config=%s/%v", repository.LatestCommitDate, repository.RawConfig)
	}
}

func TestCaptureGitSnapshotPreservesCommitTimezone(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepoNoRemote(t)
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-31T23:30:00-07:00")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-31T23:30:00-07:00")
	gitRunIn(t, repoDir, "commit", "-m", "offset")

	snapshot := captureHeadForTest(t, repoDir)
	want, err := time.Parse(time.RFC3339, gitOutputIn(t, repoDir, "show", "-s", "--format=%cI", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	_, gotOffset := snapshot.commitTime.Zone()
	_, wantOffset := want.Zone()
	if !snapshot.commitTime.Equal(want) || gotOffset != wantOffset {
		t.Fatalf("commit time=%s offset=%d, want %s offset=%d", snapshot.commitTime, gotOffset, want, wantOffset)
	}
}

func TestCaptureGitSnapshotRejectsInvalidCommit(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	for _, captured := range []string{"short", "ABCDEF0123456789ABCDEF0123456789ABCDEF01", "0123456789abcdef0123456789abcdef01234567"} {
		if _, _, err := captureGitSnapshot(t.Context(), repoDir, captured); err == nil {
			t.Fatalf("capture %q succeeded", captured)
		}
	}
}

func TestNativeGitSHA256FullAndDelta(t *testing.T) {
	requireTools(t)
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", "--object-format=sha256", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("installed Git does not support SHA-256 repositories: %v: %s", err, out)
	}
	gitRunIn(t, repoDir, "config", "user.email", "test@test.com")
	gitRunIn(t, repoDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repoDir, "sha256.go"), []byte("package sha256\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "sha256")
	head := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	if len(head) != 64 {
		t.Fatalf("SHA-256 HEAD has %d bytes", len(head))
	}
	snapshot, ok, err := captureGitSnapshot(t.Context(), repoDir, head)
	if err != nil || !ok || snapshot.commitOID.String() != head {
		t.Fatalf("capture SHA-256: snapshot=%+v ok=%t error=%v", snapshot, ok, err)
	}

	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, t.Context(), paths, plan)
	if got, want := zoektIndexedPaths(t, plan.indexDir), []string{"sha256.go"}; !slices.Equal(got, want) {
		t.Fatalf("SHA-256 full paths=%v, want %v", got, want)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "next.go"), []byte("package sha256\n// SHA256_DELTA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", "next.go")
	gitRunIn(t, repoDir, "commit", "-m", "sha256 delta")
	target := captureHeadForTest(t, paths.RepoDir)
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	delta, eligible, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.indexDir, target, scan)
	if err != nil || !eligible {
		t.Fatalf("prepare SHA-256 delta: eligible=%t error=%v", eligible, err)
	}
	deltaDir := t.TempDir()
	if _, err := indexNativeGitDelta(t.Context(), paths.RepoDir, deltaDir, scan.paths(familyCommitted), delta); err != nil {
		t.Fatalf("build SHA-256 delta: %v", err)
	}
	fullDir := t.TempDir()
	if _, err := indexNativeGitFull(t.Context(), paths.RepoDir, fullDir, target, nil, 1); err != nil {
		t.Fatalf("build SHA-256 full: %v", err)
	}
	if got, want := nativeVisibleDocuments(t, deltaDir), nativeVisibleDocuments(t, fullDir); !slices.Equal(got, want) {
		t.Fatalf("SHA-256 delta differs from full\ndelta=%+v\nfull=%+v", got, want)
	}
	assertNativeRepositoryParity(t, deltaDir, fullDir)
}

func TestNativeGitRepositoryIgnoresOriginHosting(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "origin")
	snapshot := captureHeadForTest(t, repoDir)
	for _, origin := range []string{
		"git@github.com:sourcegraph/zoekt.git",
		"https://gitlab.example/group/repo.git",
		"ssh://git@gerrit.example/project",
	} {
		gitRunIn(t, repoDir, "config", "remote.origin.url", origin)
		repository, err := nativeGitRepository(t.Context(), repoDir, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if repository.Name != fallbackGitRepositoryName(repoDir) || repository.URL != "" ||
			repository.CommitURLTemplate != "" || repository.FileURLTemplate != "" || repository.Rank != 0 {
			t.Fatalf("origin %q changed provider-neutral metadata: %+v", origin, repository)
		}
	}
}

func TestNativeGitRepositoryNormalizesConfigKeys(t *testing.T) {
	requireTools(t)
	repoDir := initGitRepo(t, "main.go", "package main\n")
	gitRunIn(t, repoDir, "config", "zoekt.name", "case-test")
	gitRunIn(t, repoDir, "config", "zoekt.tenantID", "17")
	gitRunIn(t, repoDir, "config", "zoekt.latestCommitDate", "1")
	repository, err := nativeGitRepository(t.Context(), repoDir, captureHeadForTest(t, repoDir))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"name":             "case-test",
		"tenantid":         "17",
		"latestcommitdate": "1",
	}
	wantRank := nativeGitCommitRank(repository.LatestCommitDate)
	if repository.TenantID != 17 || repository.Rank != wantRank || !maps.Equal(repository.RawConfig, want) {
		t.Fatalf("tenant=%d rank=%d raw config=%v, want rank=%d config=%v", repository.TenantID, repository.Rank, repository.RawConfig, wantRank, want)
	}

	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, t.Context(), paths, plan)
	persisted := zoektRepositoryMetadata(t, plan.indexDir)
	if persisted.Rank != wantRank || !maps.Equal(persisted.RawConfig, want) {
		t.Fatalf("persisted rank=%d raw config=%v, want rank=%d config=%v", persisted.Rank, persisted.RawConfig, wantRank, want)
	}
}

func TestNativeGitRepositoryConfigDoesNotRequireOrigin(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepoNoRemote(t)
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "no origin")
	gitRunIn(t, repoDir, "config", "zoekt.repoid", "42")
	repository, err := nativeGitRepository(t.Context(), repoDir, captureHeadForTest(t, repoDir))
	if err != nil {
		t.Fatal(err)
	}
	if repository.Name != fallbackGitRepositoryName(repoDir) || repository.ID != 42 || repository.RawConfig["repoid"] != "42" {
		t.Fatalf("no-origin repository=%+v", repository)
	}
}

func TestNativeGitRepositoryKeepsExplicitWebURLOpaque(t *testing.T) {
	repository := zoekt.Repository{Name: "fallback"}
	applyNativeGitConfig(&repository, nativeGitConfig{zoekt: map[string]string{
		"web-url":      "custom+transport://example.test/repo?view=raw",
		"web-url-type": "github",
	}})
	if repository.Name != "fallback" || repository.URL != "custom+transport://example.test/repo?view=raw" ||
		repository.CommitURLTemplate != "" || repository.FileURLTemplate != "" || repository.LineFragmentTemplate != "" {
		t.Fatalf("provider-neutral metadata=%+v", repository)
	}
}

func TestNativeGitRepositoryRejectsDirtyShardNames(t *testing.T) {
	for _, name := range []string{"uncommitted", "Uncommitted", "uncommitted_v17", "UNCOMMITTED_vcustom"} {
		repository := zoekt.Repository{Name: "safe-fallback"}
		applyNativeGitConfig(&repository, nativeGitConfig{zoekt: map[string]string{"name": name}})
		if repository.Name != "safe-fallback" {
			t.Fatalf("configured name %q replaced safe fallback with %q", name, repository.Name)
		}
	}
}

func TestNativeGitFullVisibleContract(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	for name, content := range map[string][]byte{
		".sourcegraph/ignore": []byte("ignored.go\n"),
		"binary.bin":          {0, 1, 2, 0, 3},
		"empty.txt":           nil,
		"ignored.go":          []byte("package ignored\n// NATIVE_IGNORED\n"),
		"regular.go":          []byte("package regular\n// NATIVE_VISIBLE\n"),
		"script.sh":           []byte("#!/bin/sh\necho NATIVE_EXECUTABLE\n"),
	} {
		path := filepath.Join(repoDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(repoDir, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular.go", filepath.Join(repoDir, "link.go")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	oversize := filepath.Join(repoDir, "oversize.dat")
	if err := os.WriteFile(oversize, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversize, int64(maxIndexedDocumentBytes)+1); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "native visible contract")
	gitlinkOID := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	gitRunIn(t, repoDir, "update-index", "--add", "--cacheinfo", "160000,"+gitlinkOID+",submodule")
	gitRunIn(t, repoDir, "commit", "-m", "native gitlink")
	gitRunIn(t, repoDir, "config", "zoekt.name", "native-visible")
	gitRunIn(t, repoDir, "config", "zoekt.web-url", "https://github.com/dualeai/seek")
	gitRunIn(t, repoDir, "config", "zoekt.web-url-type", "github")
	gitRunIn(t, repoDir, "config", "zoekt.repoid", "42")
	gitRunIn(t, repoDir, "config", "zoekt.tenantID", "7")
	gitRunIn(t, repoDir, "config", "zoekt.github-stars", "100")

	head := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	snapshot, ok, err := captureGitSnapshot(t.Context(), repoDir, head)
	if err != nil || !ok {
		t.Fatalf("capture snapshot: ok=%t error=%v", ok, err)
	}
	indexDir := t.TempDir()
	indexed, err := indexNativeGitFull(t.Context(), repoDir, indexDir, snapshot, nil, 1)
	if err != nil || !indexed {
		t.Fatalf("native full: indexed=%t error=%v", indexed, err)
	}
	want := []string{".sourcegraph/ignore", "binary.bin", "empty.txt", "link.go", "oversize.dat", "regular.go", "script.sh"}
	if got := zoektIndexedPaths(t, indexDir); !slices.Equal(got, want) {
		t.Fatalf("indexed paths=%v, want %v", got, want)
	}
	repository := zoektRepositoryMetadata(t, indexDir)
	if repository.Name != "native-visible" || repository.Source != repoDir || repository.URL != "https://github.com/dualeai/seek" ||
		repository.ID != 42 || repository.TenantID != 7 || repository.Rank != 0 || repository.RawConfig["name"] != "native-visible" ||
		len(repository.Branches) != 1 || repository.Branches[0].Name != "HEAD" || repository.Branches[0].Version != head ||
		repository.LatestCommitDate.Unix() != snapshot.commitTime.Unix() || repository.IndexOptions == "" || !repository.HasSymbols {
		t.Fatalf("indexed repository metadata=%+v", repository)
	}
	if repository.CommitURLTemplate != "" || repository.FileURLTemplate != "" || repository.LineFragmentTemplate != "" {
		t.Fatalf("provider-neutral repository has URL templates: %+v", repository)
	}
	matches, err := executeUnscopedShardSearchForTest(context.Background(), indexDir, "branch:HEAD NATIVE_VISIBLE")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].FileName != "regular.go" || !slices.Equal(matches[0].Branches, []string{"HEAD"}) {
		t.Fatalf("matches=%v", matches)
	}
}

func TestNativeGitFullRotatesBuilderWindows(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	wantPaths := make([]string, 0, 4)
	for i := range 4 {
		name := "window-" + strconv.Itoa(i) + ".go"
		wantPaths = append(wantPaths, name)
		content := "package window\n// WINDOW_" + strconv.Itoa(i) + "_CONTENT\n"
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "builder windows")
	snapshot := captureHeadForTest(t, repoDir)

	testReadSemMu.Lock()
	restoreWindow := swapIndexWindowBytesForTest(16)
	defer func() {
		restoreWindow()
		testReadSemMu.Unlock()
	}()

	indexDir := t.TempDir()
	indexed, err := indexNativeGitFull(t.Context(), repoDir, indexDir, snapshot, nil, 1)
	if err != nil || !indexed {
		t.Fatalf("native full: indexed=%t error=%v", indexed, err)
	}
	shards, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) < 2 {
		t.Fatalf("builder did not rotate: shards=%v", shards)
	}
	if got := zoektIndexedPaths(t, indexDir); !slices.Equal(got, wantPaths) {
		t.Fatalf("indexed paths=%v, want %v", got, wantPaths)
	}
	for _, repository := range zoektCommittedRepositoryMetadata(t, indexDir) {
		if len(repository.Branches) != 1 || repository.Branches[0].Name != "HEAD" ||
			repository.Branches[0].Version != snapshot.commitOID.String() {
			t.Fatalf("rotated shard metadata=%+v", repository)
		}
	}
}

func TestReadNativeGitTreeEmptyAndRepeatedBlob(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	gitRunIn(t, repoDir, "commit", "--allow-empty", "-m", "empty")
	empty := captureHeadForTest(t, repoDir)
	count := 0
	if err := readNativeGitTree(t.Context(), repoDir, empty, nil, func(entries []gitTreeEntry) error {
		count += len(entries)
		return nil
	}); err != nil || count != 0 {
		t.Fatalf("empty tree count=%d error=%v", count, err)
	}

	content := []byte("same blob\n")
	for _, name := range []string{"a.txt", "dir/b.txt"} {
		path := filepath.Join(repoDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "repeated blob")
	snapshot := captureHeadForTest(t, repoDir)
	var entries []gitTreeEntry
	if err := readNativeGitTree(t.Context(), repoDir, snapshot, nil, func(chunk []gitTreeEntry) error {
		entries = append(entries, chunk...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].oid != entries[1].oid {
		t.Fatalf("tree entries=%+v", entries)
	}
}

func TestNativeGitFullScopeUsesRootIgnoreAndExclusions(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	for name, content := range map[string]string{
		".sourcegraph/ignore": "scope/ignored.go\n",
		"scope/keep.go":       "package keep\n// SCOPE_KEEP\n",
		"scope/ignored.go":    "package ignored\n// SCOPE_IGNORED\n",
		"scope/excluded.go":   "package excluded\n// SCOPE_EXCLUDED\n",
		"other/out.go":        "package out\n// SCOPE_OUT\n",
	} {
		path := filepath.Join(repoDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "scope")
	snapshot := captureHeadForTest(t, repoDir)
	scope := &gitDirtyScope{includeDirs: []string{"scope"}, excludeFiles: []string{"scope/excluded.go"}}
	indexDir := t.TempDir()
	indexed, err := indexNativeGitFull(t.Context(), repoDir, indexDir, snapshot, scope, 1)
	if err != nil || !indexed {
		t.Fatalf("scoped native full: indexed=%t error=%v", indexed, err)
	}
	if got, want := zoektIndexedPaths(t, indexDir), []string{"scope/keep.go"}; !slices.Equal(got, want) {
		t.Fatalf("scoped paths=%v, want %v", got, want)
	}
}

func TestReadNativeGitIgnoreRejectsOversizeControlFile(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	path := filepath.Join(repoDir, ".sourcegraph", "ignore")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(maxIndexedDocumentBytes)+1); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "oversize ignore")
	err := func() error {
		_, err := readNativeGitIgnore(t.Context(), repoDir, captureHeadForTest(t, repoDir))
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error=%v", err)
	}
}

func TestNativeGitBudgetCountsIgnoredBlobs(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, ".sourcegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".sourcegraph", "ignore"), []byte("ignored.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored.go"), []byte("package ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "ignored budget")

	oldFiles := gitCandidateFileLimit
	gitCandidateFileLimit = 1
	t.Cleanup(func() { gitCandidateFileLimit = oldFiles })
	_, err := indexNativeGitFull(t.Context(), repoDir, t.TempDir(), captureHeadForTest(t, repoDir), nil, 1)
	if !errors.Is(err, errGitCommittedCapExceeded) {
		t.Fatalf("error=%v, want committed candidate cap", err)
	}
}

func TestNativeGitBudgetCountsIgnoredBlobBytes(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, ".sourcegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	ignoreContent := []byte("ignored.go\n")
	ignoredContent := []byte("package ignored\n")
	if err := os.WriteFile(filepath.Join(repoDir, ".sourcegraph", "ignore"), ignoreContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored.go"), ignoredContent, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", ".")
	gitRunIn(t, repoDir, "commit", "-m", "ignored byte budget")

	oldBytes := gitCorpusIndexedByteLimit
	gitCorpusIndexedByteLimit = int64(len(ignoreContent) + len(ignoredContent) - 1)
	t.Cleanup(func() { gitCorpusIndexedByteLimit = oldBytes })
	_, err := indexNativeGitFull(t.Context(), repoDir, t.TempDir(), captureHeadForTest(t, repoDir), nil, 1)
	if !errors.Is(err, errGitCommittedCapExceeded) {
		t.Fatalf("error=%v, want committed byte cap", err)
	}
	capErr, ok := errors.AsType[indexCapExceededError](err)
	if !ok || capErr.metric != indexCapIndexedBytes {
		t.Fatalf("error=%v, want indexed-byte cap", err)
	}
}
