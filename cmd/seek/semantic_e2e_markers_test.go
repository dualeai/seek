package main

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// End-to-end tests of the node states and the output markers of vector
// results. No fixture file holds all words of a description, so each result
// comes from vector search unless a test doc says otherwise. Except in
// TestE2EMarkerRealModelFindsTheRetryFunction, the description is
// e2eMarkerQuery and the test model accepts every row (payloadAccept), so
// every file with content that the search reads prints. These tests prove
// which nodes, files, tags and lines print; they prove nothing about meaning,
// tau, the retrieval radius or rank. The real-model test proves that the model
// chooses the files and the unit that print.
// The oracle is a literal set from the fixture, or a one-word text search
// with no model on the same operands. Each fixture puts that word in every
// file with content that must print.

// e2eMarkerQuery is a description of two words that no fixture file holds.
const e2eMarkerQuery = "zqalpha zqbeta"

// e2eMarkerText runs a one-word text search with no model on operands. It is
// the oracle of these tests. A search with no result gives "".
func e2eMarkerText(t *testing.T, word string, operands ...string) string {
	t.Helper()
	output, err := seek(t, slices.Concat([]string{"--lexical-only", word}, operands)...)
	if err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("text search %q %v: %v\n%s", word, operands, err, output)
	}
	return output
}

// e2eMarkerSearch runs query on operands with stubSearch in accept mode. It
// also fails the test on an error other than no match, because some tests of
// this file read only the output of a search.
func e2eMarkerSearch(t *testing.T, query string, operands ...string) (string, *hybridTestScorer, error) {
	t.Helper()
	output, scorer, err := stubSearch(t, payloadAccept, slices.Concat([]string{query}, operands)...)
	if err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("description %q %v: %v\n%s", query, operands, err, output)
	}
	return output, scorer, err
}

// e2eMarkerModelPaths returns the sorted, distinct node-relative paths that
// the test model embedded and scored.
func e2eMarkerModelPaths(scorer *hybridTestScorer) (embedded, scored []string) {
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	embedded = slices.Clone(scorer.embeddedPaths)
	slices.Sort(embedded)
	return slices.Compact(embedded), slices.Compact(documentPaths(scorer.documents))
}

// e2eMarkerSameBlocks fails the test unless the text oracle prints at least
// one file, and the description output prints the same headers, tags and
// lines. For each header, it compares the "also:" line and the first region of
// the file (firstRegions).
func e2eMarkerSameBlocks(t *testing.T, label, description, oracle string) {
	t.Helper()
	got, want := firstRegions(description), firstRegions(oracle)
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("%s: the description prints %d files, the text oracle %d:\ndescription:\n%s\noracle:\n%s",
			label, len(got), len(want), description, oracle)
	}
	for header, block := range want {
		if got[header] != block {
			t.Fatalf("%s: %q differs from the text oracle:\ndescription:\n%s\noracle:\n%s", label, header, description, oracle)
		}
	}
}

// TestE2EMarkerUncommittedTagOnDirtyVectorHits finds files of one Git node
// only through vectors, with a model that accepts every row. Modified, staged
// and untracked files print with [uncommitted] and their current bytes, and
// the clean file prints with no tag. A text search agrees. A warm search
// prints the same and embeds nothing.
func TestE2EMarkerUncommittedTagOnDirtyVectorHits(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "clean.go", "package repo\n// zqtagmark clean\n")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package repo\n// zqtagmark zqcommittedcopy\n")
	writeFixtureFile(t, filepath.Join(repo, "restaged.go"), "package repo\n// zqtagmark zqcommittedcopy\n// restaged\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "committed copies")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package repo\n// zqtagmark modified\n")
	writeFixtureFile(t, filepath.Join(repo, "restaged.go"), "package repo\n// zqtagmark restaged\n")
	gitRunIn(t, repo, "add", "restaged.go")
	writeFixtureFile(t, filepath.Join(repo, "staged.go"), "package repo\n// zqtagmark staged\n")
	gitRunIn(t, repo, "add", "staged.go")
	writeFixtureFile(t, filepath.Join(repo, "untracked.go"), "package repo\n// zqtagmark untracked\n")
	setTestUserCache(t)

	want := []string{
		filepath.Join(repo, "clean.go") + " (Go)",
		filepath.Join(repo, "modified.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "restaged.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "staged.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "untracked.go") + " (Go) [uncommitted]",
	}
	cold, _, err := e2eMarkerSearch(t, e2eMarkerQuery, repo)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	if got := headerTexts(cold); !slices.Equal(got, want) {
		t.Fatalf("headers %q, want %q:\n%s", got, want, cold)
	}
	// The dirty files print their current bytes, never the committed copy.
	if strings.Contains(cold, "zqcommittedcopy") {
		t.Fatalf("a vector hit prints the committed copy of a dirty file:\n%s", cold)
	}
	e2eMarkerSameBlocks(t, "cold", cold, e2eMarkerText(t, "zqtagmark", repo))
	warm, scorer, _ := e2eMarkerSearch(t, e2eMarkerQuery, repo)
	if warm != cold || scorer.embedCalls.Load() != 0 {
		t.Fatalf("warm search embeds %d batches or differs:\ncold:\n%s\nwarm:\n%s", scorer.embedCalls.Load(), cold, warm)
	}
}

