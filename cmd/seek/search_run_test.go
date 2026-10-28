package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestColdSearchEqualsWarmSearch searches two Git nodes and a folder node
// whose files hold no query word. The model accepts every row, so only
// vectors find the files. The first search waits for the vectors of every
// node, so it prints what the second prints, and the second embeds nothing.
func TestColdSearchEqualsWarmSearch(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	one := f.GitRepo("tree/one", "x.go", "package one\n// first copy\n")
	two := f.GitRepo("tree/two", "x.go", "package two\n// second copy\n")
	writeFixtureFile(t, f.path("tree/loose/notes.go"), "package loose\n// loose notes\n")
	setTestUserCache(t)
	cold, _, err := stubSearch(t, payloadAccept, "alpha beta", f.path("tree"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(one, "x.go"), filepath.Join(two, "x.go"), f.path("tree/loose/notes.go")}
	slices.Sort(want)
	if got := headerPaths(cold); !slices.Equal(got, want) {
		t.Fatalf("the cold search printed %v, want %v:\n%s", got, want, cold)
	}
	warm, scorer, err := stubSearch(t, payloadAccept, "alpha beta", f.path("tree"))
	if err != nil {
		t.Fatal(err)
	}
	if cold != warm || scorer.embeds.Load() != 0 {
		t.Fatalf("cold and warm differ (warm embeds=%d):\ncold:\n%s\nwarm:\n%s", scorer.embeds.Load(), cold, warm)
	}
}

// TestRunScopedSearchUsesWholeRepoGeneration searches a subfolder of a
// repository after a whole-repository search, with a model that accepts every
// row. Only the file in the scope prints, and the scoped search embeds
// nothing new. The scope bounds the rows that the vector search reads. A row
// out of the scope would have no shard copy in the scoped read. The search
// would report that as index damage, with a warning. From the subfolder, a
// search with no path prints the same bytes as a search of ".".
func TestRunScopedSearchUsesWholeRepoGeneration(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/in.go"), "package sub\n// inside the scope\n")
	writeFixtureFile(t, filepath.Join(repo, "other/out.go"), "package other\n// outside the scope\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "two files")
	setTestUserCache(t)
	if _, _, err := stubSearch(t, payloadAccept, "alpha beta", repo); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "sub")
	logs := captureTestLogs(t, slog.LevelWarn)
	output, scorer, err := stubSearch(t, payloadAccept, "alpha beta", sub)
	if got := headerPaths(output); err != nil || !slices.Equal(got, []string{filepath.Join(sub, "in.go")}) {
		t.Fatalf("err=%v, the scoped search printed %v, want only in.go:\n%s", err, got, output)
	}
	if scorer.embeds.Load() != 0 {
		t.Fatalf("the scoped search embedded %d batches, want 0", scorer.embeds.Load())
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("the scoped search warned: %v", records)
	}
	t.Chdir(sub)
	none, noneScorer, noneErr := stubSearch(t, payloadAccept, "alpha beta")
	dot, dotScorer, dotErr := stubSearch(t, payloadAccept, "alpha beta", ".")
	if noneErr != nil || dotErr != nil || none != dot || !slices.Equal(headerPaths(none), []string{"in.go"}) {
		t.Fatalf("no path and . differ or print more than in.go:\nnone=%q err=%v\ndot=%q err=%v", none, noneErr, dot, dotErr)
	}
	if embeds := noneScorer.embeds.Load() + dotScorer.embeds.Load(); embeds != 0 {
		t.Fatalf("the searches from the subfolder embedded %d batches, want 0", embeds)
	}
}

// TestRunMixedFolderGitSearch searches a folder that holds a loose file and a
// nested repository, with a model that accepts every row. The vectors of each
// node find its file.
func TestRunMixedFolderGitSearch(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("tree/loose.go"), "package loose\n// loose file\n")
	repo := f.GitRepo("tree/repo", "nested.go", "package nested\n// nested file\n")
	setTestUserCache(t)
	output, _, err := stubSearch(t, payloadAccept, "alpha beta", f.path("tree"))
	want := []string{f.path("tree/loose.go"), filepath.Join(repo, "nested.go")}
	if got := headerPaths(output); err != nil || !slices.Equal(got, want) {
		t.Fatalf("err=%v printed %v, want %v:\n%s", err, got, want, output)
	}
}

// TestPassThroughAtomMasksSemantic gives a description a repo: atom that no
// node matches, with a model that accepts every row. The Zoekt mask leaves no
// row. So neither lexical nor semantic retrieval returns a file, and the
// model scores nothing.
func TestPassThroughAtomMasksSemantic(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("tree/a.go"), "package a\n// alpha beta\n")
	setTestUserCache(t)
	output, scorer, err := stubSearch(t, payloadAccept, "alpha beta repo:nomatch", f.path("tree"))
	if !errors.Is(err, errNoMatch) || output != "" || scorer.scores.Load() != 0 {
		t.Fatalf("err=%v score calls=%d output:\n%s", err, scorer.scores.Load(), output)
	}
}

