package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
)

// searchHeaders runs query on operands from the cwd and returns the output
// and its file headers.
func searchHeaders(t *testing.T, query string, operands ...string) (string, []string) {
	t.Helper()
	out, err := captureStdout(t, func() error { return run(context.Background(), query, operands) })
	if err != nil && err != errNoMatch {
		t.Fatalf("run %q %v: %v", query, operands, err)
	}
	var headers []string
	for line := range strings.Lines(out) {
		if header, ok := strings.CutPrefix(strings.TrimSpace(line), "## "); ok {
			name, _, _ := strings.Cut(header, " (")
			headers = append(headers, name)
		}
	}
	return out, headers
}

// TestRunFileFilterIsSearchRootRelative searches a folder with a Git child
// from the folder: the header and file: read the path below the search root.
func TestRunFileFilterIsSearchRootRelative(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("multi/gitA", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "net/backoff.go"), "package net\nfunc DoWithBackoff() {}\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "backoff")
	setTestUserCache(t)
	t.Chdir(f.path("multi"))
	if _, headers := searchHeaders(t, "file:gitA/ DoWithBackoff", "."); len(headers) != 1 || headers[0] != "gitA/net/backoff.go" {
		t.Fatalf("headers=%v, want gitA/net/backoff.go", headers)
	}
	if out, headers := searchHeaders(t, "file:^net/ DoWithBackoff", "."); len(headers) != 0 {
		t.Fatalf("file:^net/ matched below the search root:\n%s", out)
	}
	if _, headers := searchHeaders(t, "-file:^gitA/ DoWithBackoff", "."); len(headers) != 0 {
		t.Fatalf("-file:^gitA/ kept %v", headers)
	}
}

// TestRunSubfolderHeaderIsCwdRelative searches from a subfolder of a Git
// repository: the header is the path below the cwd.
func TestRunSubfolderHeaderIsCwdRelative(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/deeper/d.txt"), "zqheadermarker\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "deeper")
	setTestUserCache(t)
	t.Chdir(filepath.Join(repo, "sub", "deeper"))
	for _, operands := range [][]string{nil, {"."}} {
		if _, headers := searchHeaders(t, "zqheadermarker", operands...); len(headers) != 1 || headers[0] != "d.txt" {
			t.Fatalf("operands %v: headers=%v, want d.txt", operands, headers)
		}
	}
	if _, headers := searchHeaders(t, "file:^d\\.txt$ zqheadermarker"); len(headers) != 1 {
		t.Fatalf("file: does not read the cwd-relative path: %v", headers)
	}
}

// TestSemanticFileFilterIsSearchRootRelative runs a description with a
// search-root file: filter: semantic retrieval reads the same path rule, so
// only gitA files answer, the semantic-only target among them.
func TestSemanticFileFilterIsSearchRootRelative(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	for _, name := range []string{"gitA", "gitB"} {
		repo := f.GitRepo("multi/"+name, "README.md", "readme\n")
		writeFixtureFile(t, filepath.Join(repo, "net/retry.go"), "package net\n// SEMANTIC_TARGET backs off between tries\n")
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-q", "-m", "retry")
	}
	setTestUserCache(t)
	t.Chdir(f.path("multi"))
	output, scorer, err := describe(t, "retry with exponential backoff file:gitA/", ".")
	if err != nil {
		t.Fatal(err)
	}
	if scorer.embedCalls.Load() == 0 || !strings.Contains(output, "## gitA/net/retry.go") || strings.Contains(output, "gitB") {
		t.Fatalf("want the semantic target of gitA only (embeds=%d):\n%s", scorer.embedCalls.Load(), output)
	}
}

// TestTwoOperandsKeepDistinctHeaders searches two folders that hold the same
// name: each header keeps its operand.
func TestTwoOperandsKeepDistinctHeaders(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("work/a/x.go"), "package a\n// zqtwomarker\n")
	writeFixtureFile(t, f.path("work/b/x.go"), "package b\n// zqtwomarker\n")
	setTestUserCache(t)
	t.Chdir(f.path("work"))
	_, headers := searchHeaders(t, "zqtwomarker", "./a", "./b")
	if len(headers) != 2 || headers[0] == headers[1] {
		t.Fatalf("headers=%v, want two distinct headers", headers)
	}
	for _, header := range headers {
		if header != "a/x.go" && header != "b/x.go" {
			t.Fatalf("header %q, want the operand plus the path below it", header)
		}
	}
}

// TestOutsideOperandHeadersAsTyped searches operands outside the cwd: the
// header is the operand as typed plus the path below it.
func TestOutsideOperandHeadersAsTyped(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("other/a.go"), "package other\n// zqoutsidemarker\n")
	writeFixtureFile(t, f.path("work/keep.txt"), "keep\n")
	setTestUserCache(t)
	t.Chdir(f.path("work"))
	if _, headers := searchHeaders(t, "zqoutsidemarker", "../other"); len(headers) != 1 || headers[0] != "../other/a.go" {
		t.Fatalf("headers=%v, want ../other/a.go", headers)
	}
	abs := f.path("other")
	if _, headers := searchHeaders(t, "zqoutsidemarker", abs); len(headers) != 1 || headers[0] != filepath.Join(abs, "a.go") {
		t.Fatalf("headers=%v, want %s", headers, filepath.Join(abs, "a.go"))
	}
}