// TestE2EMarkerDirtyCommittedRowsNeverPrint changes committed files in the
// work tree: an edit, an unstaged and a staged deletion, and a staged and an
// unstaged rename. With a model that accepts every row, only the clean file,
// the edited file and the new names print, each with its current bytes. The
// model never reads a committed row of a dirty path, with a warm base and
// with a cold build.
func TestE2EMarkerDirtyCommittedRowsNeverPrint(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "clean.go", "package repo\n// zqrowmark clean\n")
	for name, content := range map[string]string{
		"edited.go":    "package repo\n// zqrowmark zqoldtext edited\n",
		"deleted.go":   "package repo\n// zqrowmark zqoldtext deleted\n",
		"gitrm.go":     "package repo\n// zqrowmark zqoldtext staged deletion\n",
		"gitmv_old.go": "package repo\n// zqrowmark staged rename\n",
		"mv_old.go":    "package repo\n// zqrowmark unstaged rename\n",
	} {
		writeFixtureFile(t, filepath.Join(repo, name), content)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "targets")
	setTestUserCache(t)
	if output, _, err := e2eMarkerSearch(t, e2eMarkerQuery, repo); err != nil || len(headerTexts(output)) != 6 {
		t.Fatalf("the committed search must find the 6 files: err=%v\n%s", err, output)
	}

	writeFixtureFile(t, filepath.Join(repo, "edited.go"), "package repo\n// zqrowmark edited now\n")
	if err := os.Remove(filepath.Join(repo, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "rm", "-q", "gitrm.go")
	gitRunIn(t, repo, "mv", "gitmv_old.go", "gitmv_new.go")
	if err := os.Rename(filepath.Join(repo, "mv_old.go"), filepath.Join(repo, "mv_new.go")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(repo, "clean.go") + " (Go)",
		filepath.Join(repo, "edited.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "gitmv_new.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "mv_new.go") + " (Go) [uncommitted]",
	}
	if got := headerTexts(e2eMarkerText(t, "zqrowmark", repo)); !slices.Equal(got, want) {
		t.Fatalf("the text oracle gives %q, want %q", got, want)
	}
	if oracle := e2eMarkerText(t, "zqoldtext", repo); oracle != "" {
		t.Fatalf("the text oracle prints the old text:\n%s", oracle)
	}
	check := func(label string) {
		t.Helper()
		output, scorer, err := e2eMarkerSearch(t, e2eMarkerQuery, repo)
		if err != nil {
			t.Fatalf("%s: no vector hit: %v", label, err)
		}
		if got := headerTexts(output); !slices.Equal(got, want) || strings.Contains(output, "zqoldtext") {
			t.Fatalf("%s: headers %q, want %q, and no old text:\n%s", label, got, want, output)
		}
		// No path of a committed row prints, also not as a copy.
		for _, gone := range []string{"deleted.go", "gitrm.go", "gitmv_old.go", "mv_old.go"} {
			if strings.Contains(output, gone) {
				t.Fatalf("%s: the old path %s prints:\n%s", label, gone, output)
			}
		}
		if _, scored := e2eMarkerModelPaths(scorer); !slices.Equal(scored, []string{"clean.go", "edited.go", "gitmv_new.go", "mv_new.go"}) {
			t.Fatalf("%s: the model scored %q, want only the current files", label, scored)
		}
		scorer.mu.Lock()
		defer scorer.mu.Unlock()
		for _, document := range scorer.documents {
			if strings.Contains(document.Text, "zqoldtext") {
				t.Fatalf("%s: the model scored the committed copy of %s:\n%s", label, document.Path, document.Text)
			}
		}
	}
	check("warm base")
	// A new cache: the first build sees the changed tree.
	setTestUserCache(t)
	check("cold build")
}

// TestE2EMarkerNodeTagsAcrossRoots searches a Git node and a folder node
// with a nested repository, with a model that accepts every row. Each vector
// hit gets [git] or [folder] and keeps [uncommitted] before it. One node
// prints no node tag. A text search agrees.
func TestE2EMarkerNodeTagsAcrossRoots(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("roots/repo", "committed.go", "package repo\n// zqrootmark committed\n")
	writeFixtureFile(t, filepath.Join(repo, "untracked.go"), "package repo\n// zqrootmark untracked\n")
	loose := f.path("roots/loose")
	writeFixtureFile(t, filepath.Join(loose, "loose.go"), "package loose\n// zqrootmark loose\n")
	f.GitRepo("roots/loose/nested", "n.go", "package nested\n// zqrootmark nested\n")
	setTestUserCache(t)

	cases := []struct {
		name     string
		operands []string
		want     []string
	}{
		{
			name:     "two roots",
			operands: []string{repo, loose},
			want: []string{
				filepath.Join(loose, "loose.go") + " (Go) [folder]",
				filepath.Join(loose, "nested", "n.go") + " (Go) [git]",
				filepath.Join(repo, "committed.go") + " (Go) [git]",
				filepath.Join(repo, "untracked.go") + " (Go) [uncommitted] [git]",
			},
		},
		{
			name:     "one Git node",
			operands: []string{repo},
			want: []string{
				filepath.Join(repo, "committed.go") + " (Go)",
				filepath.Join(repo, "untracked.go") + " (Go) [uncommitted]",
			},
		},
	}
	for _, tc := range cases {
		output, _, err := e2eMarkerSearch(t, e2eMarkerQuery, tc.operands...)
		if err != nil {
			t.Fatalf("%s: no vector hit: %v", tc.name, err)
		}
		if got := headerTexts(output); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: headers %q, want %q:\n%s", tc.name, got, tc.want, output)
		}
		e2eMarkerSameBlocks(t, tc.name, output, e2eMarkerText(t, "zqrootmark", tc.operands...))
	}
}