// TestJoinedSemanticMergeKeepsSamePathInTwoNodes finds the same relative path
// with other content in two nodes through vectors, with a model that accepts
// every row. Both files print: the identity of a file includes its node.
func TestJoinedSemanticMergeKeepsSamePathInTwoNodes(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	one := f.GitRepo("tree/one", "x.go", "package one\n// first copy\n")
	two := f.GitRepo("tree/two", "x.go", "package two\n// second copy\n")
	setTestUserCache(t)
	output, _, err := stubSearch(t, payloadAccept, "alpha beta", f.path("tree"))
	want := []string{filepath.Join(one, "x.go"), filepath.Join(two, "x.go")}
	if got := headerPaths(output); err != nil || !slices.Equal(got, want) {
		t.Fatalf("err=%v printed %v, want %v:\n%s", err, got, want, output)
	}
}

// TestDuplicateContentCollapses searches a repository and two linked
// worktrees at one commit. The same content and match print once, under the
// smallest path, with the other paths after it. A dirty copy prints as its
// own result.
func TestDuplicateContentCollapses(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/a-main", "dup.go", "package dup\n// alpha beta shared\n")
	wtB := f.path("tree/b-wt")
	wtC := f.path("tree/c-wt")
	gitRunIn(t, repo, "worktree", "add", "-q", "-b", "b", wtB)
	gitRunIn(t, repo, "worktree", "add", "-q", "-b", "c", wtC)
	setTestUserCache(t)
	output, _, err := stubSearch(t, payloadFar, "alpha beta", repo, wtB, wtC)
	if err != nil {
		t.Fatal(err)
	}
	// Headers and copies keep the operands as typed.
	if strings.Count(output, "## ") != 1 || !strings.Contains(output, "## "+filepath.Join(repo, "dup.go")) ||
		!strings.Contains(output, "also: "+filepath.Join(wtB, "dup.go")+", "+filepath.Join(wtC, "dup.go")) {
		t.Fatalf("want one result with two other paths:\n%s", output)
	}
	writeFixtureFile(t, filepath.Join(wtC, "dup.go"), "package dup\n// alpha beta shared and edited\n")
	output, _, err = stubSearch(t, payloadFar, "alpha beta", repo, wtB, wtC)
	if err != nil || strings.Count(output, "## ") != 2 || !strings.Contains(output, "[uncommitted]") {
		t.Fatalf("err=%v, want the dirty copy as its own result:\n%s", err, output)
	}
}

// TestDuplicateDirtyStrictCopiesCollapse puts the same untracked file in two
// Git nodes. The strict hits take their content ID from the overlay, so they
// print as one result, as semantic-only copies do.
func TestDuplicateDirtyStrictCopiesCollapse(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	a := f.GitRepo("tree/a", "seed.go", "package seed\n")
	b := f.GitRepo("tree/b", "seed.go", "package seed\n")
	const content = "package dup\n// alpha beta dirty copy\n"
	writeFixtureFile(t, filepath.Join(a, "copy.go"), content)
	writeFixtureFile(t, filepath.Join(b, "copy.go"), content)
	setTestUserCache(t)
	output, _, err := stubSearch(t, payloadFar, "alpha beta", a, b)
	if err != nil || strings.Count(output, "## ") != 1 || !strings.Contains(output, "also: "+filepath.Join(b, "copy.go")) {
		t.Fatalf("err=%v, want one result for the two dirty copies:\n%s", err, output)
	}
}

// TestDuplicateContentCollapsesSemanticOnly finds two copies of one file only
// through vectors, with a model that accepts every row. They print as one
// result. The path in the model input gives the two copies different vectors.
// So the score, not the name, picks the copy that represents the other, and
// the test reads the two paths as a set.
func TestDuplicateContentCollapsesSemanticOnly(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	const content = "package dup\n// sends each call to its handler\n"
	writeFixtureFile(t, f.path("tree/a/copy.go"), content)
	writeFixtureFile(t, f.path("tree/b/copy.go"), content)
	setTestUserCache(t)
	output, _, err := stubSearch(t, payloadAccept, "alpha beta", f.path("tree/a"), f.path("tree/b"))
	if err != nil || strings.Count(output, "## ") != 1 {
		t.Fatalf("err=%v, want one result for the two copies:\n%s", err, output)
	}
	paths := printedFiles(output)
	if want := []string{f.path("tree/a/copy.go"), f.path("tree/b/copy.go")}; !slices.Equal(paths, want) {
		t.Fatalf("the result and its copies are %v, want %v:\n%s", paths, want, output)
	}
}

// TestMoreThan128AcceptedFilesAllPrint gives 300 files, with a model that
// accepts every row. Every file prints with no -n, and the model scores every
// file: no count cuts the candidates.
func TestMoreThan128AcceptedFilesAllPrint(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	for index := range 300 {
		writeFileAt(t, folder, fmt.Sprintf("f%03d.go", index), fmt.Sprintf("package sample\n// handler %d\n", index))
	}
	setTestUserCache(t)
	output, scorer, err := stubSearch(t, payloadAccept, "alpha beta", folder)
	if err != nil || strings.Count(output, "## ") != 300 {
		t.Fatalf("err=%v results=%d, want 300", err, strings.Count(output, "## "))
	}
	if len(scorer.documents()) != 300 {
		t.Fatalf("the model scored %d documents, want 300", len(scorer.documents()))
	}
}

