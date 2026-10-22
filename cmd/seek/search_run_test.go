package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// describe runs query through the pipeline with the fake model
// (hybridTestScorer): a unit that holds SEMANTIC_TARGET lies near the query
// vector, and the model scores its text 100; every other unit is orthogonal
// to the query and scores 0.
func describe(t *testing.T, query string, operands ...string) (string, *hybridTestScorer, error) {
	t.Helper()
	scorer := &hybridTestScorer{}
	output, err := runHybridTestSearch(t, query, operands, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil })
	return output, scorer, err
}

// TestColdSearchEqualsWarmSearch searches two Git nodes and a folder node,
// with a target that only semantic retrieval finds: the first search waits
// for every node's vectors, so it prints what the second prints.
func TestColdSearchEqualsWarmSearch(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.GitRepo("tree/one", "a.go", "package one\n// alpha beta\n")
	f.GitRepo("tree/two", "target.go", "package two\n// SEMANTIC_TARGET dispatches the request\n")
	writeFixtureFile(t, f.path("tree/loose/notes.go"), "package loose\n// alpha beta loose\n")
	setTestUserCache(t)
	cold, _, err := describe(t, "alpha beta", f.path("tree"))
	if err != nil {
		t.Fatal(err)
	}
	warm, scorer, err := describe(t, "alpha beta", f.path("tree"))
	if err != nil {
		t.Fatal(err)
	}
	if cold != warm || !strings.Contains(cold, "target.go") || scorer.embedCalls.Load() != 0 {
		t.Fatalf("cold and warm differ or miss the target (warm embeds=%d):\ncold:\n%s\nwarm:\n%s", scorer.embedCalls.Load(), cold, warm)
	}
}