// TestE2EMarkerVectorOnlyDuplicatesCollapse finds one content only through
// vectors, with a model that accepts every row. The content is in a repository,
// its linked worktree, an untracked copy and a folder. The copies print as one
// result with an "also:" line, and the model scores one copy. An edited copy
// prints as its own result. The search sorts the copies by score, then by path
// (compareNodeHits): the first copy prints, and the others follow it on the
// also: line. The test model gives each copy its own vector, because the path
// header is in the model input, as with the real model. The test cannot know
// these scores, so it checks the copies as a set.
func TestE2EMarkerVectorOnlyDuplicatesCollapse(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	const content = "package dup\n// zqdupmark shared\n"
	repo := f.GitRepo("dup/main", "dup.go", content)
	worktree := f.path("dup/wt")
	edited := f.path("dup/wt2")
	gitRunIn(t, repo, "worktree", "add", "-q", "-b", "b", worktree)
	gitRunIn(t, repo, "worktree", "add", "-q", "-b", "c", edited)
	writeFixtureFile(t, filepath.Join(repo, "extra", "dup.go"), content)
	folder := f.path("dup/folder")
	writeFixtureFile(t, filepath.Join(folder, "dup.go"), content)
	writeFixtureFile(t, filepath.Join(edited, "dup.go"), "package dup\n// zqdupmark edited copy\n")
	setTestUserCache(t)

	operands := []string{repo, worktree, edited, folder}
	output, scorer, err := e2eMarkerSearch(t, e2eMarkerQuery, operands...)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	// The copies with the same bytes, from the fixture, with the header of
	// each when it represents the others.
	copies := map[string]string{
		filepath.Join(folder, "dup.go"):        filepath.Join(folder, "dup.go") + " (Go) [folder]",
		filepath.Join(repo, "dup.go"):          filepath.Join(repo, "dup.go") + " (Go) [git]",
		filepath.Join(repo, "extra", "dup.go"): filepath.Join(repo, "extra", "dup.go") + " (Go) [uncommitted] [git]",
		filepath.Join(worktree, "dup.go"):      filepath.Join(worktree, "dup.go") + " (Go) [git]",
	}
	editedHeader := filepath.Join(edited, "dup.go") + " (Go) [uncommitted] [git]"
	headers := headerTexts(output)
	if len(headers) != 2 || !slices.Contains(headers, editedHeader) {
		t.Fatalf("headers %q, want the edited copy %q and one other:\n%s", headers, editedHeader, output)
	}
	if strings.Count(output, "also: ") != 1 {
		t.Fatalf("want one also: line, for the copies with the same bytes:\n%s", output)
	}
	// The representative is the result with the also: line.
	var representative printedResult
	for _, result := range printedResults(output) {
		if len(result.also) > 0 {
			representative = result
			break
		}
	}
	paths := append([]string{representative.path}, representative.also...)
	if want := slices.Sorted(maps.Keys(copies)); !slices.Equal(slices.Sorted(slices.Values(paths)), want) ||
		representative.header != copies[representative.path] {
		t.Fatalf("the copy %q with also: %q, want one header of %q with the other copies:\n%s",
			representative.header, representative.also, want, output)
	}
	// One document for each printed result: one of the 4 copies and the
	// edited copy.
	scorer.mu.Lock()
	documents := slices.Clone(scorer.documents)
	scorer.mu.Unlock()
	shared := slices.IndexFunc(documents, func(document rerankDocument) bool {
		return strings.Contains(document.Text, "zqdupmark shared")
	})
	editedCopy := slices.IndexFunc(documents, func(document rerankDocument) bool {
		return strings.Contains(document.Text, "zqdupmark edited copy")
	})
	if len(documents) != 2 || shared < 0 || editedCopy < 0 {
		t.Fatalf("the model scored %d documents %+v, want one copy and the edited copy", len(documents), documents)
	}
}

