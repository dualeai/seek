package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt/index"
	"golang.org/x/sys/unix"
)

// oneNameOnlyNotice is the notice that a search prints for one name-only file,
// after the blank line that ends the results. Tests compare the output with
// this literal, not with nameOnlyNoticeLine. So a change of the printed words
// fails them.
const oneNameOnlyNotice = "\n\n1 name-only file could also match: binary, too large, not on disk, or unreadable\n"

// nameOnlyNotice runs query in the current folder and returns its output and
// its name-only notice line, or "" when it prints none.
func nameOnlyNotice(t *testing.T, query string) (string, string) {
	t.Helper()
	out, err := seek(t, "--lexical-only", query)
	if err != nil {
		t.Fatalf("run %q: %v\n%s", query, err, out)
	}
	for line := range strings.Lines(out) {
		if strings.Contains(line, "could also match: binary, too large") {
			return out, strings.TrimSpace(line)
		}
	}
	return out, ""
}

// TestBinaryNoticeCountsOnlyFilterPassingFiles puts binary files inside and
// outside a file: filter: the one name-only notice counts the binary files
// that the filter keeps.
func TestBinaryNoticeCountsOnlyFilterPassingFiles(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "sub/hit.txt", "zqnoticemarker\n")
	writeFileAt(t, folder, "sub/one.bin", "zqnoticemarker\x00binary\n")
	writeFileAt(t, folder, "sub/two.bin", "\x00binary\n")
	writeFileAt(t, folder, "other.bin", "zqnoticemarker\x00binary\n")
	setTestUserCache(t)
	t.Chdir(folder)
	out, notice := nameOnlyNotice(t, "zqnoticemarker file:sub/")
	if strings.Count(out, "could also match") != 1 || !strings.HasPrefix(notice, "2 name-only files") {
		t.Fatalf("want one notice for the 2 binary files below sub/:\n%s", out)
	}
	// A query with no content atom matches name-only files by name: no notice.
	if out, notice := nameOnlyNotice(t, "file:\\.bin$"); notice != "" {
		t.Fatalf("a file-name query printed the notice:\n%s", out)
	}
}

// TestNameOnlyNoticeCountsOversizedFiles sets a small content limit: Seek
// indexes a text file above it by name only, and the notice counts it.
func TestNameOnlyNoticeCountsOversizedFiles(t *testing.T) {
	requireTools(t)
	swapForTest(t, &documentContentLimit, 64)
	folder := t.TempDir()
	writeFileAt(t, folder, "small.txt", "zqoversizemarker\n")
	writeFileAt(t, folder, "large.txt", "zqoversizemarker "+strings.Repeat("large text ", 20)+"\n")
	setTestUserCache(t)
	t.Chdir(folder)
	if out, notice := nameOnlyNotice(t, "zqoversizemarker"); !strings.HasPrefix(notice, "1 name-only file ") {
		t.Fatalf("want the oversized file counted:\n%s", out)
	}
}

// TestNameOnlyNoticeCountRows proves the count rules. The notice does not
// count a name-only file that already prints. It does not count Zoekt's own
// too-small files, or a real text file that starts with the marker. It counts
// an unreadable file. A user who ignores file modes, as root does, reads the
// mode-000 file: it then prints as a match, and the notice counts no file.
func TestNameOnlyNoticeCountRows(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "hit.txt", "zqrowmarker\n")
	writeFileAt(t, folder, "zqrowmarker.bin", "\x00binary\n")
	writeFileAt(t, folder, "tiny.txt", "z\n")
	writeFileAt(t, folder, "real.txt", "NOT-INDEXED: written by a person")
	writeFileAt(t, folder, "locked.txt", "zqrowmarker locked\n")
	if err := os.Chmod(filepath.Join(folder, "locked.txt"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(folder, "locked.txt"), 0o644) })
	unreadable := true
	if f, err := os.Open(filepath.Join(folder, "locked.txt")); err == nil {
		_ = f.Close()
		unreadable = false
	}
	setTestUserCache(t)
	t.Chdir(folder)
	// zqrowmarker.bin prints by its name; locked.txt is the one file that
	// could also match.
	out, notice := nameOnlyNotice(t, "zqrowmarker")
	if !strings.Contains(out, "zqrowmarker.bin") {
		t.Fatalf("want zqrowmarker.bin printed by its name:\n%s", out)
	}
	if unreadable && !strings.HasPrefix(notice, "1 name-only file ") {
		t.Fatalf("want the unreadable file counted once and nothing else:\n%s", out)
	}
	if !unreadable && (notice != "" || !strings.Contains(out, "## locked.txt (")) {
		t.Fatalf("want the readable mode-000 file printed and no file counted:\n%s", out)
	}
}