// TestNameOnlyFilesNeverModelScored matches a binary file by name, with a
// model that accepts every row. The file enters as a strict match and prints
// after the scored files. The model does not embed or score it.
func TestNameOnlyFilesNeverModelScored(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "alpha_beta.bin", "\x00\x01binary\x00")
	writeFileAt(t, folder, "target.go", "package sample\n// alpha beta\n")
	setTestUserCache(t)
	output, scorer, err := stubSearch(t, payloadAccept, "alpha beta", folder)
	if err != nil || !strings.Contains(output, "alpha_beta.bin") {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	if strings.Index(output, "alpha_beta.bin") < strings.Index(output, "target.go") {
		t.Fatalf("the name-only file prints before the scored file:\n%s", output)
	}
	if slices.Contains(scorer.unitPaths(), "alpha_beta.bin") {
		t.Fatal("the model embedded a name-only file")
	}
	if slices.ContainsFunc(scorer.documents(), func(document rerankDocument) bool { return document.Path == "alpha_beta.bin" }) {
		t.Fatal("the model scored a name-only file")
	}
}

// queryShapeFolder writes a folder with one strict target (cmd/parse.go), its
// test file, and one file that holds no query word (target.go). Then it moves
// into the folder. So file filters match the header, and a header of "."
// holds no part of the temporary path, whose test name holds "Test".
func queryShapeFolder(t *testing.T) string {
	t.Helper()
	folder := t.TempDir()
	writeFileAt(t, folder, "cmd/parse.go", "package cmd\n\nfunc ParseRequest() {}\n\n// parse the request line and decode the header\n")
	writeFileAt(t, folder, "cmd/parse_test.go", "package cmd\n\nfunc TestParseRequest() {}\n")
	writeFileAt(t, folder, "target.go", "package sample\n// sends each call to its handler\n")
	t.Chdir(folder)
	return "."
}

// searchShape runs one query on folder with the test model in mode. It
// returns the output, the number of queries that the model prepared, and the
// number of semantic searches that the command logged.
func searchShape(t *testing.T, mode payloadMode, query, folder string) (string, int32, int) {
	t.Helper()
	logs := captureTestLogs(t, slog.LevelDebug)
	output, scorer, err := stubSearch(t, mode, query, folder)
	if err != nil {
		t.Fatalf("%s: %v\n%s", query, err, output)
	}
	searches := 0
	for _, record := range logs.Records() {
		if record.Message == "Searched semantic index" {
			searches++
		}
	}
	return output, scorer.prepares.Load(), searches
}

// TestEveryQueryShapeUsesVectors searches with a model that accepts every
// row. The query shapes are one word, an identifier, a phrase, type:file,
// case:yes, a symbol filter, and file filters alone. Each shape takes the
// vector route, so it prepares one query and searches the vectors once. A
// shape with no filter prints every file. A filter shape prints only the
// files that its filter keeps, because the filter bounds the vectors.
// sym:ParseRequest keeps the two files whose symbols hold ParseRequest.
// file:cmd -file:test keeps cmd/parse.go.
func TestEveryQueryShapeUsesVectors(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := queryShapeFolder(t)
	every := []string{"cmd/parse.go", "cmd/parse_test.go", "target.go"}
	for query, want := range map[string][]string{
		"request":                 every,
		"ParseRequest":            every,
		`"request line"`:          every,
		"type:file parse request": every,
		"case:yes parse request":  every,
		"sym:ParseRequest":        {"cmd/parse.go", "cmd/parse_test.go"},
		"file:cmd -file:test":     {"cmd/parse.go"},
	} {
		t.Run(query, func(t *testing.T) {
			output, prepares, searches := searchShape(t, payloadAccept, query, folder)
			if prepares != 1 || searches != 1 {
				t.Fatalf("prepared queries=%d semantic searches=%d, want 1 and 1:\n%s", prepares, searches, output)
			}
			if got := headerPaths(output); !slices.Equal(got, want) {
				t.Fatalf("printed %v, want %v:\n%s", got, want, output)
			}
		})
	}
}

// TestOrOfDescriptionsUsesVectors searches an or of two descriptions. With a
// model that accepts every row, the model reads both descriptions in one
// query and every file prints. With a model that rejects every row, only the
// strict hits of the or print, as in the --lexical-only search. These are the
// two files that hold "parse" and "request": cmd/parse_test.go holds them in
// TestParseRequest.
func TestOrOfDescriptionsUsesVectors(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := queryShapeFolder(t)
	const query = "parse request or decode header"
	output, prepares, searches := searchShape(t, payloadAccept, query, folder)
	every := []string{"cmd/parse.go", "cmd/parse_test.go", "target.go"}
	if got := headerPaths(output); prepares != 1 || searches != 1 || !slices.Equal(got, every) {
		t.Fatalf("prepared queries=%d semantic searches=%d printed %v, want 1, 1 and %v:\n%s",
			prepares, searches, got, every, output)
	}
	output, _, _ = searchShape(t, payloadReject, query, folder)
	if got, want := headerPaths(output), []string{"cmd/parse.go", "cmd/parse_test.go"}; !slices.Equal(got, want) {
		t.Fatalf("with every row rejected the or printed %v, want the strict hits %v:\n%s", got, want, output)
	}
}