// TestE2EMarkerNameOnlyFilesNeverReachTheModel puts zqnamemark in files that
// Seek indexes by name only, beside three text files:
//   - binary files, with a NUL byte in the 8,000-byte probe or after it;
//   - files above the content limit;
//   - an unreadable file;
//   - a blob that a partial clone lacks.
//
// The model accepts every row, but it never embeds or scores the name-only
// files. Only the text files print, and the name-only notice counts all 10,
// as the text search does.
func TestE2EMarkerNameOnlyFilesNeverReachTheModel(t *testing.T) {
	requireTools(t)
	swapDocumentContentLimitForTest(t, 12<<10)
	f := newGraphFixture(t)
	big := "zqnamemark " + strings.Repeat("large text ", 1300) + "\n"
	// The NUL byte is after the probe of readContent: only the full check
	// finds that the file is binary.
	late := "zqnamemark late binary\n" + strings.Repeat("late text line\n", 540) + "\x00\n"
	repo := f.GitRepo("names/repo", "target.go", "package repo\n// zqnamemark repo\n")
	writeFixtureFile(t, filepath.Join(repo, "data.bin"), "zqnamemark\x00binary\n")
	writeFixtureFile(t, filepath.Join(repo, "big.txt"), big)
	writeFixtureFile(t, filepath.Join(repo, "late.bin"), late)
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "name-only")
	writeFixtureFile(t, filepath.Join(repo, "newdata.bin"), "zqnamemark\x00new binary\n")
	writeFixtureFile(t, filepath.Join(repo, "newbig.txt"), big)

	folder := f.path("names/loose")
	writeFixtureFile(t, filepath.Join(folder, "target.go"), "package loose\n// zqnamemark folder\n")
	writeFixtureFile(t, filepath.Join(folder, "data.bin"), "zqnamemark\x00folder binary\n")
	writeFixtureFile(t, filepath.Join(folder, "big.txt"), big)
	writeFixtureFile(t, filepath.Join(folder, "flate.bin"), late)
	locked := filepath.Join(folder, "locked.txt")
	writeFixtureFile(t, locked, "zqnamemark locked\n")
	chmodUnreadableOrFatal(t, locked, 0)
	const nameOnlyFiles = 10

	source := f.GitRepo("names/source", "kept.go", "package source\n// zqnamemark kept\n")
	writeFixtureFile(t, filepath.Join(source, "dir", "missing.go"), "package dir\n// zqnamemark missing\n")
	gitRunIn(t, source, "add", ".")
	gitRunIn(t, source, "commit", "-q", "-m", "a file outside the sparse cone")
	gitRunIn(t, source, "config", "uploadpack.allowFilter", "true")
	clone := f.path("names/clone")
	gitRunIn(t, f.root, "clone", "--quiet", "--filter=blob:none", "--sparse", "file://"+source, clone)
	setTestUserCache(t)

	operands := []string{repo, folder, clone}
	notice := fmt.Sprintf("%d name-only files could also match: binary, too large, not on disk, or unreadable\n", nameOnlyFiles)
	output, scorer, err := e2eMarkerSearch(t, e2eMarkerQuery, operands...)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	want := []string{
		filepath.Join(clone, "kept.go") + " (Go) [git]",
		filepath.Join(folder, "target.go") + " (Go) [folder]",
		filepath.Join(repo, "target.go") + " (Go) [git]",
	}
	slices.Sort(want)
	if got := headerTexts(output); !slices.Equal(got, want) || !strings.HasSuffix(output, "\n\n"+notice) {
		t.Fatalf("headers %q, want %q and the notice %q:\n%s", got, want, notice, output)
	}
	oracle := e2eMarkerText(t, "zqnamemark", operands...)
	if !strings.HasSuffix(oracle, "\n\n"+notice) {
		t.Fatalf("the text oracle counts other name-only files:\n%s", oracle)
	}
	nameOnly := []string{"data.bin", "big.txt", "late.bin", "newdata.bin", "newbig.txt", "flate.bin", "locked.txt", "dir/missing.go"}
	embedded, scored := e2eMarkerModelPaths(scorer)
	for _, path := range nameOnly {
		if slices.Contains(embedded, path) || slices.Contains(scored, path) {
			t.Fatalf("the model read the name-only file %s: embedded %q, scored %q", path, embedded, scored)
		}
	}
}

// TestE2EMarkerNestedRepositoriesAreTheirOwnNodes searches a repository with
// a submodule and an untracked nested repository, with a model that accepts
// every row. The vector hits of each print under its own path with its own
// tags, also a dirty submodule file. The parent never embeds a file of a
// child. A text search agrees, and a warm search prints the same. Both
// queries drop .gitmodules, which the model would accept too.
func TestE2EMarkerNestedRepositoriesAreTheirOwnNodes(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	sub := f.GitRepo("nest/src", "s.go", "package sub\n// zqnestmark submodule\n")
	parent := f.GitRepo("nest/parent", "p.go", "package parent\n// zqnestmark parent\n")
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "mysub")
	gitRunIn(t, parent, "commit", "-q", "-m", "add submodule")
	writeFixtureFile(t, filepath.Join(parent, "mysub", "new.go"), "package sub\n// zqnestmark dirty submodule file\n")
	f.GitRepo("nest/parent/inner", "i.go", "package inner\n// zqnestmark inner\n")
	setTestUserCache(t)

	want := []string{
		filepath.Join(parent, "inner", "i.go") + " (Go) [git]",
		filepath.Join(parent, "mysub", "new.go") + " (Go) [uncommitted] [git]",
		filepath.Join(parent, "mysub", "s.go") + " (Go) [git]",
		filepath.Join(parent, "p.go") + " (Go) [git]",
	}
	const filter = " -file:gitmodules"
	cold, scorer, err := e2eMarkerSearch(t, e2eMarkerQuery+filter, parent)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	if got := headerTexts(cold); !slices.Equal(got, want) {
		t.Fatalf("headers %q, want %q:\n%s", got, want, cold)
	}
	e2eMarkerSameBlocks(t, "cold", cold, e2eMarkerText(t, "zqnestmark"+filter, parent))
	embedded, _ := e2eMarkerModelPaths(scorer)
	for _, path := range embedded {
		if strings.HasPrefix(path, "mysub/") || strings.HasPrefix(path, "inner/") {
			t.Fatalf("the parent node embedded the child file %s: %q", path, embedded)
		}
	}
	if warm, _, _ := e2eMarkerSearch(t, e2eMarkerQuery+filter, parent); warm != cold {
		t.Fatalf("cold and warm differ:\ncold:\n%s\nwarm:\n%s", cold, warm)
	}
}

