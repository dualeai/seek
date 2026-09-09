package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

// gitOracleBlobPaths returns the committed blob paths that Git itself sees.
// It is the source oracle for the native indexer regression fixtures.
func gitOracleBlobPaths(t testing.TB, repoDir, commit string) []string {
	t.Helper()
	cmd := exec.Command("git", "--no-pager", "--no-replace-objects", "ls-tree", "-r", "-z", "--full-tree", "--no-abbrev", commit)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-tree oracle: %v", err)
	}

	var paths []string
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, path, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			t.Fatalf("git ls-tree oracle record has no path: %q", record)
		}
		fields := bytes.Fields(header)
		if len(fields) != 3 {
			t.Fatalf("git ls-tree oracle header: %q", header)
		}
		if bytes.Equal(fields[1], []byte("blob")) {
			paths = append(paths, string(path))
		}
	}
	sort.Strings(paths)
	return paths
}

func zoektIndexedPaths(t testing.TB, indexDir string) []string {
	t.Helper()
	searchers, err := loadShardsOptional(indexDir)
	if err != nil {
		t.Fatalf("load Zoekt shards: %v", err)
	}
	defer func() {
		for _, searcher := range searchers {
			searcher.Close()
		}
	}()
	seen := make(map[string]struct{})
	for _, searcher := range searchers {
		result, err := searcher.Search(context.Background(), &query.Const{Value: true}, &zoekt.SearchOptions{})
		if err != nil {
			t.Fatalf("list indexed paths: %v", err)
		}
		for _, match := range result.Files {
			seen[match.FileName] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func zoektRepositoryMetadata(t testing.TB, indexDir string) *zoekt.Repository {
	t.Helper()
	return zoektCommittedRepositoryMetadata(t, indexDir)[0]
}

func zoektCommittedRepositoryMetadata(t testing.TB, indexDir string) []*zoekt.Repository {
	t.Helper()
	shards, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		t.Fatalf("list Zoekt shards: %v", err)
	}
	sort.Strings(shards)
	var committed []*zoekt.Repository
	for _, shard := range shards {
		repositories, _, err := index.ReadMetadataPathAlive(shard)
		if err != nil {
			t.Fatalf("read metadata from %s: %v", shard, err)
		}
		for _, repository := range repositories {
			if repository.Name != repoUncommitted {
				committed = append(committed, repository)
			}
		}
	}
	if len(committed) == 0 {
		t.Fatal("no committed repository metadata")
	}
	return committed
}

func requireCompleteNativeIndex(t *testing.T, repoDir string, want []string) {
	t.Helper()
	indexDir := t.TempDir()
	snapshot := captureHeadForTest(t, repoDir)
	if _, err := indexNativeGitFull(t.Context(), repoDir, indexDir, snapshot, nil, 1); err != nil {
		t.Fatalf("native committed index: %v", err)
	}
	got := zoektIndexedPaths(t, indexDir)
	if !slices.Equal(got, want) {
		t.Fatalf("native index paths=%v, Git paths=%v", got, want)
	}
}

func TestNativeGitFullMatchesGitInventory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files int
		pack  bool
	}{
		{name: "empty"},
		{name: "small", files: 3},
		{name: "packed", files: 40, pack: true},
		{name: "loose", files: 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireTools(t)
			repoDir := initEmptyGitRepo(t)
			for i := range tc.files {
				name := fmt.Sprintf("file-%03d.txt", i)
				if err := os.WriteFile(filepath.Join(repoDir, name), []byte("inventory\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gitRunIn(t, repoDir, "add", ".")
			gitRunIn(t, repoDir, "commit", "--allow-empty", "-m", tc.name)
			if tc.pack {
				gitRunIn(t, repoDir, "repack", "-a", "-d")
			}
			want := gitOracleBlobPaths(t, repoDir, "HEAD")
			requireCompleteNativeIndex(t, repoDir, want)
		})
	}
}

// moveObjectToLoosePrefixedPack stores one object in a valid Git pack whose
// name does not start with "pack-", then removes its loose copy. Git reads all
// *.idx packs. The current go-git filesystem backend reads only pack-* packs.
func moveObjectToLoosePrefixedPack(t *testing.T, repoDir, oid string) {
	t.Helper()
	prefix := filepath.Join(repoDir, ".git", "objects", "pack", "loose-native-fixture")
	cmd := exec.Command("git", "pack-objects", prefix)
	cmd.Dir = repoDir
	cmd.Stdin = strings.NewReader(oid + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("create loose-prefixed pack: %v\n%s", err, out)
	}
	packID := strings.TrimSpace(string(out))
	if packID == "" {
		t.Fatal("git pack-objects returned no pack ID")
	}
	if _, err := os.Stat(prefix + "-" + packID + ".pack"); err != nil {
		t.Fatalf("loose-prefixed pack missing: %v", err)
	}
	loose := filepath.Join(repoDir, ".git", "objects", oid[:2], oid[2:])
	if err := os.Remove(loose); err != nil {
		t.Fatalf("remove loose object %s: %v", oid, err)
	}
	gitRunIn(t, repoDir, "cat-file", "-e", oid)
}

func TestNativeGitIndexer_LoosePrefixedPackIsComplete(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	for name, content := range map[string]string{
		"a.go":          "package a\n// NATIVE_BASE_A\n",
		"hidden/one.go": "package hidden\n// NATIVE_HIDDEN_ONE\n",
		"hidden/two.go": "package hidden\n// NATIVE_HIDDEN_TWO\n",
		"z.go":          "package z\n// NATIVE_BASE_Z\n",
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
	gitRunIn(t, repoDir, "commit", "-m", "loose-prefixed fixture")

	hiddenTree := gitOutputIn(t, repoDir, "rev-parse", "HEAD:hidden")
	moveObjectToLoosePrefixedPack(t, repoDir, hiddenTree)
	want := gitOracleBlobPaths(t, repoDir, "HEAD")
	if len(want) != 4 {
		t.Fatalf("Git oracle paths=%v, want four", want)
	}
	requireCompleteNativeIndex(t, repoDir, want)
}

func TestNativeGitIndexer_AlternatesIsComplete(t *testing.T) {
	requireTools(t)
	source := initEmptyGitRepoNoRemote(t)
	for name, content := range map[string]string{
		"shared/one.go": "package shared\n// NATIVE_ALT_ONE\n",
		"shared/two.go": "package shared\n// NATIVE_ALT_TWO\n",
	} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, source, "add", ".")
	gitRunIn(t, source, "commit", "-m", "alternate source")

	clone := filepath.Join(t.TempDir(), "clone")
	gitRunIn(t, filepath.Dir(clone), "clone", "--shared", source, clone)
	gitRunIn(t, clone, "config", "user.email", "test@test.com")
	gitRunIn(t, clone, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(clone, "a.go"), []byte("package a\n// NATIVE_ALT_LOCAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, clone, "add", "a.go")
	gitRunIn(t, clone, "commit", "-m", "local commit over alternate")

	want := gitOracleBlobPaths(t, clone, "HEAD")
	if len(want) != 3 {
		t.Fatalf("Git oracle paths=%v, want three", want)
	}
	requireCompleteNativeIndex(t, clone, want)
}

func TestNativeGitIndexer_VisibleContract(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)

	files := map[string][]byte{
		".sourcegraph/ignore": []byte("ignored.go\n"),
		"binary.bin":          {0, 1, 2, 0, 3},
		"empty.txt":           nil,
		"ignored.go":          []byte("package ignored\n// NATIVE_IGNORED\n"),
		"regular.go":          []byte("package regular\n// NATIVE_VISIBLE\n"),
		"script.sh":           []byte("#!/bin/sh\necho NATIVE_EXECUTABLE\n"),
	}
	for name, content := range files {
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
	gitRunIn(t, repoDir, "commit", "-m", "golden files")
	gitlinkOID := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	gitRunIn(t, repoDir, "update-index", "--add", "--cacheinfo", "160000,"+gitlinkOID+",submodule")
	gitRunIn(t, repoDir, "commit", "-m", "golden gitlink")
	gitRunIn(t, repoDir, "config", "zoekt.name", "native-golden")
	gitRunIn(t, repoDir, "config", "zoekt.web-url", "https://example.test/native-golden")
	gitRunIn(t, repoDir, "config", "zoekt.repoid", "42")
	gitRunIn(t, repoDir, "config", "zoekt.tenantID", "7")
	gitRunIn(t, repoDir, "config", "zoekt.github-stars", "100")

	indexDir := t.TempDir()
	snapshot := captureHeadForTest(t, repoDir)
	if _, err := indexNativeGitFull(t.Context(), repoDir, indexDir, snapshot, nil, 1); err != nil {
		t.Fatalf("native committed index: %v", err)
	}
	wantPaths := []string{
		".sourcegraph/ignore",
		"binary.bin",
		"empty.txt",
		"link.go",
		"oversize.dat",
		"regular.go",
		"script.sh",
	}
	if got := zoektIndexedPaths(t, indexDir); !slices.Equal(got, wantPaths) {
		t.Fatalf("indexed paths=%v, want %v", got, wantPaths)
	}

	head := gitOutputIn(t, repoDir, "rev-parse", "HEAD")
	commitUnix, err := strconv.ParseInt(gitOutputIn(t, repoDir, "show", "-s", "--format=%ct", "HEAD"), 10, 64)
	if err != nil {
		t.Fatalf("parse commit time: %v", err)
	}
	repository := zoektRepositoryMetadata(t, indexDir)
	if repository.Name != "native-golden" || repository.Source != repoDir {
		t.Fatalf("repository identity name=%q source=%q", repository.Name, repository.Source)
	}
	if repository.URL != "https://example.test/native-golden" {
		t.Fatalf("repository URL=%q", repository.URL)
	}
	if repository.ID != 42 || repository.TenantID != 7 || repository.Rank != 0 {
		t.Fatalf("repository numeric metadata ID=%d TenantID=%d Rank=%d", repository.ID, repository.TenantID, repository.Rank)
	}
	if repository.CommitURLTemplate != "" || repository.FileURLTemplate != "" || repository.LineFragmentTemplate != "" {
		t.Fatalf("provider-neutral repository has URL templates: %+v", repository)
	}
	if len(repository.Branches) != 1 || repository.Branches[0].Name != "HEAD" || repository.Branches[0].Version != head {
		t.Fatalf("repository branches=%v, want HEAD@%s", repository.Branches, head)
	}
	if repository.LatestCommitDate.Unix() != commitUnix {
		t.Fatalf("latest commit=%s, want unix %d", repository.LatestCommitDate, commitUnix)
	}
	if repository.RawConfig["name"] != "native-golden" {
		t.Fatalf("raw config=%v", repository.RawConfig)
	}

	q, err := parseSearchQuery("branch:HEAD NATIVE_VISIBLE")
	if err != nil {
		t.Fatalf("parse branch query: %v", err)
	}
	matches, err := executeParsedSearchScoped(context.Background(), indexDir, q, nil, defaultSearchConfig())
	if err != nil {
		t.Fatalf("branch search: %v", err)
	}
	if len(matches) != 1 || matches[0].FileName != "regular.go" || !slices.Equal(matches[0].Branches, []string{"HEAD"}) {
		t.Fatalf("branch matches=%v", matches)
	}
}