// TestRunScopedSearchUsesWholeRepoGeneration searches a subfolder of a
// repository after a whole-repository search: only in-scope files show, and
// the scoped search embeds nothing new.
func TestRunScopedSearchUsesWholeRepoGeneration(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/in.go"), "package sub\n// SEMANTIC_TARGET inside the scope\n")
	writeFixtureFile(t, filepath.Join(repo, "other/out.go"), "package other\n// SEMANTIC_TARGET outside the scope\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "two targets")
	setTestUserCache(t)
	if _, _, err := describe(t, "alpha beta", repo); err != nil {
		t.Fatal(err)
	}
	output, scorer, err := describe(t, "alpha beta", filepath.Join(repo, "sub"))
	if err != nil || !strings.Contains(output, "in.go") || strings.Contains(output, "out.go") {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	if scorer.embedCalls.Load() != 0 {
		t.Fatalf("the scoped search embedded %d batches, want 0", scorer.embedCalls.Load())
	}
}

// TestRunMixedFolderGitSearch searches a folder that holds a loose file and a
// nested repository: semantic retrieval finds a target in each.
func TestRunMixedFolderGitSearch(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("tree/loose.go"), "package loose\n// SEMANTIC_TARGET loose file\n")
	f.GitRepo("tree/repo", "nested.go", "package nested\n// SEMANTIC_TARGET nested file\n")
	setTestUserCache(t)
	output, _, err := describe(t, "alpha beta", f.path("tree"))
	if err != nil || !strings.Contains(output, "loose.go") || !strings.Contains(output, "nested.go") {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
}

// TestPassThroughAtomMasksSemantic gives a description a repo: atom that no
// node matches: Zoekt's mask leaves no row, so neither lexical nor semantic
// retrieval returns a file.
func TestPassThroughAtomMasksSemantic(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("tree/a.go"), "package a\n// alpha beta SEMANTIC_TARGET\n")
	setTestUserCache(t)
	output, _, err := describe(t, "alpha beta repo:nomatch", f.path("tree"))
	if !errors.Is(err, errNoMatch) || output != "" {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
}

// TestJoinedSemanticMergeKeepsSamePathInTwoNodes finds the same relative path
// with other content in two nodes through vectors: both show (the identity
// of a file includes its node).
func TestJoinedSemanticMergeKeepsSamePathInTwoNodes(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	one := f.GitRepo("tree/one", "x.go", "package one\n// SEMANTIC_TARGET first copy\n")
	two := f.GitRepo("tree/two", "x.go", "package two\n// SEMANTIC_TARGET second copy\n")
	setTestUserCache(t)
	output, _, err := describe(t, "alpha beta", f.path("tree"))
	if err != nil || !strings.Contains(output, filepath.Join(one, "x.go")) || !strings.Contains(output, filepath.Join(two, "x.go")) {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
}

// TestDuplicateContentCollapses searches a repository and two linked
// worktrees at one commit: the same content and match print once, under the
// smallest path, with the other paths after it; a dirty copy prints as its
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
	output, _, err := describe(t, "alpha beta", repo, wtB, wtC)
	if err != nil {
		t.Fatal(err)
	}
	// Headers and copies keep the operands as typed.
	if strings.Count(output, "## ") != 1 || !strings.Contains(output, "## "+filepath.Join(repo, "dup.go")) ||
		!strings.Contains(output, "also: "+filepath.Join(wtB, "dup.go")+", "+filepath.Join(wtC, "dup.go")) {
		t.Fatalf("want one result with two other paths:\n%s", output)
	}
	writeFixtureFile(t, filepath.Join(wtC, "dup.go"), "package dup\n// alpha beta shared and edited\n")
	output, _, err = describe(t, "alpha beta", repo, wtB, wtC)
	if err != nil || strings.Count(output, "## ") != 2 || !strings.Contains(output, "[uncommitted]") {
		t.Fatalf("err=%v, want the dirty copy as its own result:\n%s", err, output)
	}
}

// TestDuplicateDirtyStrictCopiesCollapse puts the same untracked file in two
// Git nodes: the strict hits take their content ID from the overlay, so they
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
	output, _, err := describe(t, "alpha beta", a, b)
	if err != nil || strings.Count(output, "## ") != 1 || !strings.Contains(output, "also: "+filepath.Join(b, "copy.go")) {
		t.Fatalf("err=%v, want one result for the two dirty copies:\n%s", err, output)
	}
}

// TestDuplicateContentCollapsesSemanticOnly finds two copies of one file only
// through vectors: they print as one result.
func TestDuplicateContentCollapsesSemanticOnly(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	const content = "package dup\n// SEMANTIC_TARGET dispatches the request\n"
	writeFixtureFile(t, f.path("tree/a/copy.go"), content)
	writeFixtureFile(t, f.path("tree/b/copy.go"), content)
	setTestUserCache(t)
	output, _, err := describe(t, "alpha beta", f.path("tree/a"), f.path("tree/b"))
	if err != nil || strings.Count(output, "## ") != 1 || !strings.Contains(output, "also: ") {
		t.Fatalf("err=%v, want one result for the two copies:\n%s", err, output)
	}
}

// TestMoreThan128RelevantFilesAllAdmitted gives 300 files that each pass
// tau_file: every one prints with no -n (no candidate count).
func TestMoreThan128RelevantFilesAllAdmitted(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	for index := range 300 {
		writeFileAt(t, folder, fmt.Sprintf("f%03d.go", index), fmt.Sprintf("package sample\n// SEMANTIC_TARGET handler %d\n", index))
	}
	setTestUserCache(t)
	// Each file gets its own unit vector near the query's, so USearch's graph
	// has distinct points (equal vectors give a degenerate graph).
	scorer := &hybridTestScorer{vectorForUnit: func(unit semanticUnit) semanticVector {
		var index int
		_, _ = fmt.Sscanf(unit.path, "f%03d.go", &index)
		vector := semanticVector{0: 1}
		vector[1+index%(semanticEmbeddingDimensions-1)] = 0.05 * float32(1+index/(semanticEmbeddingDimensions-1))
		if err := normalizeSemanticVector(&vector); err != nil {
			t.Fatal(err)
		}
		return vector
	}}
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil })
	if err != nil || strings.Count(output, "## ") != 300 {
		t.Fatalf("err=%v results=%d, want 300", err, strings.Count(output, "## "))
	}
}