// TestE2EMarkerGitIgnoredFileFollowsTheOperand puts a file in a folder that
// the repository ignores, with a model that accepts every row. The rule is in
// .git/info/exclude, so no .gitignore file prints. A search of the repository
// or of a parent folder never embeds the ignored file. The ignored folder as
// an operand finds it. A text search agrees on each operand set.
// TestE2EMarkerExactFileOperandUsesVectors covers an exact file outside Git.
func TestE2EMarkerGitIgnoredFileFollowsTheOperand(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("ign/repo", "keep.go", "package repo\n// zqignmark kept\n")
	writeFixtureFile(t, filepath.Join(repo, ".git", "info", "exclude"), "build/\n")
	build := filepath.Join(repo, "build")
	writeFixtureFile(t, filepath.Join(build, "out.go"), "package build\n// zqignmark ignored\n")
	setTestUserCache(t)

	keep := filepath.Join(repo, "keep.go") + " (Go)"
	cases := []struct {
		name     string
		operands []string
		want     []string
	}{
		{name: "repository", operands: []string{repo}, want: []string{keep}},
		// The parent folder is a folder node and the repository a Git node.
		{name: "parent folder", operands: []string{f.path("ign")}, want: []string{keep + " [git]"}},
		{name: "ignored folder", operands: []string{build}, want: []string{filepath.Join(build, "out.go") + " (Go)"}},
		{name: "repository and ignored folder", operands: []string{repo, build}, want: []string{
			filepath.Join(build, "out.go") + " (Go) [folder]",
			keep + " [git]",
		}},
	}
	for _, tc := range cases {
		output, scorer, err := e2eMarkerSearch(t, e2eMarkerQuery, tc.operands...)
		if err != nil {
			t.Fatalf("%s: no vector hit: %v", tc.name, err)
		}
		if got := headerTexts(output); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: headers %q, want %q:\n%s", tc.name, got, tc.want, output)
		}
		e2eMarkerSameBlocks(t, tc.name, output, e2eMarkerText(t, "zqignmark", tc.operands...))
		if !strings.Contains(output, "out.go") {
			embedded, scored := e2eMarkerModelPaths(scorer)
			if slices.ContainsFunc(slices.Concat(embedded, scored), func(path string) bool { return strings.HasSuffix(path, "out.go") }) {
				t.Fatalf("%s: the model read the ignored file: embedded %q, scored %q", tc.name, embedded, scored)
			}
		}
	}
}