// TestBareWordKeepsZoektFileNameMeaning searches a word that only the typed
// operand holds: the file-name half of a bare word reads the node path, so it
// matches nothing; an explicit file: reads the header.
func TestBareWordKeepsZoektFileNameMeaning(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("zqoperandword/plain/a.txt"), "plain text\n")
	setTestUserCache(t)
	t.Chdir(f.root)
	if out, headers := searchHeaders(t, "zqoperandword", "zqoperandword/plain"); len(headers) != 0 {
		t.Fatalf("a bare word matched the operand text:\n%s", out)
	}
	if _, headers := searchHeaders(t, "file:zqoperandword plain", "zqoperandword/plain"); len(headers) != 1 {
		t.Fatalf("an explicit file: does not read the header: %v", headers)
	}
}

// TestBareWordKeepsFileNameRankBelowCwd searches from a subfolder: the file
// whose name holds the word ranks first, as at the root.
func TestBareWordKeepsFileNameRankBelowCwd(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/zqwidget.go"), "package sub\nfunc A() {}\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/other.go"), "package sub\n// zqwidget zqwidget zqwidget\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "widget")
	setTestUserCache(t)
	t.Chdir(repo)
	_, root := searchHeaders(t, "zqwidget")
	t.Chdir(filepath.Join(repo, "sub"))
	_, below := searchHeaders(t, "zqwidget")
	if len(root) != 2 || len(below) != 2 || "sub/"+below[0] != root[0] || "sub/"+below[1] != root[1] {
		t.Fatalf("order at the root %v, below it %v", root, below)
	}
}

// TestWrongCaseOperandKeepsRelativeHeaders types a folder operand in another
// case on a case-insensitive file system: the header keeps the typed form and
// file: reads it.
func TestWrongCaseOperandKeepsRelativeHeaders(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("work/sub/a.txt"), "zqcasemarker\n")
	if _, err := os.Stat(f.path("work/SUB")); err != nil {
		t.Skip("case-sensitive file system")
	}
	setTestUserCache(t)
	t.Chdir(f.path("work"))
	if _, headers := searchHeaders(t, "zqcasemarker", "SUB"); len(headers) != 1 || headers[0] != "SUB/a.txt" {
		t.Fatalf("headers=%v, want SUB/a.txt", headers)
	}
	if _, headers := searchHeaders(t, "file:^SUB/ zqcasemarker", "SUB"); len(headers) != 1 {
		t.Fatalf("file: does not read the typed header: %v", headers)
	}
}

// TestFileNameLineShowsTheHeader runs a names-only query from a subfolder:
// the file-name line prints the header, not the node path.
func TestFileNameLineShowsTheHeader(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/deep/zqline.go"), "package deep\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "deep")
	setTestUserCache(t)
	t.Chdir(filepath.Join(repo, "sub"))
	out, _ := searchHeaders(t, "type:file zqline")
	if !strings.Contains(out, "## deep/zqline.go (Go)\n0 deep/zqline.go\n") {
		t.Fatalf("want the header form on the file-name line:\n%s", out)
	}
}

// TestHeadersArePathsForTypedSubpaths types operands as their paths below the
// node root: the headers are the node paths, so file: needs no listing.
func TestHeadersArePathsForTypedSubpaths(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"sub", "pkg"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	node := canonicalCorpusPath(root)
	if operands := newDisplayOperands([]string{"sub", "./pkg"}); !operands.headersArePaths(node) {
		t.Fatal("operands typed as their paths need no rewrite")
	}
	t.Chdir(filepath.Join(root, "sub"))
	if operands := newDisplayOperands([]string{"."}); operands.headersArePaths(node) {
		t.Fatal("a cwd below the root changes the headers")
	}
}

// TestExplicitFileAtomInOrReadsTheHeader pairs an explicit file: atom with a
// content atom of the same pattern: it still reads the header, so a wider
// query never gives fewer files.
func TestExplicitFileAtomInOrReadsTheHeader(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/alpha.go"), "package sub\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/beta.go"), "package sub\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "sub")
	setTestUserCache(t)
	t.Chdir(filepath.Join(repo, "sub"))
	for _, query := range []string{"file:^beta", "file:^beta or content:^beta", "file:^beta or ^beta"} {
		if out, headers := searchHeaders(t, query); !slices.Contains(headers, "beta.go") {
			t.Fatalf("%q: headers=%v, want beta.go:\n%s", query, headers, out)
		}
	}
}

// TestFileNameLineMarksBelowTheCwd rewrites the file-name line of a hit
// whose header drops the start of its node path (the cwd is below the node
// root): a highlight in the shared part moves into the header, one in the
// dropped part goes.
func TestFileNameLineMarksBelowTheCwd(t *testing.T) {
	root := canonicalCorpusPath(t.TempDir())
	results := []corpusSearchResult{{
		displayRoot: root,
		file: zoekt.FileMatch{
			FileName: "pkg/util/foo.go",
			LineMatches: []zoekt.LineMatch{{
				FileName: true,
				Line:     []byte("pkg/util/foo.go"),
				LineFragments: []zoekt.LineFragmentMatch{
					{LineOffset: 0, MatchLength: 3},
					{LineOffset: 4, MatchLength: 4},
				},
			}},
		},
	}}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(root, "pkg"))
	applyHeaders(results, newDisplayOperands([]string{"."}))
	line := results[0].file.LineMatches[0]
	if string(line.Line) != "util/foo.go" || len(line.LineFragments) != 1 ||
		line.LineFragments[0].LineOffset != 0 || line.LineFragments[0].MatchLength != 4 {
		t.Fatalf("line=%q fragments=%+v, want util marked at 0", line.Line, line.LineFragments)
	}
}