// TestOrOfDescriptionsKeepsItsFilters searches ors in which each branch holds
// a filter, with a model that accepts every row. The vectors add only the
// files that the filter of a branch allows. So a file in another language or
// path never prints, and every file that a filter allows does.
func TestOrOfDescriptionsKeepsItsFilters(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.go", "package a\n// alpha\n")
	writeFileAt(t, folder, "b.py", "# beta\n")
	writeFileAt(t, folder, "target.rs", "// sends each call to its handler\nfn main() {}\n")
	writeFileAt(t, folder, "other/target.go", "package other\n// sends each call to its handler\n")
	t.Chdir(folder)
	for query, want := range map[string][]string{
		"(lang:go alpha) or (lang:python beta)":       {"a.go", "b.py", "other/target.go"},
		`(file:^a\.go$ alpha) or (file:^b\.py$ beta)`: {"a.go", "b.py"},
		// Punctuation filters nothing, as at the top level: every Go file
		// prints, with or without a digit, also in an or of filters.
		`(alpha \d+ lang:go) or (lang:python beta)`: {"a.go", "b.py", "other/target.go"},
		`(\d+ lang:go) or lang:python`:              {"a.go", "b.py", "other/target.go"},
		// A nested type:file reads as its child, as at the top level.
		"lang:go (type:file alpha)": {"a.go", "other/target.go"},
	} {
		output, _, err := stubSearch(t, payloadAccept, query, ".")
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got := slices.Sorted(slices.Values(headerPaths(output))); !slices.Equal(got, want) {
			t.Fatalf("%s printed %v, want %v:\n%s", query, got, want, output)
		}
	}
}