// TestNameOnlyFilesNeverModelScored matches a binary file by name: it enters
// as a strict match, prints after the scored files, and the model never reads
// it.
func TestNameOnlyFilesNeverModelScored(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "alpha_beta.bin", "\x00\x01binary\x00")
	writeFileAt(t, folder, "target.go", "package sample\n// SEMANTIC_TARGET alpha beta\n")
	setTestUserCache(t)
	output, scorer, err := describe(t, "alpha beta", folder)
	if err != nil || !strings.Contains(output, "alpha_beta.bin") {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	if strings.Index(output, "alpha_beta.bin") < strings.Index(output, "target.go") {
		t.Fatalf("the name-only file prints before the scored file:\n%s", output)
	}
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	if slices.ContainsFunc(scorer.documents, func(document rerankDocument) bool { return document.Path == "alpha_beta.bin" }) {
		t.Fatal("the model scored a name-only file")
	}
}

// longQueryScorer prepares each query as three 128-token query windows, as
// the model prepares a long query. Each window has one scoring token on the
// direction of the SEMANTIC_TARGET units.
type longQueryScorer struct {
	*hybridTestScorer
}

func (longQueryScorer) PrepareSemanticQuery(_ context.Context, query string) (*semanticQueryEmbedding, error) {
	const windows = 3
	prepared := &semanticQueryEmbedding{
		tokens:     make([]float32, windows*lateOnSequenceLength*semanticEmbeddingDimensions),
		scoreMask:  make([]bool, windows*lateOnSequenceLength),
		modelQuery: query,
	}
	for window := range windows {
		token := window*lateOnSequenceLength + 1
		prepared.tokens[token*semanticEmbeddingDimensions] = 1
		prepared.scoreMask[token] = true
	}
	return prepared, nil
}

// TestLongQueryUsesVectors runs a 303-term query whose query embedding has
// three windows (longQueryScorer). The target holds no query word, so only
// vectors find it, and only with the tokens of every window: its MeanMaxSim
// is 1 when retrieval reads the three windows, and 1/3 (below the 0.4842
// radius) when it reads only the first.
// TestLateOnPrepareSemanticQueryUsesWindows checks the windows that the model
// makes.
func TestLongQueryUsesVectors(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "target.go", "package sample\n// SEMANTIC_TARGET routes each call to its handler\n")
	setTestUserCache(t)
	scorer := longQueryScorer{&hybridTestScorer{}}
	query := "authenticate request token " + strings.TrimSpace(strings.Repeat("identifier ", 300))
	output, err := runHybridTestSearch(t, query, []string{folder}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil })
	if err != nil || !strings.Contains(output, "target.go") {
		t.Fatalf("the long query did not find the target through vectors: err=%v\n%s", err, output)
	}
}

// queryShapeFolder writes a folder with one strict target (cmd/parse.go), its
// test file, and one file that only vectors reach (target.go), and moves into
// it: file filters match the header, and a header of "." holds no part of the
// temporary path (whose test name holds "Test").
func queryShapeFolder(t *testing.T) string {
	t.Helper()
	folder := t.TempDir()
	writeFileAt(t, folder, "cmd/parse.go", "package cmd\n\nfunc ParseRequest() {}\n\n// parse the request line and decode the header\n")
	writeFileAt(t, folder, "cmd/parse_test.go", "package cmd\n\nfunc TestParseRequest() {}\n")
	writeFileAt(t, folder, "target.go", "package sample\n// SEMANTIC_TARGET dispatches each call\n")
	t.Chdir(folder)
	return "."
}

// searchShape runs one query on folder with the fake model and returns its
// output, the model sessions it opened and the semantic searches it logged.
func searchShape(t *testing.T, query, folder string) (string, int32, int) {
	t.Helper()
	logs := captureTestLogs(t, slog.LevelDebug)
	output, scorer, err := describe(t, query, folder)
	if err != nil {
		t.Fatalf("%s: %v\n%s", query, err, output)
	}
	searches := 0
	for _, record := range logs.Records() {
		if record.Message == "Searched semantic index" {
			searches++
		}
	}
	return output, scorer.prepareCalls.Load(), searches
}