// TestE2EMarkerStaleIndexAddsNoFilesByMeaning holds the build locks of a Git
// node and a folder node, as a peer builder does, and then adds files. The
// description then gets its candidates from text matches only (README
// "Parallel use"), and the model embeds nothing. The model accepts every row,
// so a search that read the stale vectors would print target.go and
// ftarget.go too. When the locks go, the search finds the new files. This test
// needs text hits: strict.go and fstrict.go hold both description words.
func TestE2EMarkerStaleIndexAddsNoFilesByMeaning(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("stale/repo", "target.go", "package repo\n// committed target\n")
	writeFixtureFile(t, filepath.Join(repo, "strict.go"), "package repo\n// zqalpha zqbeta text match\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "strict")
	folder := f.path("stale/loose")
	writeFixtureFile(t, filepath.Join(folder, "ftarget.go"), "package loose\n// folder target\n")
	writeFixtureFile(t, filepath.Join(folder, "fstrict.go"), "package loose\n// zqalpha zqbeta folder text match\n")
	setTestUserCache(t)
	operands := []string{repo, folder}
	strict := []string{
		filepath.Join(folder, "fstrict.go") + " (Go) [folder]",
		filepath.Join(repo, "strict.go") + " (Go) [git]",
	}
	fresh := append(slices.Clone(strict),
		filepath.Join(folder, "ftarget.go")+" (Go) [folder]",
		filepath.Join(repo, "target.go")+" (Go) [git]",
	)
	slices.Sort(fresh)
	if output, _, _ := e2eMarkerSearch(t, e2eMarkerQuery, operands...); !slices.Equal(headerTexts(output), fresh) {
		t.Fatalf("the first search must find every file, want %q:\n%s", fresh, output)
	}

	plans, err := planCorpora(t.Context(), operands)
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	holders := make([]*os.File, 0, len(plans))
	for _, plan := range plans {
		// tryBuildLock opens its own file description, so the lock conflicts
		// with the build of the search, as the lock of a peer process does.
		holder, ok, err := tryBuildLock(plan.cacheDir)
		if err != nil || !ok {
			t.Fatalf("take the build lock of %s: ok=%t err=%v", plan.cacheDir, ok, err)
		}
		holders = append(holders, holder)
	}
	release := func() {
		for _, holder := range holders {
			releaseLock(holder)
		}
		holders = nil
	}
	t.Cleanup(release)
	writeFixtureFile(t, filepath.Join(repo, "new.go"), "package repo\n// untracked target\n")
	writeFixtureFile(t, filepath.Join(folder, "fnew.go"), "package loose\n// new folder target\n")

	output, scorer, err := e2eMarkerSearch(t, e2eMarkerQuery, operands...)
	if err != nil {
		t.Fatalf("the stale index lost the text matches: %v", err)
	}
	if got := headerTexts(output); !slices.Equal(got, strict) {
		t.Fatalf("stale headers %q, want only the text matches %q:\n%s", got, strict, output)
	}
	embedded, scored := e2eMarkerModelPaths(scorer)
	if len(embedded) != 0 || !slices.Equal(scored, []string{"fstrict.go", "strict.go"}) {
		t.Fatalf("a stale index read vectors: embedded %q, scored %q", embedded, scored)
	}

	release()
	output, _, _ = e2eMarkerSearch(t, e2eMarkerQuery, operands...)
	current := append(slices.Clone(fresh),
		filepath.Join(folder, "fnew.go")+" (Go) [folder]",
		filepath.Join(repo, "new.go")+" (Go) [uncommitted] [git]",
	)
	slices.Sort(current)
	if got := headerTexts(output); !slices.Equal(got, current) {
		t.Fatalf("after the peer, headers %q, want %q:\n%s", got, current, output)
	}
}

// TestE2EMarkerCommitMovesTheBaseAndTheOverlay changes a Git node between
// descriptions, with a model that accepts every row: dirty files first, then
// a commit of them and a deletion. Each step prints the files, tags and lines
// that a text search prints. After the commit, no file keeps [uncommitted],
// the deleted file is gone, and a warm search embeds nothing. Each file has
// one unit and at most 4 lines. So the vector hit and the text match of a file
// both print the full file: the test model chooses no unit by meaning.
func TestE2EMarkerCommitMovesTheBaseAndTheOverlay(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("commit/repo", "stay.go", "package repo\n// zqcommitmark stay\n")
	writeFixtureFile(t, filepath.Join(repo, "gone.go"), "package repo\n// zqcommitmark gone\n")
	writeFixtureFile(t, filepath.Join(repo, "changed.go"), "package repo\n// zqcommitmark changed\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "first")
	setTestUserCache(t)
	step := func(label string, want []string) string {
		t.Helper()
		output, _, err := e2eMarkerSearch(t, e2eMarkerQuery, repo)
		if err != nil {
			t.Fatalf("%s: no vector hit: %v", label, err)
		}
		if got := headerTexts(output); !slices.Equal(got, want) {
			t.Fatalf("%s: headers %q, want %q:\n%s", label, got, want, output)
		}
		e2eMarkerSameBlocks(t, label, output, e2eMarkerText(t, "zqcommitmark", repo))
		return output
	}
	header := func(name, tag string) string { return filepath.Join(repo, name) + " (Go)" + tag }

	step("first commit", []string{header("changed.go", ""), header("gone.go", ""), header("stay.go", "")})

	writeFixtureFile(t, filepath.Join(repo, "new.go"), "package repo\n// zqcommitmark new\n")
	writeFixtureFile(t, filepath.Join(repo, "changed.go"), "package repo\n// zqshift\n// zqcommitmark changed\n")
	step("dirty tree", []string{
		header("changed.go", " [uncommitted]"), header("gone.go", ""), header("new.go", " [uncommitted]"), header("stay.go", ""),
	})

	gitRunIn(t, repo, "rm", "-q", "gone.go")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "second")
	committed := step("second commit", []string{header("changed.go", ""), header("new.go", ""), header("stay.go", "")})
	if warm, scorer, _ := e2eMarkerSearch(t, e2eMarkerQuery, repo); warm != committed || scorer.embedCalls.Load() != 0 {
		t.Fatalf("warm search embeds %d batches or differs:\n%s", scorer.embedCalls.Load(), warm)
	}
}

// TestE2EMarkerExactFileOperandUsesVectors gives an exact file as the one
// operand: a committed and a dirty file of a Git repository, and a file
// outside Git. README "Usage" accepts exact files, so a model that accepts
// every row prints the file, as the text search prints it, and no sibling.
func TestE2EMarkerExactFileOperandUsesVectors(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("exact/repo", "committed.go", "package repo\n// zqexactmark committed\n")
	writeFixtureFile(t, filepath.Join(repo, "dirty.go"), "package repo\n// zqexactmark dirty\n")
	loose := f.path("exact/loose/loose.go")
	writeFixtureFile(t, loose, "package loose\n// zqexactmark loose\n")
	writeFixtureFile(t, f.path("exact/loose/other.go"), "package loose\n// zqexactmark sibling\n")
	setTestUserCache(t)
	for _, tc := range []struct{ name, operand, want string }{
		{name: "committed Git file", operand: filepath.Join(repo, "committed.go"), want: filepath.Join(repo, "committed.go") + " (Go)"},
		{name: "dirty Git file", operand: filepath.Join(repo, "dirty.go"), want: filepath.Join(repo, "dirty.go") + " (Go) [uncommitted]"},
		{name: "file outside Git", operand: loose, want: loose + " (Go)"},
	} {
		output, scorer, err := e2eMarkerSearch(t, e2eMarkerQuery, tc.operand)
		oracle := e2eMarkerText(t, "zqexactmark", tc.operand)
		if got := headerTexts(oracle); !slices.Equal(got, []string{tc.want}) {
			t.Fatalf("%s: the text oracle gives %q, want %q", tc.name, got, tc.want)
		}
		if err != nil {
			embedded, _ := e2eMarkerModelPaths(scorer)
			t.Errorf("%s: no vector hit (%v), the build embedded %q; the text oracle prints:\n%s", tc.name, err, embedded, oracle)
			continue
		}
		e2eMarkerSameBlocks(t, tc.name, output, oracle)
	}
}

// e2eMarkerRetryQuery is the description of
// TestE2EMarkerRealModelFindsTheRetryFunction, from
// TestModelAcceptanceOnRealText. No fixture file holds the word "operations",
// so no file is a strict match, and only the model adds a file.
const e2eMarkerRetryQuery = "retry failed operations"

// e2eMarkerRetryDecoys are the files of acceptanceRealFiles on other topics
// that TestE2EMarkerRealModelFindsTheRetryFunction puts beside the retry
// files.
var e2eMarkerRetryDecoys = []string{"auth/token.go", "cache/lru.go", "db/migrate.go"}

// e2eMarkerBackoffGo is a decoy on a near topic. It has a row in the
// retrieval radius, so the model scores it, but its lift does not pass
// tau_file.
const e2eMarkerBackoffGo = "package util\n\n// backoffDelay returns the wait before the next attempt: it doubles with each attempt.\nfunc backoffDelay(attempt int) time.Duration {\n\treturn time.Millisecond << attempt\n}\n"

// e2eMarkerRetryFunction is the retry function of acceptanceRetryGo, with the
// same doc comment and less code. A unit longer than one model input splits
// into windows. A later window can be the best row, so a hit could start
// inside the function. This function and its header fit one window.
// A symbol unit starts at the line of its symbol, so a doc comment above the
// func line is in the unit of Before. The doc comment is thus the first line
// of the body, where the unit of retryOp holds it. %s is the tag of the file.
const e2eMarkerRetryFunction = "func retryOp(op func() error) error { // zqregionmark %s\n" +
	"\t// retryOp runs op until it succeeds, and doubles the wait after each failed attempt.\n" +
	"\tfor wait := time.Millisecond; op() != nil; wait *= 2 {\n\t\ttime.Sleep(wait)\n\t}\n\treturn nil\n}\n"

// e2eMarkerRetryFile returns a Go file of 4 units: the package line with pad
// comment lines, Before, e2eMarkerRetryFunction, and After. The retry
// function starts at line 7+pad, and its first line holds the word
// zqregionmark and tag for the text oracle.
func e2eMarkerRetryFile(pad int, tag string) string {
	var builder strings.Builder
	builder.WriteString("package net\n")
	for index := range pad {
		fmt.Fprintf(&builder, "// zqpad %s %d\n", tag, index)
	}
	builder.WriteString("\nfunc Before() int {\n\treturn 1\n}\n\n")
	fmt.Fprintf(&builder, e2eMarkerRetryFunction, tag)
	builder.WriteString("\nfunc After() int {\n\treturn 3\n}\n")
	return builder.String()
}

// e2eMarkerRetryBlock returns the lines that a hit on the retry function of
// e2eMarkerRetryFile(pad, tag) prints. They are its first line with 3 lines of
// context on each side, each after its line number.
func e2eMarkerRetryBlock(pad int, tag string) string {
	lines := strings.Split(e2eMarkerRetryFile(pad, tag), "\n")
	first := 7 + pad
	var builder strings.Builder
	for number := first - 3; number <= first+3; number++ {
		fmt.Fprintf(&builder, "%d %s\n", number, lines[number-1])
	}
	return builder.String()
}

// TestE2EMarkerRealModelFindsTheRetryFunction searches with the real model for
// e2eMarkerRetryQuery in a Git node and a folder node. Four files hold
// e2eMarkerRetryFunction as the third of 4 units:
//   - a committed file;
//   - a file that the work tree moves down 5 lines;
//   - an untracked file;
//   - a folder file.
//
// Four files are decoys. The 3 files of e2eMarkerRetryDecoys have no row in
// the retrieval radius. util/backoff.go has one, so the model scores it and
// tau_file rejects it. No file holds the word "operations", so no file is a
// strict match. Only a model that reads meaning can choose the files and the
// unit that print. Four searches:
//
//   - Cold: the model scores exactly the 4 retry files and util/backoff.go.
//     The 4 retry files print with their tags, and no decoy prints. Each
//     prints the first line of the retry function with 3 lines of context on
//     each side. A text search for the zqregionmark word of that line prints
//     the same. A text search for the description finds nothing, so the
//     files come from the model.
//   - Warm, -n 2: the first 2 files of the cold order print, with a footer
//     for the other 2. The model scores the cold set: the limit cuts the
//     output after the rank, not the candidates.
//   - Warm, file:net/: the filtered route prints and scores the cold sets
//     restricted to net/.
//   - After a commit of the moved file, it prints with no [uncommitted] at
//     its new line. The build embeds nothing: it copies the rows of the
//     overlay by key.
//
// Each lift (S - S0) / T below is of the best row of a file, from runs on
// 2026-10-22. The Core ML and the CPU providers agree within 0.0004. A fine
// lift is the same value from the fine vectors of retrieval. The retry files:
// 0.294 to 0.314, at least 0.044 above tau_file. util/backoff.go: 0.154,
// 0.095 under tau_file, with a fine lift 0.020 above the radius. The other
// decoys: fine lifts of at most 0.091, 0.043 under the radius. The test costs
// about 11 model batches. A failure prints the provider and the raw score of
// each scored document.
func TestE2EMarkerRealModelFindsTheRetryFunction(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := initEmptyGitRepoNoRemoteAt(t, f.path("rm/repo"))
	netDir := filepath.Join(repo, "net")
	writeFixtureFile(t, filepath.Join(netDir, "committed.go"), e2eMarkerRetryFile(0, "committed"))
	writeFixtureFile(t, filepath.Join(netDir, "shifted.go"), e2eMarkerRetryFile(0, "shifted"))
	for _, file := range acceptanceRealFiles {
		if slices.Contains(e2eMarkerRetryDecoys, file[0]) {
			writeFixtureFile(t, filepath.Join(repo, filepath.FromSlash(file[0])), file[1])
		}
	}
	writeFixtureFile(t, filepath.Join(repo, "util", "backoff.go"), e2eMarkerBackoffGo)
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "retry")
	writeFixtureFile(t, filepath.Join(netDir, "shifted.go"), e2eMarkerRetryFile(5, "shifted"))
	writeFixtureFile(t, filepath.Join(netDir, "untracked.go"), e2eMarkerRetryFile(1, "untracked"))
	// The folder node is 3 fixed levels below the temporary folder, so the
	// model reads the same path in each run (stableModelRoot).
	folder := f.path("rm/loose/pkg")
	writeFixtureFile(t, filepath.Join(folder, "folder.go"), e2eMarkerRetryFile(2, "folder"))
	setTestUserCache(t)

	search := func(label string, args ...string) (string, *observedModel) {
		t.Helper()
		obs := observeModel(sharedModel(t))
		output, err := seekWith(t, obs.factory(), args...)
		if err != nil {
			_, report := obs.scoredPaths()
			t.Fatalf("%s: %v; %s\n%s", label, err, report, output)
		}
		return output, obs
	}
	checkBlocks := func(label, output string, obs *observedModel, want map[string]string) {
		t.Helper()
		_, report := obs.scoredPaths()
		got := firstRegions(output)
		if len(got) != len(want) {
			t.Fatalf("%s: printed %d files, want %d; %s\n%s", label, len(got), len(want), report, output)
		}
		for header, block := range want {
			if got[header] != block {
				t.Fatalf("%s: %s prints:\n%s\nwant:\n%s\n%s\nfull output:\n%s", label, header, got[header], block, report, output)
			}
		}
	}

	shiftedHeader := filepath.Join(netDir, "shifted.go") + " (Go) [uncommitted] [git]"
	want := map[string]string{
		filepath.Join(netDir, "committed.go") + " (Go) [git]": e2eMarkerRetryBlock(0, "committed"),
		shiftedHeader: e2eMarkerRetryBlock(5, "shifted"),
		filepath.Join(netDir, "untracked.go") + " (Go) [uncommitted] [git]": e2eMarkerRetryBlock(1, "untracked"),
		filepath.Join(folder, "folder.go") + " (Go) [folder]":               e2eMarkerRetryBlock(2, "folder"),
	}
	cold, coldObs := search("cold", "-n", "0", e2eMarkerRetryQuery, repo, folder)
	checkBlocks("cold", cold, coldObs, want)
	e2eMarkerSameBlocks(t, "cold", cold, e2eMarkerText(t, "zqregionmark", repo, folder))
	if output, err := seek(t, "--lexical-only", e2eMarkerRetryQuery, repo, folder); !errors.Is(err, errNoMatch) {
		t.Fatalf("a text search for the description: err=%v, want no match, so that the files come from the model\n%s", err, output)
	}
	coldScored, report := coldObs.scoredPaths()
	wantScored := []string{"folder.go", "net/committed.go", "net/shifted.go", "net/untracked.go", "util/backoff.go"}
	if !slices.Equal(coldScored, wantScored) {
		t.Fatalf("cold: the model scored %q, want %q: the retry files and util/backoff.go are in the retrieval radius, "+
			"the other decoys are not; %s", coldScored, wantScored, report)
	}
	coldOrder := headerOrder(cold)
	coldBlocks := firstRegions(cold)

	limited, limitedObs := search("-n 2", "-n", "2", e2eMarkerRetryQuery, repo, folder)
	checkBlocks("-n 2", limited, limitedObs, map[string]string{
		coldOrder[0]: coldBlocks[coldOrder[0]],
		coldOrder[1]: coldBlocks[coldOrder[1]],
	})
	limitedScored, report := limitedObs.scoredPaths()
	if got := headerOrder(limited); !slices.Equal(got, coldOrder[:2]) ||
		!strings.HasSuffix(limited, "\n\n… 2 more files (showing 2 of 4)\n") {
		t.Fatalf("-n 2: headers %q, want the first 2 of the cold order %q and a footer for 2 more files; %s\n%s",
			got, coldOrder, report, limited)
	}
	if !slices.Equal(limitedScored, coldScored) || limitedObs.embeds.Load() != 0 {
		t.Fatalf("-n 2: the model scored %q and embedded %d batches, want the cold set %q and no batch; %s",
			limitedScored, limitedObs.embeds.Load(), coldScored, report)
	}

	logs := captureTestLogs(t, slog.LevelDebug)
	filtered, filteredObs := search("file:net/", e2eMarkerRetryQuery+" file:net/", repo, folder)
	inNet := make(map[string]string)
	for header, block := range coldBlocks {
		if strings.HasPrefix(header, netDir+string(filepath.Separator)) {
			inNet[header] = block
		}
	}
	checkBlocks("file:net/", filtered, filteredObs, inNet)
	var backends []string
	for _, record := range logs.Records() {
		if record.Message == "Searched semantic index" {
			backends = append(backends, fmt.Sprint(testLogAttrs(record)["backend"]))
		}
	}
	filteredScored, report := filteredObs.scoredPaths()
	inNetScored := slices.DeleteFunc(slices.Clone(coldScored), func(path string) bool { return !strings.HasPrefix(path, "net/") })
	if !slices.Equal(backends, []string{"filtered-exact"}) || !slices.Equal(filteredScored, inNetScored) ||
		filteredObs.embeds.Load() != 0 {
		t.Fatalf("file:net/: backends %q, the model scored %q and embedded %d batches, want filtered-exact, %q and no batch; %s",
			backends, filteredScored, filteredObs.embeds.Load(), inNetScored, report)
	}

	gitRunIn(t, repo, "add", "net/shifted.go")
	gitRunIn(t, repo, "commit", "-q", "-m", "move the retry function down")
	committed := maps.Clone(want)
	delete(committed, shiftedHeader)
	committed[filepath.Join(netDir, "shifted.go")+" (Go) [git]"] = e2eMarkerRetryBlock(5, "shifted")
	after, afterObs := search("after the commit", e2eMarkerRetryQuery, repo, folder)
	checkBlocks("after the commit", after, afterObs, committed)
	if embeds := afterObs.embeds.Load(); embeds != 0 {
		t.Fatalf("after the commit: the build embedded %d batches, want none: the build copies the rows of the overlay by key",
			embeds)
	}
}