// TestRun_NoPathEqualsDot proves that with no path Seek searches ".": each
// row runs a text search with no path and checks its outcome.
// TestRunScopedSearchUsesWholeRepoGeneration compares no path and "." on the
// vector route.
func TestRun_NoPathEqualsDot(t *testing.T) {
	requireTools(t)
	const marker = "zqnopathmarker"
	search := func(t *testing.T) (string, []slog.Record, error) {
		t.Helper()
		logs := captureTestLogs(t, slog.LevelWarn)
		out, err := seek(t, "--lexical-only", marker)
		return out, logs.Records(), err
	}
	t.Run("outside Git with a nested repo", func(t *testing.T) {
		f := newGraphFixture(t)
		f.graphFile("plain/loose.txt", marker)
		f.graphRepo("plain/nested", "n.txt", marker)
		setTestUserCache(t)
		t.Chdir(f.path("plain"))
		out, _, err := search(t)
		if err != nil || !strings.Contains(out, "loose.txt") || !strings.Contains(out, "n.txt") {
			t.Fatalf("err=%v output:\n%s", err, out)
		}
	})
	t.Run("Git root with a nested repo", func(t *testing.T) {
		f := newGraphFixture(t)
		f.graphRepo("repo", "r.txt", marker)
		f.graphRepo("repo/nested", "n.txt", marker)
		setTestUserCache(t)
		t.Chdir(f.path("repo"))
		out, _, err := search(t)
		if err != nil || !strings.Contains(out, "r.txt") || !strings.Contains(out, "n.txt") {
			t.Fatalf("err=%v output:\n%s", err, out)
		}
	})
	t.Run("Git subfolder runs no rev-parse", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.graphRepo("repo", "top.txt", marker)
		f.graphFile("repo/sub/s.txt", marker)
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-q", "-m", "sub")
		setTestUserCache(t)
		trace := filepath.Join(t.TempDir(), "trace")
		t.Setenv("GIT_TRACE", trace)
		t.Chdir(f.path("repo/sub"))
		out, _, err := search(t)
		if err != nil || !strings.Contains(out, "s.txt") || strings.Contains(out, "top.txt") {
			t.Fatalf("err=%v output:\n%s", err, out)
		}
		// The walk-up probe finds the top with no Git process; the
		// HEAD lookup of the node state is not a root probe.
		if data, _ := os.ReadFile(trace); strings.Contains(string(data), "--show-toplevel") {
			t.Fatalf("the walk-up probe asked Git for the top:\n%s", data)
		}
	})
	t.Run("Git-ignored folder", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.GitRepo("repo", ".gitignore", "build/\n")
		f.graphFile("repo/build/out.txt", marker)
		setTestUserCache(t)
		t.Chdir(filepath.Join(repo, "build"))
		out, _, err := search(t)
		if err != nil || !strings.Contains(out, "out.txt") {
			t.Fatalf("err=%v output:\n%s", err, out)
		}
	})
	t.Run("symlinked cwd into another repo", func(t *testing.T) {
		f := newGraphFixture(t)
		f.graphRepo("other", "o.txt", marker)
		f.graphFile("other/sub/deep.txt", marker)
		gitRunIn(t, f.path("other"), "add", ".")
		gitRunIn(t, f.path("other"), "commit", "-q", "-m", "deep")
		link := f.path("link")
		if err := os.Symlink(f.path("other/sub"), link); err != nil {
			t.Fatal(err)
		}
		setTestUserCache(t)
		t.Chdir(link)
		out, _, err := search(t)
		if err != nil || !strings.Contains(out, "deep.txt") || strings.Contains(out, "o.txt") {
			t.Fatalf("err=%v output:\n%s", err, out)
		}
	})
	t.Run("cwd in .git", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.graphRepo("repo", "r.txt", marker)
		setTestUserCache(t)
		t.Chdir(filepath.Join(repo, ".git"))
		_, _, err := search(t)
		if exitCodeForError(err) != 2 || !strings.Contains(formatCLIError(err), "Git directory") {
			t.Fatalf("err=%v, want one Git-directory error with exit 2", err)
		}
	})
	gitMissing := func(t *testing.T) { t.Setenv("PATH", t.TempDir()) }
	t.Run("Git missing at a Git root", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.graphRepo("repo", "r.txt", marker)
		setTestUserCache(t)
		t.Chdir(repo)
		gitMissing(t)
		out, warns, err := search(t)
		if exitCodeForError(err) != 1 || out != "" || len(warns) != 1 || !logsMention(warns, "Git is required") {
			t.Fatalf("err=%v warnings=%v output:\n%s", err, warns, out)
		}
	})
	t.Run("Git missing in a Git subfolder", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.GitRepo("repo", ".gitignore", ".env\n")
		writeFixtureFile(t, filepath.Join(repo, "sub/.env"), marker+" secret\n")
		f.graphFile("repo/sub/s.txt", marker)
		setTestUserCache(t)
		t.Chdir(filepath.Join(repo, "sub"))
		gitMissing(t)
		out, warns, err := search(t)
		if exitCodeForError(err) != 1 || strings.Contains(out, ".env") || len(warns) != 1 {
			t.Fatalf("err=%v warnings=%v output:\n%s", err, warns, out)
		}
	})
	t.Run("Git missing in a Git-ignored folder", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.GitRepo("repo", ".gitignore", "build/\n")
		f.graphFile("repo/build/out.txt", marker)
		setTestUserCache(t)
		t.Chdir(filepath.Join(repo, "build"))
		gitMissing(t)
		out, warns, err := search(t)
		if exitCodeForError(err) != 1 || strings.Contains(out, "out.txt") || len(warns) != 1 {
			t.Fatalf("err=%v warnings=%v output:\n%s, want no folder fallback", err, warns, out)
		}
	})
	t.Run("Git missing in a plain folder with a nested repo", func(t *testing.T) {
		f := newGraphFixture(t)
		f.graphFile("plain/loose.txt", marker)
		f.graphRepo("plain/nested", "n.txt", marker)
		setTestUserCache(t)
		t.Chdir(f.path("plain"))
		gitMissing(t)
		out, warns, err := search(t)
		if err != nil || !strings.Contains(out, "loose.txt") || strings.Contains(out, "n.txt") || len(warns) != 1 {
			t.Fatalf("err=%v warnings=%v output:\n%s", err, warns, out)
		}
	})
	t.Run("deleted cwd", func(t *testing.T) {
		dir := t.TempDir()
		gone := filepath.Join(dir, "gone")
		if err := os.Mkdir(gone, 0o755); err != nil {
			t.Fatal(err)
		}
		setTestUserCache(t)
		t.Chdir(gone)
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
		_, _, err := search(t)
		message := formatCLIError(err)
		if exitCodeForError(err) != 2 || strings.Count(message, "\n") != 0 || strings.Contains(message, "corpora") {
			t.Fatalf("err=%q, want one operand error with no cache path", message)
		}
	})
}

// TestPartitionIndependence searches the same 60 files as one folder node and
// as three folder nodes, with a model that accepts every row. The strict rank
// reads the global average, so the top-20 sets overlap by at least 0.95. The
// short filler files of tree/a make the averages of the nodes differ.
func TestPartitionIndependence(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	for index := range 60 {
		part := []string{"a", "b", "c"}[index%3]
		words := strings.Repeat("alpha beta ", 1+index%7)
		body := fmt.Sprintf("package sample\n// %s file %d\n", words, index)
		if index%5 == 0 {
			body = fmt.Sprintf("package sample\n// handler %d\n", index)
		}
		writeFixtureFile(t, f.path(fmt.Sprintf("tree/%s/f%02d.go", part, index)), body)
	}
	for index := range 30 {
		writeFixtureFile(t, f.path(fmt.Sprintf("tree/a/fill%02d.txt", index)), "x\n")
	}
	setTestUserCache(t)
	top := func(output, root string) []string {
		var files []string
		for _, result := range printedResults(output) {
			name := result.path
			if rel, err := filepath.Rel(root, name); err == nil && !strings.HasPrefix(rel, "..") {
				name = rel
			}
			files = append(files, filepath.ToSlash(name))
		}
		return files[:min(20, len(files))]
	}
	root := canonicalCorpusPath(f.path("tree"))
	one, _, err := stubSearch(t, payloadAccept, "alpha beta", f.path("tree"))
	if err != nil {
		t.Fatal(err)
	}
	three, _, err := stubSearch(t, payloadAccept, "alpha beta", f.path("tree/a"), f.path("tree/b"), f.path("tree/c"))
	if err != nil {
		t.Fatal(err)
	}
	left, right := top(one, root), top(three, root)
	if len(left) != 20 || len(right) != 20 {
		t.Fatalf("top sets have %d and %d files", len(left), len(right))
	}
	shared := 0
	for _, file := range left {
		if slices.Contains(right, file) {
			shared++
		}
	}
	if overlap := float64(shared) / 20; overlap < 0.95 {
		t.Fatalf("top-20 overlap=%.2f, want >= 0.95:\none node: %v\nthree nodes: %v", overlap, left, right)
	}
}