// TestEveryQueryShapeUsesVectors searches one word, an identifier, a phrase,
// type:file, case:yes, a symbol filter and file filters alone: each opens one
// model session and searches the vectors once. A shape with no filter also
// finds the file that only vectors reach.
func TestEveryQueryShapeUsesVectors(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := queryShapeFolder(t)
	for query, unfiltered := range map[string]bool{
		"request":                 true,
		"ParseRequest":            true,
		`"request line"`:          true,
		"type:file parse request": true,
		"case:yes parse request":  true,
		"sym:ParseRequest":        false,
		"file:cmd -file:test":     false,
	} {
		t.Run(query, func(t *testing.T) {
			output, sessions, searches := searchShape(t, query, folder)
			if sessions != 1 || searches != 1 {
				t.Fatalf("model sessions=%d semantic searches=%d, want 1 and 1:\n%s", sessions, searches, output)
			}
			if unfiltered && !strings.Contains(output, "target.go") {
				t.Fatalf("no vector-only hit:\n%s", output)
			}
		})
	}
}

// TestOrOfDescriptionsUsesVectors searches an or of two descriptions: the
// model reads both, the vectors find the file that only they reach, and the
// strict or keeps its hit.
func TestOrOfDescriptionsUsesVectors(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := queryShapeFolder(t)
	output, sessions, searches := searchShape(t, "parse request or decode header", folder)
	if sessions != 1 || searches != 1 || !strings.Contains(output, "target.go") || !strings.Contains(output, "parse.go") {
		t.Fatalf("model sessions=%d semantic searches=%d, want 1 and 1, the vector hit and the strict hit:\n%s",
			sessions, searches, output)
	}
}

// TestOrOfDescriptionsKeepsItsFilters searches ors whose every branch holds a
// filter: the vectors add only files that a branch's filter allows, so a
// vector target in another language or path never prints, and the target that
// a filter allows does.
func TestOrOfDescriptionsKeepsItsFilters(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.go", "package a\n// alpha\n")
	writeFileAt(t, folder, "b.py", "# beta\n")
	writeFileAt(t, folder, "target.rs", "// SEMANTIC_TARGET dispatches each call\nfn main() {}\n")
	writeFileAt(t, folder, "other/target.go", "package other\n// SEMANTIC_TARGET dispatches each call\n")
	t.Chdir(folder)
	for query, want := range map[string][]string{
		"(lang:go alpha) or (lang:python beta)":       {"a.go", "b.py", "other/target.go"},
		`(file:^a\.go$ alpha) or (file:^b\.py$ beta)`: {"a.go", "b.py"},
		// Punctuation filters nothing, as at the top level: the Go vector
		// target has no digit and still prints, also in an or of filters.
		`(alpha \d+ lang:go) or (lang:python beta)`: {"b.py", "other/target.go"},
		`(\d+ lang:go) or lang:python`:              {"b.py", "other/target.go"},
		// A nested type:file reads as its child, as at the top level.
		"lang:go (type:file alpha)": {"a.go", "other/target.go"},
	} {
		output, _, err := describe(t, query, ".")
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got := slices.Sorted(slices.Values(headerPaths(output))); !slices.Equal(got, want) {
			t.Fatalf("%s printed %v, want %v:\n%s", query, got, want, output)
		}
	}
}

