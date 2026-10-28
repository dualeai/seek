package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
)

func captureHeadForTest(t *testing.T, repoDir string) gitObjectID {
	t.Helper()
	head := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	snapshot, err := captureGitSnapshot(head)
	if err != nil {
		t.Fatalf("capture HEAD: %v", err)
	}
	return snapshot
}

func TestCaptureGitSnapshotAndRepositoryMetadata(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repoDir, "add", "main.go")
	gitRunIn(t, repoDir, "commit", "-m", "snapshot")
	// Seek reads no zoekt.* config and no remote: neither changes the metadata.
	gitRunIn(t, repoDir, "config", "zoekt.name", "native-metadata")
	gitRunIn(t, repoDir, "config", "zoekt.web-url", "https://github.com/dualeai/seek")
	gitRunIn(t, repoDir, "config", "zoekt.repoid", "42")
	gitRunIn(t, repoDir, "config", "zoekt.tenantID", "7")
	gitRunIn(t, repoDir, "config", "remote.origin.url", "git@github.com:sourcegraph/zoekt.git")

	head := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	snapshot, err := captureGitSnapshot(head)
	if err != nil {
		t.Fatalf("capture snapshot: %v", err)
	}
	if snapshot.String() != head {
		t.Fatalf("snapshot commit=%s, want %s", snapshot, head)
	}
	want := zoekt.Repository{
		Name:     fallbackGitRepositoryName(repoDir),
		Source:   repoDir,
		Branches: []zoekt.RepositoryBranch{{Name: "HEAD", Version: head}},
	}
	if got := nativeGitRepository(repoDir, snapshot); !reflect.DeepEqual(got, want) {
		t.Fatalf("repository metadata:\ngot  %+v\nwant %+v", got, want)
	}
}

// TestCaptureGitSnapshotRejectsMalformedCommit checks the form of the ID only.
// TestGitBuildWithAMissingCapturedCommitFails covers a well-formed ID of a
// missing commit.
func TestCaptureGitSnapshotRejectsMalformedCommit(t *testing.T) {
	for _, captured := range []string{"short", "ABCDEF0123456789ABCDEF0123456789ABCDEF01"} {
		if _, err := captureGitSnapshot(captured); err == nil {
			t.Fatalf("capture %q succeeded", captured)
		}
	}
}

// TestGitBuildWithAMissingCapturedCommitFails builds a state whose commit is
// missing, as when the commit disappears after git status. The build fails
// and deletes the state, so the next search builds again.
func TestGitBuildWithAMissingCapturedCommitFails(t *testing.T) {
	requireTools(t)
	repoDir := initGitRepo(t, "base.go", "package base\n// MISSING_COMMIT_BASE\n")
	paths, plan := planGitTestCorpus(t, repoDir)
	reindexGit(t, t.Context(), paths, plan)
	if readStateFile(plan.cacheDir) == "" {
		t.Fatal("the first build wrote no state")
	}
	state := mustGitRepoStateIn(t, t.Context(), repoDir)
	// A well-formed ID of no object in the repository.
	state.HeadSHA = strings.Repeat("0", len(state.HeadSHA)-1) + "1"
	err := runIndexingWithCache(t.Context(), paths, plan.corpusStore, state, gitCorpusStateHash(paths, state))
	if err == nil {
		t.Fatal("the build of a missing commit succeeded")
	}
	if got := readStateFile(plan.cacheDir); got != "" {
		t.Fatalf("state=%q after the failed build, want none", got)
	}
}