// TestRelaxedRankIndependentOfPartition searches four files that hold one of
// the two words, as one node and as two, with a model that accepts every row.
// The relaxed lexical rank reads the global average, so the order is the
// same. The short filler files make the averages of the two nodes differ.
func TestRelaxedRankIndependentOfPartition(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	filler := strings.Repeat("// filler text for the file length\n", 8)
	writeFixtureFile(t, f.path("tree/a/a1.go"), "package a\n// alpha first\n")
	writeFixtureFile(t, f.path("tree/a/a2.go"), "package a\n// alpha alpha alpha second\n"+filler)
	writeFixtureFile(t, f.path("tree/b/b1.go"), "package b\n// alpha third\n")
	writeFixtureFile(t, f.path("tree/b/b2.go"), "package b\n// alpha alpha alpha fourth\n"+filler)
	for index := range 30 {
		writeFixtureFile(t, f.path(fmt.Sprintf("tree/a/fill%02d.txt", index)), "x\n")
	}
	setTestUserCache(t)
	t.Chdir(f.path("tree"))
	order := func(operands ...string) []string {
		t.Helper()
		output, _, err := stubSearch(t, payloadAccept, slices.Concat([]string{"alpha beta"}, operands)...)
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for _, result := range printedResults(output) {
			files = append(files, filepath.ToSlash(result.path))
		}
		return files
	}
	one, two := order("."), order("a", "b")
	if len(one) != 4 || !slices.Equal(one, two) {
		t.Fatalf("one node %v, two nodes %v; want the same four files in the same order", one, two)
	}
}

// TestRelaxedRankReadsTheCurrentCopy removes "alpha" from a committed file in
// the work tree, with a model that accepts every row. The relaxed rank of the
// file reads the current copy. So the dirty tree ranks as a commit with the
// same bytes, and both searches print the two files in the same order.
func TestRelaxedRankReadsTheCurrentCopy(t *testing.T) {
	requireTools(t)
	order := func(dirty bool) []string {
		t.Helper()
		f := newGraphFixture(t)
		committed := "package s\n// two\n"
		if dirty {
			committed = "package s\n// alpha two\n"
		}
		repo := f.GitRepo("repo", "s1.go", "package s\n// one\n")
		writeFixtureFile(t, filepath.Join(repo, "s2.go"), committed)
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-q", "-m", "s2")
		writeFixtureFile(t, filepath.Join(repo, "s2.go"), "package s\n// two\n")
		setTestUserCache(t)
		t.Chdir(repo)
		output, _, err := stubSearch(t, payloadAccept, "alpha beta", ".")
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for _, result := range printedResults(output) {
			files = append(files, result.path)
		}
		return files
	}
	dirty, control := order(true), order(false)
	both := []string{"s1.go", "s2.go"}
	if !slices.Equal(dirty, control) || !slices.Equal(slices.Sorted(slices.Values(dirty)), both) {
		t.Fatalf("dirty tree %v, commit with the same bytes %v; want the same order of %v", dirty, control, both)
	}
}

// TestModelInputIndependentOfSearchRoot searches a Git node as the operand,
// then as a child of a parent folder seed. Every row keys a window that names
// the repository-relative path, and the second search embeds nothing. The
// model reads each scored file by its repository-relative path. The
// generation of a Git node has the commit as its key. So the second search
// cannot show a model input that follows the search root; the row keys can.
// The folder rows are in TestFolderModelInputIndependentOfSearchRoot.
func TestModelInputIndependentOfSearchRoot(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("parent/repo", "a.go", "package repo\n// alpha beta handler\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/b.go"), "package sub\n// alpha beta worker\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "sub")
	setTestUserCache(t)
	first, scorer, err := stubSearch(t, payloadFar, "alpha beta", repo)
	if err != nil {
		t.Fatal(err)
	}
	if scorer.embeds.Load() == 0 || !strings.Contains(first, "sub/b.go") {
		t.Fatalf("the first search built no vectors (embeds=%d):\n%s", scorer.embeds.Load(), first)
	}
	requireRowsKeyModelPath(t, resolvedPath(t, repo), "")
	second, scorer, err := stubSearch(t, payloadFar, "alpha beta", f.path("parent"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "b.go") {
		t.Fatalf("the parent search lost the repository:\n%s", second)
	}
	if got := scorer.embeds.Load(); got != 0 {
		t.Fatalf("the parent search embedded %d batches (%v), want 0", got, scorer.unitPaths())
	}
	for _, document := range scorer.documents() {
		if document.modelBase != "" {
			t.Fatalf("the model reads %s below %q, want the repository-relative path", document.Path, document.modelBase)
		}
	}
}

// requireRowsKeyModelPath checks every row of the one generation in the
// cache. Each row must key a window of its file below root. The header of
// that window names the path from the model path base (semanticModelPath).
// That is the absolute path for the base of a folder node, and the
// node-relative path for base "".
func requireRowsKeyModelPath(t *testing.T, root, base string) {
	t.Helper()
	cache, err := seekUserCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	dirs, _ := filepath.Glob(filepath.Join(cache, "corpora", "*", "index", semanticGenerationPrefix+"*"))
	if len(dirs) != 1 {
		t.Fatalf("generations %v, want one", dirs)
	}
	manifest, err := readSemanticManifestUnchecked(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(dirs[0], semanticRowsFile))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := readSemanticRowsFrom(file, manifest.Rows, false)
	_ = file.Close()
	if err != nil || len(rows) == 0 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	for _, row := range rows {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(row.path)))
		if err != nil {
			t.Fatal(err)
		}
		row.text = content[row.start:row.end]
		windows, err := semanticUnitWindows(tokenizer, row, base)
		if err != nil || len(windows) != 1 || makeSemanticRowKey(windows[0].input) != row.key {
			t.Fatalf("the row of %s does not key the window that names its path from base %q (windows=%d err=%v)",
				row.path, base, len(windows), err)
		}
	}
}