// TestRun_NoPathEqualsDot proves that with no path Seek searches ".". One row
// compares no path and "." through the model; every other row runs with no
// path and checks its outcome.
func TestRun_NoPathEqualsDot(t *testing.T) {
	requireTools(t)
	const marker = "zqnopathmarker"
	search := func(t *testing.T) (string, []slog.Record, error) {
		t.Helper()
		logs := captureTestLogs(t, slog.LevelWarn)
		out, err := captureStdout(t, func() error { return run(context.Background(), marker, nil) })
		return out, logs.Records(), err
	}
	t.Run("fake model, Git subfolder", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.GitRepo("repo", "README.md", "readme\n")
		writeFixtureFile(t, filepath.Join(repo, "sub/in.go"), "package sub\n// SEMANTIC_TARGET inside\n")
		writeFixtureFile(t, filepath.Join(repo, "other/out.go"), "package other\n// SEMANTIC_TARGET outside\n")
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-q", "-m", "targets")
		setTestUserCache(t)
		t.Chdir(filepath.Join(repo, "sub"))
		none, _, noneErr := describe(t, "alpha beta")
		dot, _, dotErr := describe(t, "alpha beta", ".")
		if none != dot || !errors.Is(noneErr, errNoMatch) && noneErr != nil || (noneErr == nil) != (dotErr == nil) {
			t.Fatalf("no path and . differ:\nnone=%q err=%v\ndot=%q err=%v", none, noneErr, dot, dotErr)
		}
		if !strings.Contains(none, "in.go") || strings.Contains(none, "out.go") {
			t.Fatalf("want the semantic target of the subfolder only:\n%s", none)
		}
	})
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
// as three folder nodes: the top-20 sets overlap by at least 0.95.
func TestPartitionIndependence(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	for index := range 60 {
		part := []string{"a", "b", "c"}[index%3]
		words := strings.Repeat("alpha beta ", 1+index%7)
		body := fmt.Sprintf("package sample\n// %s file %d\n", words, index)
		if index%5 == 0 {
			body = fmt.Sprintf("package sample\n// SEMANTIC_TARGET handler %d\n", index)
		}
		writeFixtureFile(t, f.path(fmt.Sprintf("tree/%s/f%02d.go", part, index)), body)
	}
	setTestUserCache(t)
	top := func(output, root string) []string {
		var files []string
		for line := range strings.Lines(output) {
			header, ok := strings.CutPrefix(line, "## ")
			if !ok {
				continue
			}
			name, _, _ := strings.Cut(header, " (")
			if rel, err := filepath.Rel(root, name); err == nil && !strings.HasPrefix(rel, "..") {
				name = rel
			}
			files = append(files, filepath.ToSlash(name))
		}
		return files[:min(20, len(files))]
	}
	root := canonicalCorpusPath(f.path("tree"))
	one, _, err := describe(t, "alpha beta", f.path("tree"))
	if err != nil {
		t.Fatal(err)
	}
	three, _, err := describe(t, "alpha beta", f.path("tree/a"), f.path("tree/b"), f.path("tree/c"))
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

// TestRelaxedRankIndependentOfPartition searches four semantic files that hold
// one of the two words, as one node and as two: the relaxed lexical rank reads
// the global average, so the order is the same. The short filler files
// make the two nodes' averages differ.
func TestRelaxedRankIndependentOfPartition(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	filler := strings.Repeat("// filler text for the file length\n", 8)
	writeFixtureFile(t, f.path("tree/a/a1.go"), "package a\n// SEMANTIC_TARGET alpha first\n")
	writeFixtureFile(t, f.path("tree/a/a2.go"), "package a\n// SEMANTIC_TARGET alpha alpha alpha second\n"+filler)
	writeFixtureFile(t, f.path("tree/b/b1.go"), "package b\n// SEMANTIC_TARGET alpha third\n")
	writeFixtureFile(t, f.path("tree/b/b2.go"), "package b\n// SEMANTIC_TARGET alpha alpha alpha fourth\n"+filler)
	for index := range 30 {
		writeFixtureFile(t, f.path(fmt.Sprintf("tree/a/fill%02d.txt", index)), "x\n")
	}
	setTestUserCache(t)
	t.Chdir(f.path("tree"))
	order := func(operands ...string) []string {
		t.Helper()
		output, _, err := describe(t, "alpha beta", operands...)
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for line := range strings.Lines(output) {
			if header, ok := strings.CutPrefix(line, "## "); ok {
				name, _, _ := strings.Cut(header, " (")
				files = append(files, filepath.ToSlash(name))
			}
		}
		return files
	}
	one, two := order("."), order("a", "b")
	if len(one) != 4 || !slices.Equal(one, two) {
		t.Fatalf("one node %v, two nodes %v; want the same four files in the same order", one, two)
	}
}

// TestRelaxedRankReadsTheCurrentCopy removes "alpha" from a committed
// semantic file in the work tree: its relaxed rank reads the current copy, so
// the dirty tree ranks as a commit with the same bytes.
func TestRelaxedRankReadsTheCurrentCopy(t *testing.T) {
	requireTools(t)
	order := func(dirty bool) []string {
		t.Helper()
		f := newGraphFixture(t)
		committed := "package s\n// SEMANTIC_TARGET two\n"
		if dirty {
			committed = "package s\n// SEMANTIC_TARGET alpha two\n"
		}
		repo := f.GitRepo("repo", "s1.go", "package s\n// SEMANTIC_TARGET one\n")
		writeFixtureFile(t, filepath.Join(repo, "s2.go"), committed)
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-q", "-m", "s2")
		writeFixtureFile(t, filepath.Join(repo, "s2.go"), "package s\n// SEMANTIC_TARGET two\n")
		setTestUserCache(t)
		t.Chdir(repo)
		output, _, err := describe(t, "alpha beta", ".")
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for line := range strings.Lines(output) {
			if header, ok := strings.CutPrefix(line, "## "); ok {
				name, _, _ := strings.Cut(header, " (")
				files = append(files, name)
			}
		}
		return files
	}
	if dirty, control := order(true), order(false); !slices.Equal(dirty, control) {
		t.Fatalf("dirty tree %v, commit with the same bytes %v", dirty, control)
	}
}

// TestModelInputIndependentOfSearchRoot searches a Git node as the operand,
// then as a child of a parent folder seed: the second search embeds nothing,
// because the model input keeps the repository-relative path. The folder rows
// are in TestFolderModelInputIndependentOfSearchRoot.
func TestModelInputIndependentOfSearchRoot(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("parent/repo", "a.go", "package repo\n// alpha beta handler\n")
	writeFixtureFile(t, filepath.Join(repo, "sub/b.go"), "package sub\n// alpha beta worker\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "sub")
	setTestUserCache(t)
	first, scorer, err := describe(t, "alpha beta", repo)
	if err != nil {
		t.Fatal(err)
	}
	if scorer.embedCalls.Load() == 0 || !strings.Contains(first, "sub/b.go") {
		t.Fatalf("the first search built no vectors (embeds=%d):\n%s", scorer.embedCalls.Load(), first)
	}
	second, scorer, err := describe(t, "alpha beta", f.path("parent"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "b.go") {
		t.Fatalf("the parent search lost the repository:\n%s", second)
	}
	if got := scorer.embedCalls.Load(); got != 0 {
		scorer.mu.Lock()
		defer scorer.mu.Unlock()
		t.Fatalf("the parent search embedded %d batches (%v), want 0", got, scorer.embeddedPaths)
	}
}

// requireRowsKeyAbsolutePath checks that every row of the one generation in
// the cache keys a window whose header names the absolute path of its file
// below root.
func requireRowsKeyAbsolutePath(t *testing.T, root string) {
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
		windows, err := semanticUnitWindows(tokenizer, row, filepath.ToSlash(root))
		if err != nil || len(windows) != 1 || makeSemanticRowKey(windows[0].input) != row.key {
			t.Fatalf("the row of %s does not key the window that names its absolute path (windows=%d err=%v)",
				row.path, len(windows), err)
		}
	}
}

// TestRenamedFolderGetsVectorsOfItsNewPath searches a folder, renames it, and
// searches it again. The folder keeps its corpus (the corpus ID is the root's
// file identity), and the model reads its files by absolute path: the second
// search builds vectors whose windows name the new path.
func TestRenamedFolderGetsVectorsOfItsNewPath(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("old/notes/a.go"), "package notes\n// alpha beta handler\n")
	if _, _, err := describe(t, "alpha beta", f.path("old/notes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.path("old"), f.path("new")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := describe(t, "alpha beta", f.path("new/notes")); err != nil {
		t.Fatal(err)
	}
	requireRowsKeyAbsolutePath(t, canonicalCorpusPath(f.path("new/notes")))
}

// TestFileRootReusesOnlyFoldersThatHoldIt searches a subfolder and then a
// file next to it: no folder inside the file's directory can hold the file,
// so the file's build loads no generation. A file inside a searched folder
// copies that folder's vectors.
func TestFileRootReusesOnlyFoldersThatHoldIt(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("dir/sub/x.go"), "package sub\n// alpha beta inside\n")
	writeFixtureFile(t, f.path("dir/f.go"), "package dir\n// alpha beta beside\n")
	loaded := func(t *testing.T, operand string) (int64, int32) {
		t.Helper()
		logs := captureTestLogs(t, slog.LevelDebug)
		output, scorer, err := describe(t, "alpha beta", operand)
		if err != nil {
			t.Fatalf("search %s: %v\n%s", operand, err, output)
		}
		var generations int64 = -1
		for _, record := range logs.Records() {
			if record.Message == "Loaded semantic reuse sources" {
				generations, _ = testLogAttrs(record)["generations"].(int64)
			}
		}
		return generations, scorer.embedCalls.Load()
	}
	if _, _, err := describe(t, "alpha beta", f.path("dir/sub")); err != nil {
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
// subfolder of it, and in a new cache the subfolder first. A folder node is
// rooted at its search root, so its node-relative paths change with the root;
// the model reads a folder file by its absolute path instead. So the second
// search copies the vectors of every file that the first one embedded, and
// embeds only the files that the first one did not hold.
func TestFolderModelInputIndependentOfSearchRoot(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("parent/notes/a/x.go"), "package a\n// alpha beta handler\n")
	writeFixtureFile(t, f.path("parent/notes/b.go"), "package notes\n// alpha beta worker\n")
	writeFixtureFile(t, f.path("parent/top.go"), "package parent\n// alpha beta top\n")
	embedded := func(t *testing.T, operand string) []string {
		t.Helper()
		output, scorer, err := describe(t, "alpha beta", operand)
		if err != nil {
			t.Fatalf("search %s: %v\n%s", operand, err, output)
		}
		scorer.mu.Lock()
		defer scorer.mu.Unlock()
		return slices.Compact(slices.Sorted(slices.Values(scorer.embeddedPaths)))
	}
	t.Run("folder then subfolder", func(t *testing.T) {
		setTestUserCache(t)
		if got := embedded(t, f.path("parent")); len(got) != 3 {
			t.Fatalf("the folder search embedded %v, want its 3 files", got)
		}
		requireRowsKeyAbsolutePath(t, canonicalCorpusPath(f.path("parent")))
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

// TestDisplayLimitsApplyAfterTheRank searches five strict files whose model
// target sits in a context line of the fourth: the model reads every
// strict file with its context, so each -n prints the first n files of the
// unlimited order, and a promoted file keeps its context lines.
func TestDisplayLimitsApplyAfterTheRank(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "f1.go", "package s\n// alpha beta alpha beta alpha beta alpha beta\n")
	writeFileAt(t, folder, "f2.go", "package s\n// alpha beta alpha beta alpha beta\n")
	writeFileAt(t, folder, "f3.go", "package s\n// alpha beta alpha beta\n")
	writeFileAt(t, folder, "f4.go", "package s\n// alpha beta\n// SEMANTIC_TARGET CONTEXT_AFTER\n")
	writeFileAt(t, folder, "f5.go", "package s\n// alpha beta\n// nothing here\n")
	setTestUserCache(t)
	search := func(limit int) string {
		t.Helper()
		// No unit is near the query: only the strict route reaches the files.
		scorer := &hybridTestScorer{vectorForUnit: func(semanticUnit) semanticVector { return semanticVector{1: 1} }}
		runConfig := searchRunConfig{policy: defaultSearchPolicy(), newModel: func(context.Context) (semanticModel, error) { return scorer, nil }}
		output, err := captureStdout(t, func() error {
			return runSearchCommand(t.Context(), "alpha beta", []string{folder}, limit, 0, defaultSearchConfig(), runConfig)
		})
		if err != nil {
			t.Fatalf("-n %d: %v", limit, err)
		}
		return output
	}
	headers := func(output string) []string {
		var out []string
		for line := range strings.Lines(output) {
			if strings.HasPrefix(line, "## ") {
				out = append(out, strings.TrimSpace(line))
			}
		}
		return out
	}
	full := headers(search(0))
	if len(full) != 5 {
		t.Fatalf("unlimited order %v, want 5 files", full)
	}
	for limit := 1; limit <= 4; limit++ {
		output := search(limit)
		if got := headers(output); !slices.Equal(got, full[:limit]) {
			t.Fatalf("-n %d prints %v, want %v", limit, got, full[:limit])
		}
		if slices.Contains(full[:limit], "## f4.go (Go)") && !strings.Contains(output, "CONTEXT_AFTER") {
			t.Fatalf("-n %d prints f4.go without its context:\n%s", limit, output)
		}
	}
}

// TestDeletedSemanticSourceKeepsOtherNodes deletes a folder file while its
// unit is embedded (a change during a build). The command still answers with
// the other node's file. A vector hit of the deleted file shows the text that
// the index holds, as a strict hit does. The next search reads the tree again
// and prints no deleted file.
func TestDeletedSemanticSourceKeepsOtherNodes(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	target := f.path("tree/a/target.go")
	writeFixtureFile(t, target, "package a\n// SEMANTIC_TARGET dispatches the request\n")
	writeFixtureFile(t, f.path("tree/a/other.go"), "package a\n// alpha beta\n")
	writeFixtureFile(t, f.path("tree/b/b.go"), "package b\n// alpha beta\n")
	setTestUserCache(t)
	var once sync.Once
	scorer := &hybridTestScorer{vectorForUnit: func(unit semanticUnit) semanticVector {
		if bytes.Contains(unit.text, []byte("SEMANTIC_TARGET")) {
			once.Do(func() { _ = os.Remove(target) })
			return semanticVector{0: 1}
		}
		return semanticVector{1: 1}
	}}
	search := func() string {
		output, err := runHybridTestSearch(t, "alpha beta", []string{f.path("tree/a"), f.path("tree/b")}, defaultSearchPolicy(),
			func(context.Context) (semanticModel, error) { return scorer, nil })
		if err != nil {
			t.Fatalf("one deleted file ended the command: %v", err)
		}
		if !strings.Contains(output, "b.go") {
			t.Fatalf("want the other node's file:\n%s", output)
		}
		return output
	}
	if output := search(); strings.Contains(output, "target.go") && !strings.Contains(output, "SEMANTIC_TARGET dispatches the request") {
		t.Fatalf("the deleted file prints with no indexed text:\n%s", output)
	}
	if output := search(); strings.Contains(output, "target.go") {
		t.Fatalf("the next search prints the deleted file:\n%s", output)
	}
}

// TestDuplicateInsideOneNodePrintsRelativeCopies collapses two copies in one
// folder node: the `also:` line prints the copy in the header's form,
// node-relative.
func TestDuplicateInsideOneNodePrintsRelativeCopies(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a/dup.go", "package dup\n// alpha beta SEMANTIC_TARGET\n")
	writeFileAt(t, folder, "b/dup.go", "package dup\n// alpha beta SEMANTIC_TARGET\n")
	setTestUserCache(t)
	t.Chdir(folder)
	output, _, err := describe(t, "alpha beta", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## a/dup.go (Go)\nalso: b/dup.go\n") {
		t.Fatalf("want a/dup.go with the relative copy b/dup.go:\n%s", output)
	}
}