// TestNameOnlyNoticeReadsTheCurrentCopy changes a committed binary file to
// text: the stale committed copy does not count.
func TestNameOnlyNoticeReadsTheCurrentCopy(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "hit.txt", "zqcopymarker\n")
	writeFixtureFile(t, filepath.Join(repo, "data.bin"), "\x00binary\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "binary")
	setTestUserCache(t)
	t.Chdir(repo)
	if _, notice := nameOnlyNotice(t, "zqcopymarker"); !strings.HasPrefix(notice, "1 name-only file ") {
		t.Fatalf("the committed binary file is not counted: %q", notice)
	}
	writeFixtureFile(t, filepath.Join(repo, "data.bin"), "now text\n")
	if out, notice := nameOnlyNotice(t, "zqcopymarker"); notice != "" {
		t.Fatalf("the stale committed copy counted:\n%s", out)
	}
}

// TestPartialCloneMissingBlobCounted counts a blob that a partial clone
// lacks in the name-only notice.
func TestPartialCloneMissingBlobCounted(t *testing.T) {
	requireTools(t)
	clone := newPartialClone(t)
	out, err := seek(t, "--lexical-only", "zqpartialmarker", clone)
	if err != nil || !strings.Contains(out, "\n1 name-only file could also match") {
		t.Fatalf("the missing blob is not counted: err=%v\n%s", err, out)
	}
}

// TestCountedSkipReasonsMatchZoekt indexes one document per counted reason
// and reads the node: Zoekt writes each explanation that countedSkipReasons
// names.
func TestCountedSkipReasonsMatchZoekt(t *testing.T) {
	requireTools(t)
	reasons := []index.SkipReason{index.SkipReasonTooLarge, index.SkipReasonBinary, seekNoBytesReason}
	input := make(chan fileContent, len(reasons))
	for i, reason := range reasons {
		input <- fileContent{name: filepath.Join("d", string(rune('a'+i))), skipReason: reason}
	}
	close(input)
	indexDir := t.TempDir()
	if indexed, err := indexDocuments(t.Context(), indexDir, "reasons", t.TempDir(), input, 1); err != nil || !indexed {
		t.Fatalf("indexed=%t err=%v", indexed, err)
	}
	plan := node{cacheDir: t.TempDir(), indexDir: indexDir}
	files, err := searchPlannedCorpusParsed(t.Context(), plan, nil, notIndexedMarkerQuery(), defaultSearchConfig())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, file := range files {
		for _, line := range file.LineMatches {
			got = append(got, strings.TrimPrefix(string(line.Line), zoektNotIndexedMarker))
		}
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(countedSkipReasons))
	if !slices.Equal(got, want) {
		t.Fatalf("Zoekt explanations %q, want %q", got, want)
	}
}

// TestGitDirtyDatalessFileNotRead marks an untracked file of a Git node as
// cloud-only. The reader does not open it, because the open would download it.
// So its content does not match, and the notice counts it.
func TestGitDirtyDatalessFileNotRead(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "hit.txt", "zqcloudmarker\n")
	writeFixtureFile(t, filepath.Join(repo, "cloud.txt"), "zqcloudmarker cloud\n")
	realDataless := fileIsDataless
	fileIsDataless = func(name string, st *unix.Stat_t) bool {
		return name == "cloud.txt" || realDataless(name, st)
	}
	t.Cleanup(func() { fileIsDataless = realDataless })
	setTestUserCache(t)
	t.Chdir(repo)
	out, notice := nameOnlyNotice(t, "zqcloudmarker")
	if strings.Contains(out, "zqcloudmarker cloud") || !strings.HasPrefix(notice, "1 name-only file ") {
		t.Fatalf("want the cloud-only file unread and counted:\n%s", out)
	}
}