// TestRenamedFolderGetsVectorsOfItsNewPath searches a folder, renames it, and
// searches it again. The folder keeps its corpus: the corpus ID is the file
// identity of the root. The model reads its files by absolute path, so the
// second search builds vectors whose windows name the new path.
func TestRenamedFolderGetsVectorsOfItsNewPath(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("old/notes/a.go"), "package notes\n// alpha beta handler\n")
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", f.path("old/notes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.path("old"), f.path("new")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", f.path("new/notes")); err != nil {
		t.Fatal(err)
	}
	newRoot := resolvedPath(t, f.path("new/notes"))
	requireRowsKeyModelPath(t, newRoot, filepath.ToSlash(newRoot))
}

// TestFileRootReusesOnlyFoldersThatHoldIt searches a subfolder and then a
// file next to it. No folder inside the directory of the file can hold the
// file, so the build of the file loads no generation. A file inside a
// searched folder copies the vectors of that folder.
func TestFileRootReusesOnlyFoldersThatHoldIt(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("dir/sub/x.go"), "package sub\n// alpha beta inside\n")
	writeFixtureFile(t, f.path("dir/f.go"), "package dir\n// alpha beta beside\n")
	loaded := func(t *testing.T, operand string) (int64, int32) {
		t.Helper()
		logs := captureTestLogs(t, slog.LevelDebug)
		output, scorer, err := stubSearch(t, payloadFar, "alpha beta", operand)
		if err != nil {
			t.Fatalf("search %s: %v\n%s", operand, err, output)
		}
		var generations int64 = -1
		for _, record := range logs.Records() {
			if record.Message == "Loaded semantic reuse sources" {
				generations, _ = testLogAttrs(record)["generations"].(int64)
			}
		}
		return generations, scorer.embeds.Load()
	}
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", f.path("dir/sub")); err != nil {
		t.Fatal(err)
	}
	if generations, _ := loaded(t, f.path("dir/f.go")); generations != 0 {
		t.Fatalf("the file root loaded %d generations, want none", generations)
	}
	if generations, embeds := loaded(t, f.path("dir/sub/x.go")); generations != 1 || embeds != 0 {
		t.Fatalf("a file inside a searched folder loaded %d generations and embedded %d batches, want 1 and 0", generations, embeds)
	}
}

// TestFolderModelInputIndependentOfSearchRoot searches a folder and then a
// subfolder of it, and in a new cache the subfolder first. The root of a
// folder node is its search root, so its node-relative paths change with the
// root. The model reads a folder file by its absolute path instead. So the
// second search copies the vectors of each file that the first one embedded.
// It embeds only the files that the first one did not hold.
func TestFolderModelInputIndependentOfSearchRoot(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("parent/notes/a/x.go"), "package a\n// alpha beta handler\n")
	writeFixtureFile(t, f.path("parent/notes/b.go"), "package notes\n// alpha beta worker\n")
	writeFixtureFile(t, f.path("parent/top.go"), "package parent\n// alpha beta top\n")
	embedded := func(t *testing.T, operand string) []string {
		t.Helper()
		output, scorer, err := stubSearch(t, payloadFar, "alpha beta", operand)
		if err != nil {
			t.Fatalf("search %s: %v\n%s", operand, err, output)
		}
		return slices.Compact(slices.Sorted(slices.Values(scorer.unitPaths())))
	}
	t.Run("folder then subfolder", func(t *testing.T) {
		setTestUserCache(t)
		if got := embedded(t, f.path("parent")); len(got) != 3 {
			t.Fatalf("the folder search embedded %v, want its 3 files", got)
		}
		parent := resolvedPath(t, f.path("parent"))
		requireRowsKeyModelPath(t, parent, filepath.ToSlash(parent))
		if got := embedded(t, f.path("parent/notes")); len(got) != 0 {
			t.Fatalf("the subfolder search embedded %v, want none", got)
		}
	})
	t.Run("subfolder then folder", func(t *testing.T) {
		setTestUserCache(t)
		if got := embedded(t, f.path("parent/notes")); !slices.Equal(got, []string{"a/x.go", "b.go"}) {
			t.Fatalf("the subfolder search embedded %v, want its 2 files", got)
		}
		if got := embedded(t, f.path("parent")); !slices.Equal(got, []string{"top.go"}) {
			t.Fatalf("the folder search embedded %v, want only top.go", got)
		}
	})
}