func TestNativeGitSHA256FullAndDelta(t *testing.T) {
	requireTools(t)
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", "--object-format=sha256", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the installed Git cannot make a SHA-256 repository: %v: %s", err, out)
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
	snapshot, err := captureGitSnapshot(head)
	if err != nil || snapshot.String() != head {
		t.Fatalf("capture SHA-256: snapshot=%+v error=%v", snapshot, err)
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
	delta, err := prepareNativeGitDelta(t.Context(), paths.RepoDir, plan.corpusStore, target, scan)
	if err != nil || delta == nil {
		t.Fatalf("prepare SHA-256 delta: eligible=%t error=%v", delta != nil, err)
	}
	deltaDir := t.TempDir()
	if err := indexNativeGitDelta(t.Context(), paths.RepoDir, deltaDir, scan.paths(familyCommitted), delta, indexParallelism()); err != nil {
		t.Fatalf("build SHA-256 delta: %v", err)
	}
	fullDir := t.TempDir()
	if _, err := indexNativeGitFull(t.Context(), paths.RepoDir, fullDir, target, 1, nil); err != nil {
		t.Fatalf("build SHA-256 full: %v", err)
	}
	if got, want := nativeVisibleDocuments(t, deltaDir), nativeVisibleDocuments(t, fullDir); !slices.Equal(got, want) {
		t.Fatalf("SHA-256 delta differs from full\ndelta=%+v\nfull=%+v", got, want)
	}
	assertNativeRepositoryParity(t, deltaDir, fullDir)
}

// TestNativeGitFullKeepsHeadInEveryShard builds a commit into several shards
// with a small shardMax. The build indexes every file, and every shard records
// HEAD.
func TestNativeGitFullKeepsHeadInEveryShard(t *testing.T) {
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
	// Each document is about 45 bytes: below this target, so no document
	// fills a shard alone, and each shard holds two documents.
	swapForTest(t, &shardMax, 64)

	indexDir := t.TempDir()
	if _, err := indexNativeGitFull(t.Context(), repoDir, indexDir, snapshot, 1, nil); err != nil {
		t.Fatalf("native full: %v", err)
	}
	shards, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) < 2 {
		t.Fatalf("build made one shard: shards=%v", shards)
	}
	if got := zoektIndexedPaths(t, indexDir); !slices.Equal(got, wantPaths) {
		t.Fatalf("indexed paths=%v, want %v", got, wantPaths)
	}
	for _, repository := range zoektCommittedRepositoryMetadata(t, indexDir) {
		if len(repository.Branches) != 1 || repository.Branches[0].Name != "HEAD" ||
			repository.Branches[0].Version != snapshot.String() {
			t.Fatalf("shard metadata=%+v", repository)
		}
	}
}

func TestReadNativeGitTreeEmptyAndRepeatedBlob(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	gitRunIn(t, repoDir, "commit", "--allow-empty", "-m", "empty")
	empty := captureHeadForTest(t, repoDir)
	count := 0
	if err := readNativeGitTree(t.Context(), repoDir, empty, func(entries []gitTreeEntry) error {
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
	if err := readNativeGitTree(t.Context(), repoDir, snapshot, func(chunk []gitTreeEntry) error {
		entries = append(entries, chunk...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].oid != entries[1].oid {
		t.Fatalf("tree entries=%+v", entries)
	}
}

// TestNativeGitFullIndexesEveryCommittedBlob commits a root
// .sourcegraph/ignore that names a file. Seek reads no ignore file, so the
// full build indexes every committed blob.
func TestNativeGitFullIndexesEveryCommittedBlob(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	for name, content := range map[string]string{
		".sourcegraph/ignore": "scope/ignored.go\n",
		"scope/keep.go":       "package keep\n// SCOPE_KEEP\n",
		"scope/ignored.go":    "package ignored\n// SCOPE_IGNORED\n",
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
	indexDir := t.TempDir()
	if _, err := indexNativeGitFull(t.Context(), repoDir, indexDir, snapshot, 1, nil); err != nil {
		t.Fatalf("native full: %v", err)
	}
	if got, want := zoektIndexedPaths(t, indexDir), []string{".sourcegraph/ignore", "other/out.go", "scope/ignored.go", "scope/keep.go"}; !slices.Equal(got, want) {
		t.Fatalf("paths=%v, want %v", got, want)
	}
}