// TestDisplayLimitsApplyAfterTheRank searches five strict files, each with a
// context line after its match, with the far model. No row is near the query,
// and every score is 0. The model reads every strict file with its context at
// each -n, and -n prints the first n files of the unlimited order. The equal
// scores leave the lexical order, so the test cannot show that -n cuts after
// the model rank; TestE2EMarkerRealModelFindsTheRetryFunction checks that.
func TestDisplayLimitsApplyAfterTheRank(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	for index, repeat := range []int{4, 3, 2, 1, 1} {
		name := fmt.Sprintf("f%d.go", index+1)
		words := strings.TrimSpace(strings.Repeat("alpha beta ", repeat))
		writeFileAt(t, folder, name, fmt.Sprintf("package s\n// %s\n// context of %s\n", words, name))
	}
	setTestUserCache(t)
	search := func(limit int) (string, []rerankDocument) {
		t.Helper()
		output, scorer, err := stubSearch(t, payloadFar, "-n", strconv.Itoa(limit), "alpha beta", folder)
		if err != nil {
			t.Fatalf("-n %d: %v", limit, err)
		}
		return output, scorer.documents()
	}
	unlimited, _ := search(0)
	full := headerOrder(unlimited)
	if len(full) != 5 {
		t.Fatalf("unlimited order %v, want 5 files", full)
	}
	for limit := range 5 {
		output, documents := search(limit)
		if got := headerOrder(output); limit > 0 && !slices.Equal(got, full[:limit]) {
			t.Fatalf("-n %d prints %v, want %v", limit, got, full[:limit])
		}
		var withContext []string
		for _, document := range documents {
			if strings.Contains(document.Text, "context of "+document.Path) {
				withContext = append(withContext, document.Path)
			}
		}
		if len(documents) != 5 || len(withContext) != 5 {
			t.Fatalf("-n %d: the model read %d documents, and only %v with their context line", limit, len(documents), withContext)
		}
	}
}

// TestDeletedSemanticSourceKeepsOtherNodes deletes a folder file while its
// unit is embedded (a change during a build), with a model that accepts every
// row. The command still answers with the file of the other node. It prints
// the deleted file with the text that the index holds, as a strict hit does.
// The next search reads the tree again and prints no deleted file. The delete
// runs inside the embed call, so the test cannot prove the timing against the
// latency of a real model call.
func TestDeletedSemanticSourceKeepsOtherNodes(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	target := f.path("tree/a/target.go")
	writeFixtureFile(t, target, "package a\n// sends each call to its handler\n")
	writeFixtureFile(t, f.path("tree/a/other.go"), "package a\n// alpha beta\n")
	writeFixtureFile(t, f.path("tree/b/b.go"), "package b\n// alpha beta\n")
	setTestUserCache(t)
	var once sync.Once
	scorer := &hybridTestScorer{mode: payloadAccept, vectorForUnit: func(unit semanticUnit) semanticVector {
		if unit.path == "target.go" {
			once.Do(func() { _ = os.Remove(target) })
		}
		return payloadUnitVector(unit, payloadAccept)
	}}
	search := func() string {
		output, err := seekWith(t, modelFactory(scorer), "alpha beta", f.path("tree/a"), f.path("tree/b"))
		if err != nil {
			t.Fatalf("one deleted file ended the command: %v", err)
		}
		if !strings.Contains(output, "b.go") {
			t.Fatalf("want the other node's file:\n%s", output)
		}
		return output
	}
	block := "## " + target + " (Go) [folder]\n1 package a\n2 // sends each call to its handler\n"
	if output := search(); !strings.Contains(output, block) {
		t.Fatalf("want the deleted file with the text that the index holds:\n%s\nfull output:\n%s", block, output)
	}
	if output := search(); strings.Contains(output, "target.go") {
		t.Fatalf("the next search prints the deleted file:\n%s", output)
	}
}

// TestDuplicateInsideOneNodePrintsRelativeCopies collapses two copies in one
// folder node, with a model that accepts every row. The `also:` line prints
// the copy in the form of the header: node-relative.
func TestDuplicateInsideOneNodePrintsRelativeCopies(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a/dup.go", "package dup\n// alpha beta\n")
	writeFileAt(t, folder, "b/dup.go", "package dup\n// alpha beta\n")
	setTestUserCache(t)
	t.Chdir(folder)
	output, _, err := stubSearch(t, payloadAccept, "alpha beta", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## a/dup.go (Go)\nalso: b/dup.go\n") {
		t.Fatalf("want a/dup.go with the relative copy b/dup.go:\n%s", output)
	}
}
